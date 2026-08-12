package trakt

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/berejant/movie-torrent-finder/internal/config"
	"github.com/berejant/movie-torrent-finder/internal/healthcheck"
	"github.com/berejant/movie-torrent-finder/internal/media"
	"github.com/berejant/movie-torrent-finder/internal/storage"
)

// Store is the persistence the syncer needs; *storage.Store satisfies it.
type Store interface {
	TraktCursor(ctx context.Context) (time.Time, error)
	CreateTraktRequests(ctx context.Context, items []storage.TraktRequest, checkDuplicates bool) ([]storage.Request, error)
}

// Notifier wakes the worker pool once new requests are queued.
type Notifier interface {
	Notify()
}

// Syncer polls the watchlist on an interval and queues what is new.
//
// It owns no state of its own: the access token is re-read from the jellyfin
// trakt plugin and the cursor re-read from the database on every pass, so a
// restart, a token refresh by the owning application, or a missed interval all
// resolve themselves on the next run.
type Syncer struct {
	cfg             config.Trakt
	checkDuplicates bool

	store    Store
	client   *Client
	tokens   *TokenSource
	notifier Notifier
	health   *healthcheck.Pinger
	logger   *slog.Logger

	// failures counts consecutive failed runs, for the healthcheck signal. It
	// is only touched by the sync loop.
	failures int

	wg sync.WaitGroup
}

// failuresBeforeAlert is how many runs in a row must fail before the monitor is
// told the sync is down. A single failure is unremarkable — jellyfin may be
// restarting, trakt may be briefly unreachable — and the next run an hour and a
// quarter later is a better answer than an alert. Five in a row is not a blip:
// at the default interval it means the watchlist has been unread for over an
// hour.
const failuresBeforeAlert = 5

// NewSyncer builds the syncer. Call Start to run it.
func NewSyncer(store Store, client *Client, tokens *TokenSource, cfg config.Config, notifier Notifier, logger *slog.Logger) *Syncer {
	return &Syncer{
		cfg:             cfg.Trakt,
		checkDuplicates: cfg.DuplicateCheckEnabled,
		store:           store,
		client:          client,
		tokens:          tokens,
		notifier:        notifier,
		health:          healthcheck.New(cfg.Trakt.HealthcheckBaseURL, cfg.Trakt.HealthcheckUUID, logger),
		logger:          logger.With("component", "trakt"),
	}
}

// Start runs the sync loop in the background and returns immediately.
func (s *Syncer) Start(ctx context.Context) {
	s.wg.Add(1)
	go s.run(ctx)
	s.logger.Info("trakt watchlist sync started",
		"interval", s.cfg.Interval(),
		"healthcheck", s.health.Enabled(),
	)
}

// Wait blocks until the sync loop has stopped.
func (s *Syncer) Wait() {
	if s != nil {
		s.wg.Wait()
	}
}

