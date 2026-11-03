// Package worker runs the search-and-download pipeline.
//
// Workers are stateless: everything they need lives in the request row, and
// every retry is persisted as next_attempt_at rather than slept in memory.
// That is what lets a restart pick the schedule back up instead of losing it.
//
// One request is one unit of work across every configured tracker: a worker
// searches them all concurrently, ranks the merged candidates and downloads the
// winner. The queue is therefore global, not per tracker.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/berejant/movie-torrent-finder/internal/config"
	"github.com/berejant/movie-torrent-finder/internal/media"
	"github.com/berejant/movie-torrent-finder/internal/storage"
	"github.com/berejant/movie-torrent-finder/internal/tracker"
)

// idlePoll is how often an idle worker re-checks the queue when no submission
// has woken it. Notify() makes new work visible immediately; this is only the
// safety net that picks up due retries.
const idlePoll = 2 * time.Second

// Pool is the set of workers serving every configured tracker.
type Pool struct {
	store    *storage.Store
	trackers []*tracker.Client
	cfg      config.Config
	logger   *slog.Logger

	wake chan struct{}
	wg   sync.WaitGroup
}

// New creates the pool. Call Start to run it.
func New(store *storage.Store, trackers []*tracker.Client, cfg config.Config, logger *slog.Logger) *Pool {
	if cfg.MockbusterFilterEnabled && len(cfg.MockbusterCompanies) == 0 {
		cfg.MockbusterCompanies = media.DefaultMockbusterCompanies
	}
	return &Pool{
		store:    store,
		trackers: trackers,
		cfg:      cfg,
		logger:   logger.With("component", "worker"),
		wake:     make(chan struct{}, 1),
	}
}

// Recover prepares the queue after a restart: tasks that were mid-flight go
// back to QUEUED, and half-written temp files are removed. Tasks are short and
// cheap to repeat, so they restart from the beginning rather than resuming.
func (p *Pool) Recover(ctx context.Context) error {
	requeued, err := p.store.RequeueInFlight(ctx)
	if err != nil {
		return err
	}
	if requeued > 0 {
		p.logger.Info("re-queued in-flight requests after restart", "count", requeued)
	}

	removed, err := cleanTempFiles(p.cfg.TorrentFilesDir)
	if err != nil {
		// A dirty temp file is not worth refusing to start over.
		p.logger.Warn("could not clean temporary files", "err", err)
	} else if removed > 0 {
		p.logger.Info("removed orphaned temporary files", "count", removed)
	}

	return nil
}

// Start launches the configured number of workers and returns immediately.
func (p *Pool) Start(ctx context.Context) {
	for i := range p.cfg.Workers {
		p.wg.Add(1)
		go p.run(ctx, i+1)
	}
	p.logger.Info("worker pool started",
		"workers", p.cfg.Workers,
		"trackers", p.cfg.TrackerNamesList(),
	)
}

// Wait blocks until every worker has stopped.
func (p *Pool) Wait() { p.wg.Wait() }

// Notify tells the pool that new work may be available. It never blocks.
func (p *Pool) Notify() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *Pool) run(ctx context.Context, id int) {
	defer p.wg.Done()

	logger := p.logger.With("worker", id)
	timer := time.NewTimer(idlePoll)
	defer timer.Stop()

	for {
		request, ok, err := p.store.ClaimNext(ctx)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			logger.Error("claim failed", "err", err)
		case ok:
			p.process(ctx, logger, request)
			continue // keep draining while work remains
		}

		// Nothing to do: sleep until woken, until the next poll, or shutdown.
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(idlePoll)

		select {
		case <-ctx.Done():
			return
		case <-p.wake:
		case <-timer.C:
		}
	}
}

// candidate is one tracker result paired with the client that found it, so the
// download goes back to the tracker the winner actually came from.
type candidate struct {
	client  *tracker.Client
	torrent tracker.Torrent
}

// process runs one request end to end.
func (p *Pool) process(ctx context.Context, logger *slog.Logger, request storage.Request) {
	logger = logger.With("request_id", request.ID, "query", request.Query)
	logger.Info("processing request", "attempt", request.AttemptCount)

	// Bound a single attempt so one stuck request cannot hold a worker forever.
	// The trackers are searched in parallel, so the slowest one sets the pace.
	taskCtx, cancel := context.WithTimeout(ctx, 4*p.cfg.MaxTrackerTimeout())
	defer cancel()

	candidates, failures := p.searchAll(taskCtx, request.Query)
	if len(candidates) == 0 {
		p.handleError(ctx, logger, request, combineFailures(failures), "search")
		return
	}

	// A tracker that is down must not withhold a release another tracker
	// already found, so a partial failure is a log line and nothing more.
	for _, failure := range failures {
		if errors.Is(failure.err, tracker.ErrNoResults) {
			continue
		}
		logger.Warn("tracker unavailable, ranking the remaining results",
			"tracker", failure.tracker, "err", failure.err)
	}

	var (
		best              *candidate
		transientInspects []trackerFailure
	)

	for _, cand := range candidates {
		if p.cfg.MockbusterFilterEnabled {
			// 1. Fast check against the release title
			if isMock, reason := media.IsTitleMockbuster(cand.torrent.Title, p.cfg.MockbusterCompanies); isMock {
				logger.Info("skipping candidate: mockbuster detected in title",
					"tracker", cand.client.Name(),
					"title", cand.torrent.Title,
					"reason", reason,
				)
				continue
			}

			// 2. Fetch and inspect topic page metadata
			if cand.torrent.TopicURL != "" {
				details, err := cand.client.FetchTopicDetails(taskCtx, cand.torrent.TopicURL)
				if err != nil {
					logger.Warn("could not fetch topic details for candidate",
						"tracker", cand.client.Name(),
						"topic", cand.torrent.TopicURL,
						"err", err,
					)
					if tracker.IsTransient(err) {
						transientInspects = append(transientInspects, trackerFailure{
							tracker: cand.client.Name(),
							err:     err,
						})
					}
				} else if isMock, reason := media.IsMockbuster(cand.torrent.Title, details.Company, details.Metadata, p.cfg.MockbusterCompanies); isMock {
					logger.Info("skipping candidate: mockbuster detected",
						"tracker", cand.client.Name(),
						"title", cand.torrent.Title,
						"topic", cand.torrent.TopicURL,
						"company", details.Company,
						"reason", reason,
					)
					continue
				}
			}
		}

		chosen := cand
		best = &chosen
		break
	}

	if best == nil {
		if len(transientInspects) > 0 && len(transientInspects) == len(candidates) {
			p.handleError(ctx, logger, request, combineFailures(transientInspects), "inspect")
			return
		}
		logger.Info("all candidates were filtered out as mockbusters")
		p.handleError(ctx, logger, request, tracker.ErrNoResults, "filter")
		return
	}

	result := storage.Result{
		Tracker:  best.client.Name(),
		Title:    best.torrent.Title,
		TopicURL: best.torrent.TopicURL,
		Size:     best.torrent.SizeBytes,
		Quality:  best.torrent.Quality,
		Codec:    best.torrent.Codec,
	}
	if err := p.store.MarkFound(ctx, request.ID, result); err != nil {
		logger.Error("could not record found result", "err", err)
		return
	}
	logger.Info("selected release",
		"tracker", result.Tracker,
		"title", result.Title,
		"quality", result.Quality,
		"codec", result.Codec,
		"candidates", len(candidates),
	)

	path, err := best.client.Download(taskCtx, best.torrent, p.cfg.TorrentFilesDir, request.ID)
	if err != nil {
		p.handleError(ctx, logger, request, err, "download")
		return
	}

	if err := p.store.MarkDownloaded(ctx, request.ID, path); err != nil {
		logger.Error("could not record download", "err", err)
		return
	}
	logger.Info("saved torrent file", "path", path)
}

// trackerFailure is one tracker's reason for contributing no candidates.
type trackerFailure struct {
	tracker string
	err     error
}