func (s *Syncer) run(ctx context.Context) {
	defer s.wg.Done()

	ticker := time.NewTicker(s.cfg.Interval())
	defer ticker.Stop()

	for {
		// The first pass runs at startup rather than after the first interval:
		// a restart should not delay the watchlist by a quarter of an hour.
		s.syncAndLog(ctx)

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// syncAndLog runs one pass, reports it, and signals the healthcheck monitor. A
// failure is never fatal: jellyfin may not be reachable yet, or trakt may be
// down, and the next tick is a perfectly good retry.
func (s *Syncer) syncAndLog(ctx context.Context) {
	summary, err := s.SyncOnce(ctx)

	// A cancelled context is the shutdown, not a failed run: it must neither
	// count towards the failure streak nor reset it.
	if ctx.Err() != nil {
		return
	}

	if err != nil {
		if errors.Is(err, ErrUnauthorized) {
			s.logger.Error("trakt rejected the credentials and refreshing did not help; "+
				"re-authorize the trakt plugin in jellyfin", "err", err)
		} else {
			s.logger.Error("trakt watchlist sync failed", "err", err)
		}
		s.signalFailure(ctx, err)
		return
	}

	s.logger.Info("trakt watchlist synced",
		"pages", summary.Pages,
		"scanned", summary.Scanned,
		"new", summary.New,
		"queued", summary.Queued,
		"duplicates", summary.Duplicates,
		"skipped", summary.Skipped,
	)

	s.failures = 0
	s.health.Success(ctx, fmt.Sprintf(
		"watchlist synced: %d scanned, %d new, %d queued, %d duplicate, %d skipped, over %d page(s)",
		summary.Scanned, summary.New, summary.Queued, summary.Duplicates, summary.Skipped, summary.Pages))
}

// signalFailure counts the failed run and tells the monitor once the streak is
// long enough to mean something. Below the threshold the failure is a log line
// only: the check stays up, and its own grace period is what catches a sync
// that has stopped running altogether.
func (s *Syncer) signalFailure(ctx context.Context, err error) {
	s.failures++

	if s.failures < failuresBeforeAlert {
		s.logger.Info("waiting for the next sync before alerting",
			"consecutive_failures", s.failures, "alert_after", failuresBeforeAlert)
		return
	}

	s.health.Fail(ctx, fmt.Sprintf("%d consecutive failed syncs, last error: %v", s.failures, err))
}

// Summary is what one pass did, for the log line and for tests.
type Summary struct {
	// Pages is how many watchlist pages were fetched.
	Pages int
	// Scanned is how many entries were read before the cursor stopped the walk.
	Scanned int
	// New is the entries this sync had not processed before.
	New int
	// Queued and Duplicates split New by what the request became.
	Queued     int
	Duplicates int
	// Skipped is entries without a usable movie id or title.
	Skipped int
}

// SyncOnce runs a single pass: read the token, walk the watchlist newest first
// down to the last entry already processed, and queue what is left.
func (s *Syncer) SyncOnce(ctx context.Context) (Summary, error) {
	token, err := s.tokens.Token(ctx)
	if err != nil {
		return Summary{}, err
	}

	cursor, err := s.store.TraktCursor(ctx)
	if err != nil {
		return Summary{}, err
	}

	items, summary, err := s.collect(ctx, token, cursor)
	if errors.Is(err, ErrUnauthorized) {
		// The plugin's recorded expiry said the token was good and trakt
		// disagreed: revoked, or an expiry that was simply wrong. A pass throws
		// away partial results, so replaying the whole walk costs only the
		// requests.
		s.logger.Info("trakt rejected the access token; refreshing it and retrying the sync")

		if token, err = s.tokens.Refresh(ctx, token); err != nil {
			return Summary{}, err
		}
		items, summary, err = s.collect(ctx, token, cursor)
	}
	if err != nil {
		return Summary{}, err
	}
	if len(items) == 0 {
		return summary, nil
	}

	created, err := s.store.CreateTraktRequests(ctx, items, s.checkDuplicates)
	if err != nil {
		return Summary{}, err
	}

	summary.New = len(created)
	for _, request := range created {
		if request.Status == storage.StatusDuplicate {
			summary.Duplicates++
			continue
		}
		summary.Queued++
		s.logger.Info("queued movie from trakt watchlist",
			"request_id", request.ID, "title", request.RawTitle, "query", request.Query)
	}

	if summary.Queued > 0 && s.notifier != nil {
		s.notifier.Notify()
	}
	return summary, nil
}

// collect walks the watchlist until it reaches an entry older than the cursor,
// runs out of pages, or hits MaxPages. Entries are returned oldest first, so
// the queue order matches the order they were added to the watchlist.
func (s *Syncer) collect(
	ctx context.Context,
	accessToken string,
	cursor time.Time,
) ([]storage.TraktRequest, Summary, error) {
	var (
		summary  Summary
		items    []storage.TraktRequest
		seen     = map[int64]struct{}{}
		reached  bool
		lastPage bool
	)

	for page := 1; page <= s.cfg.MaxPages; page++ {
		result, err := s.client.WatchlistMovies(ctx, accessToken, page, s.cfg.PageLimit)
		if err != nil {
			// Pages already collected are dropped rather than queued: the cursor
			// only moves forward correctly if a run is all-or-nothing.
			return nil, Summary{}, err
		}

		summary.Pages = page
		lastPage = isLastPage(result, page, s.cfg.PageLimit)

		for _, entry := range result.Items {
			// Newest first, so the first entry at or before the cursor means
			// everything below it has been processed by an earlier run.
			if !cursor.IsZero() && entry.ListedAt.Before(cursor) {
				reached = true
				break
			}
			summary.Scanned++

			request, ok := s.toRequest(entry)
			if !ok {
				summary.Skipped++
				continue
			}

			// A watchlist edited between two page requests can shift an entry
			// across the page boundary and hand it to us twice; the movie id is
			// the primary key of the bookkeeping table, so a repeat would fail
			// the whole insert.
			if _, duplicate := seen[request.MovieID]; duplicate {
				continue
			}
			seen[request.MovieID] = struct{}{}

			items = append(items, request)
		}

		if reached || lastPage {
			break
		}
	}

	if !reached && !lastPage {
		// Never silently: the rest of the watchlist is picked up by the
		// following runs, one MaxPages window at a time, but the operator
		// should see why the first sync of a long list takes several rounds.
		s.logger.Warn("watchlist scan stopped at the page limit; the rest follows on the next sync",
			"max_pages", s.cfg.MaxPages, "page_limit", s.cfg.PageLimit)
	}

	// Oldest first: the queue is served in creation order, and a watchlist read
	// newest first would otherwise download the newest addition first.
	for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
		items[i], items[j] = items[j], items[i]
	}

	return items, summary, nil
}

// isLastPage decides whether paging can stop. X-Pagination-Page-Count is the
// reliable answer; a short page is the fallback for when trakt omits it.
func isLastPage(result Page, page, limit int) bool {
	if result.PageCount > 0 {
		return page >= result.PageCount
	}
	return len(result.Items) < limit
}

// toRequest turns a watchlist entry into a schedulable request. It reports
// false for an entry with nothing to search for.
func (s *Syncer) toRequest(entry WatchlistItem) (storage.TraktRequest, bool) {
	title := strings.TrimSpace(entry.Movie.Title)
	if title == "" || entry.Movie.IDs.Trakt == 0 {
		s.logger.Warn("skipping watchlist entry without a movie id or title",
			"item_id", entry.ID, "title", entry.Movie.Title)
		return storage.TraktRequest{}, false
	}

	// The raw title carries the year for the operator; the query carries it only
	// when the tracker search is expected to cope with it.
	rawTitle := title
	query := title
	if entry.Movie.Year > 0 {
		rawTitle = fmt.Sprintf("%s (%d)", title, entry.Movie.Year)
		if s.cfg.QueryWithYear {
			query = fmt.Sprintf("%s %d", title, entry.Movie.Year)
		}
	}

	normalized := media.NormalizeQuery(query)
	if normalized == "" {
		s.logger.Warn("skipping watchlist entry with no searchable characters",
			"item_id", entry.ID, "title", title)
		return storage.TraktRequest{}, false
	}

	return storage.TraktRequest{
		NewRequest: storage.NewRequest{
			RawTitle:        rawTitle,
			Query:           query,
			NormalizedQuery: normalized,
		},
		MovieID:  entry.Movie.IDs.Trakt,
		ItemID:   entry.ID,
		Year:     entry.Movie.Year,
		ListedAt: entry.ListedAt,
	}, true
}