// searchAll queries every tracker concurrently and returns the merged
// candidates, best first, alongside the trackers that produced nothing.
//
// Each client has its own rate limiter and cookie session, so there is nothing
// shared to serialise here; the fan-out costs one goroutine per tracker.
func (p *Pool) searchAll(ctx context.Context, query string) ([]candidate, []trackerFailure) {
	type outcome struct {
		torrents []tracker.Torrent
		err      error
	}

	outcomes := make([]outcome, len(p.trackers))

	var wg sync.WaitGroup
	for i, client := range p.trackers {
		wg.Go(func() {
			torrents, err := client.Search(ctx, query)
			outcomes[i] = outcome{torrents: torrents, err: err}
		})
	}
	wg.Wait()

	var (
		candidates []candidate
		failures   []trackerFailure
	)
	for i, client := range p.trackers {
		if outcomes[i].err != nil {
			failures = append(failures, trackerFailure{tracker: client.Name(), err: outcomes[i].err})
			continue
		}
		for _, torrent := range outcomes[i].torrents {
			candidates = append(candidates, candidate{client: client, torrent: torrent})
		}
	}

	// Stable, so equally ranked candidates keep configuration order and the
	// winner does not change between two runs of the same search.
	sort.SliceStable(candidates, func(i, j int) bool {
		return media.Better(rank(candidates[i]), rank(candidates[j]))
	})

	return candidates, failures
}

func rank(c candidate) media.Ranked {
	return media.Ranked{Attributes: c.torrent.Attributes, Priority: c.client.Priority()}
}

// combineFailures reduces the per-tracker errors to the single error the retry
// policy acts on. Silence from every tracker is an answer (NOT_FOUND); one
// tracker failing in a retryable way makes the whole attempt retryable, because
// that tracker may have been holding the better release.
func combineFailures(failures []trackerFailure) error {
	var (
		messages  []string
		transient bool
	)

	for _, failure := range failures {
		if errors.Is(failure.err, tracker.ErrNoResults) {
			continue
		}
		messages = append(messages, failure.tracker+": "+failure.err.Error())
		if tracker.IsTransient(failure.err) {
			transient = true
		}
	}

	if len(messages) == 0 {
		return tracker.ErrNoResults
	}

	err := fmt.Errorf("every tracker failed: %s", strings.Join(messages, "; "))
	if transient {
		return tracker.Transient(err)
	}
	return err
}

// handleError maps a pipeline failure onto the right terminal or retry state.
func (p *Pool) handleError(
	ctx context.Context,
	logger *slog.Logger,
	request storage.Request,
	err error,
	stage string,
) {
	// A shutdown is not a task failure. Leaving the request in SEARCHING means
	// Recover picks it up on the next start.
	if ctx.Err() != nil {
		logger.Info("shutting down mid-request, leaving it for restart", "stage", stage)
		return
	}

	switch {
	case errors.Is(err, tracker.ErrNoResults):
		logger.Info("no matching release", "stage", stage)
		if err := p.store.MarkNotFound(ctx, request.ID, "tracker returned no usable match"); err != nil {
			logger.Error("could not record not-found", "err", err)
		}
		return

	case !tracker.IsTransient(err):
		logger.Warn("permanent failure", "stage", stage, "err", err)
		if err := p.store.MarkFailed(ctx, request.ID, storage.TrimError(err)); err != nil {
			logger.Error("could not record failure", "err", err)
		}
		return
	}

	if request.AttemptCount >= p.cfg.Retry.MaxAttempts {
		logger.Warn("retries exhausted", "stage", stage, "attempts", request.AttemptCount, "err", err)
		if err := p.store.MarkFailed(ctx, request.ID, storage.TrimError(err)); err != nil {
			logger.Error("could not record failure", "err", err)
		}
		return
	}

	// AttemptCount was incremented when the request was claimed, so the next
	// attempt's delay is the one for the attempt we are about to schedule.
	delay := storage.Backoff(request.AttemptCount, p.cfg.Retry.BaseSeconds, p.cfg.Retry.MaxBackoffSeconds)
	next := time.Now().UTC().Add(delay)

	logger.Info("scheduling retry",
		"stage", stage,
		"attempt", request.AttemptCount,
		"delay", delay.Round(time.Second),
		"err", err,
	)
	if err := p.store.ScheduleRetry(ctx, request.ID, next, storage.TrimError(err)); err != nil {
		logger.Error("could not schedule retry", "err", err)
	}
}

// cleanTempFiles removes leftovers from an interrupted download. Nothing else
// is running at startup, so every match is stale by definition.
func cleanTempFiles(dir string) (int, error) {
	matches, err := filepath.Glob(filepath.Join(dir, ".torrent-*.tmp"))
	if err != nil {
		return 0, err
	}

	removed := 0
	for _, path := range matches {
		if err := os.Remove(path); err == nil {
			removed++
		}
	}
	return removed, nil
}
