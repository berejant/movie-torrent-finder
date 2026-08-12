package trakt

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/berejant/movie-torrent-finder/internal/config"
	"github.com/berejant/movie-torrent-finder/internal/storage"
)

// countingNotifier stands in for the worker pool.
type countingNotifier struct{ calls int }

func (n *countingNotifier) Notify() { n.calls++ }

// newSyncer wires a syncer onto a real SQLite store, a fake trakt and a fake
// jellyfin holding an unexpired token accepted by the fake trakt.
func newSyncer(t *testing.T, fake *fakeTrakt, tweak func(*config.Config)) (*Syncer, *storage.Store, *countingNotifier) {
	t.Helper()

	// The fake trakt only accepts testToken, so that is what jellyfin holds.
	document := fmt.Sprintf(
		`{"TraktUsers":[{"AccessToken":%q,"RefreshToken":"stored-refresh","AccessTokenExpiration":%q}]}`,
		testToken, rfc3339(time.Now().Add(30*24*time.Hour)))

	return newSyncerWithTokens(t, fake, document, newFakeOAuth(t), tweak)
}

// newSyncerWithTokens is newSyncer's wiring, parameterized over the jellyfin
// document and oauth fake so a test can set up a rejected token or a specific
// issued one without restating the store/client/syncer plumbing.
func newSyncerWithTokens(t *testing.T, fake *fakeTrakt, document string, oauth *fakeOAuth, tweak func(*config.Config)) (*Syncer, *storage.Store, *countingNotifier) {
	t.Helper()

	store, err := storage.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("storage.Open() error: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cfg := config.Config{DuplicateCheckEnabled: true, Trakt: testConfig(fake.URL)}
	if tweak != nil {
		tweak(&cfg)
	}

	client, err := NewClient(cfg.Trakt, testLogger())
	if err != nil {
		t.Fatalf("NewClient() error: %v", err)
	}

	jelly := newFakeJellyfin(t, document)
	tokens := newTokenSource(t, jelly, oauth, "")

	notifier := &countingNotifier{}
	return NewSyncer(store, client, tokens, cfg, notifier, testLogger()), store, notifier
}

func queries(t *testing.T, store *storage.Store) []string {
	t.Helper()

	requests, err := store.List(context.Background(), "", 100)
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}

	out := make([]string, 0, len(requests))
	for _, request := range requests {
		out = append(out, request.Query)
	}
	return out
}

func TestSyncQueuesWatchlistMovies(t *testing.T) {
	fake := newFakeTrakt(t, [][]WatchlistItem{{
		movie(396109, "Extraction", 2020, "2026-08-04T13:38:29Z"),
		movie(1234, "Sicario", 2015, "2026-08-03T10:00:00Z"),
	}})

	syncer, store, notifier := newSyncer(t, fake, nil)

	summary, err := syncer.SyncOnce(context.Background())
	if err != nil {
		t.Fatalf("SyncOnce() error: %v", err)
	}

	if summary.Queued != 2 || summary.Scanned != 2 {
		t.Fatalf("summary = %+v, want 2 scanned and 2 queued", summary)
	}
	if notifier.calls != 1 {
		t.Errorf("notifier called %d times, want 1", notifier.calls)
	}

	// Newest first in the response, oldest first in the queue.
	requests, err := store.List(context.Background(), "", 100)
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	if len(requests) != 2 {
		t.Fatalf("got %d requests, want 2", len(requests))
	}
	for _, request := range requests {
		if request.Status != storage.StatusQueued {
			t.Errorf("%s: status = %s, want QUEUED", request.Query, request.Status)
		}
	}

	got := queries(t, store)
	if !contains(got, "Extraction 2020") || !contains(got, "Sicario 2015") {
		t.Errorf("queries = %v, want the titles with their years", got)
	}
}

func TestSyncQueryWithoutYear(t *testing.T) {
	fake := newFakeTrakt(t, [][]WatchlistItem{{
		movie(396109, "Extraction", 2020, "2026-08-04T13:38:29Z"),
	}})

	syncer, store, _ := newSyncer(t, fake, func(cfg *config.Config) {
		cfg.Trakt.QueryWithYear = false
	})

	if _, err := syncer.SyncOnce(context.Background()); err != nil {
		t.Fatalf("SyncOnce() error: %v", err)
	}

	if got := queries(t, store); len(got) != 1 || got[0] != "Extraction" {
		t.Errorf("queries = %v, want [Extraction]", got)
	}
}

// The point of sorting by listed_at: a second run must not re-queue anything,
// and must not walk past the newest entry it already knows.
func TestSyncSkipsProcessedMoviesAndStopsAtTheCursor(t *testing.T) {
	fake := newFakeTrakt(t, [][]WatchlistItem{
		{
			movie(1, "Newest", 2024, "2026-08-04T13:38:29Z"),
			movie(2, "Middle", 2023, "2026-08-03T13:38:29Z"),
		},
		{
			movie(3, "Older", 2022, "2026-08-02T13:38:29Z"),
			movie(4, "Oldest", 2021, "2026-08-01T13:38:29Z"),
		},
	})

	syncer, store, notifier := newSyncer(t, fake, nil)

	first, err := syncer.SyncOnce(context.Background())
	if err != nil {
		t.Fatalf("first SyncOnce() error: %v", err)
	}
	if first.Queued != 4 || first.Pages != 2 {
		t.Fatalf("first summary = %+v, want 4 queued over 2 pages", first)
	}

	second, err := syncer.SyncOnce(context.Background())
	if err != nil {
		t.Fatalf("second SyncOnce() error: %v", err)
	}
	// The entry sitting exactly on the cursor is read again on purpose — a bulk
	// add can give several entries the same listed_at — and dropped by movie id.
	if second.New != 0 || second.Scanned != 1 {
		t.Errorf("second summary = %+v, want 1 scanned and nothing new", second)
	}
	if second.Pages != 1 {
		t.Errorf("second sync fetched %d pages, want 1: the cursor should stop the walk", second.Pages)
	}
	if want := []int{1, 2, 1}; !equalInts(fake.requestedPages(), want) {
		t.Errorf("requested pages = %v, want %v", fake.requestedPages(), want)
	}
	if notifier.calls != 1 {
		t.Errorf("notifier called %d times, want 1: no new work on the second run", notifier.calls)
	}

	if got := len(queries(t, store)); got != 4 {
		t.Errorf("got %d requests, want 4", got)
	}
}

// A movie re-added to the watchlist gets a new entry id and a newer listed_at,
// so the cursor lets it through; the movie id is what stops it.
func TestSyncSkipsAMovieReAddedToTheWatchlist(t *testing.T) {
	fake := newFakeTrakt(t, [][]WatchlistItem{{
		movie(396109, "Extraction", 2020, "2026-08-04T13:38:29Z"),
	}})

	syncer, store, _ := newSyncer(t, fake, nil)
	if _, err := syncer.SyncOnce(context.Background()); err != nil {
		t.Fatalf("first SyncOnce() error: %v", err)
	}

	readded := movie(396109, "Extraction", 2020, "2026-08-05T09:00:00Z")
	readded.ID = 999999
	fake.mu.Lock()
	fake.pages = [][]WatchlistItem{{readded}}
	fake.mu.Unlock()

	summary, err := syncer.SyncOnce(context.Background())
	if err != nil {
		t.Fatalf("second SyncOnce() error: %v", err)
	}
	if summary.Scanned != 1 {
		t.Errorf("Scanned = %d, want 1: the newer listed_at is past the cursor", summary.Scanned)
	}
	if summary.New != 0 {
		t.Errorf("New = %d, want 0: the movie was already processed", summary.New)
	}
	if got := len(queries(t, store)); got != 1 {
		t.Errorf("got %d requests, want 1", got)
	}
}

// The same entry can arrive twice when the watchlist is edited between two page
// requests. The bookkeeping table is keyed by movie id, so a repeat inside one
// batch would fail the insert if it were not filtered out first.
func TestSyncCollapsesAMovieRepeatedAcrossPages(t *testing.T) {
	repeated := movie(1, "Extraction", 2020, "2026-08-04T13:38:29Z")
	fake := newFakeTrakt(t, [][]WatchlistItem{
		{repeated, movie(2, "Sicario", 2015, "2026-08-03T13:38:29Z")},
		{repeated, movie(3, "Dune", 2021, "2026-08-02T13:38:29Z")},
	})

	syncer, store, _ := newSyncer(t, fake, nil)

	summary, err := syncer.SyncOnce(context.Background())
	if err != nil {
		t.Fatalf("SyncOnce() error: %v", err)
	}
	if summary.Queued != 3 {
		t.Errorf("Queued = %d, want 3", summary.Queued)
	}
	if got := len(queries(t, store)); got != 3 {
		t.Errorf("got %d requests, want 3", got)
	}
}

// Entries with nothing to search for are counted, not queued.
func TestSyncSkipsEntriesWithoutAMovieID(t *testing.T) {
	broken := movie(0, "No IDs", 2020, "2026-08-04T13:38:29Z")
	broken.Movie.IDs.Trakt = 0

	fake := newFakeTrakt(t, [][]WatchlistItem{{
		broken,
		movie(2, "Sicario", 2015, "2026-08-03T13:38:29Z"),
	}})

	syncer, _, _ := newSyncer(t, fake, nil)

	summary, err := syncer.SyncOnce(context.Background())
	if err != nil {
		t.Fatalf("SyncOnce() error: %v", err)
	}
	if summary.Skipped != 1 || summary.Queued != 1 {
		t.Errorf("summary = %+v, want 1 skipped and 1 queued", summary)
	}
}

// Without X-Pagination-Page-Count a short page has to end the walk, otherwise
// the syncer would keep asking for empty pages until MaxPages.
func TestSyncStopsOnAShortPageWithoutPaginationHeaders(t *testing.T) {
	fake := newFakeTrakt(t, [][]WatchlistItem{{
		movie(1, "Extraction", 2020, "2026-08-04T13:38:29Z"),
	}})
	fake.mu.Lock()
	fake.omitPageCount = true
	fake.mu.Unlock()

	syncer, _, _ := newSyncer(t, fake, nil)

	summary, err := syncer.SyncOnce(context.Background())
	if err != nil {
		t.Fatalf("SyncOnce() error: %v", err)
	}
	if summary.Pages != 1 {
		t.Errorf("Pages = %d, want 1", summary.Pages)
	}
}

// A failure part-way through must queue nothing: a half-applied run would move
// the cursor past entries that were never scheduled.
func TestSyncQueuesNothingWhenAPageFails(t *testing.T) {
	fake := newFakeTrakt(t, [][]WatchlistItem{{
		movie(1, "Extraction", 2020, "2026-08-04T13:38:29Z"),
	}})
	fake.setStatus(http.StatusInternalServerError)

	syncer, store, notifier := newSyncer(t, fake, nil)

	if _, err := syncer.SyncOnce(context.Background()); err == nil {
		t.Fatal("SyncOnce() succeeded, want an error")
	}
	if got := len(queries(t, store)); got != 0 {
		t.Errorf("got %d requests, want 0", got)
	}
	if notifier.calls != 0 {
		t.Errorf("notifier called %d times, want 0", notifier.calls)
	}
}

// A title already downloaded by hand is recorded as processed all the same, so
// the watchlist entry is not reconsidered on every run.
func TestSyncMarksDuplicatesAsProcessed(t *testing.T) {
	fake := newFakeTrakt(t, [][]WatchlistItem{{
		movie(396109, "Extraction", 2020, "2026-08-04T13:38:29Z"),
	}})

	syncer, store, _ := newSyncer(t, fake, nil)
	ctx := context.Background()

	created, err := store.CreateBatch(ctx, []storage.NewRequest{{
		RawTitle:        "Extraction 2020",
		Query:           "Extraction 2020",
		NormalizedQuery: "extraction 2020",
	}}, true)
	if err != nil {
		t.Fatalf("CreateBatch() error: %v", err)
	}
	if err := store.MarkDownloaded(ctx, created[0].ID, "/torrents/extraction.torrent"); err != nil {
		t.Fatalf("MarkDownloaded() error: %v", err)
	}

	summary, err := syncer.SyncOnce(ctx)
	if err != nil {
		t.Fatalf("SyncOnce() error: %v", err)
	}
	if summary.Duplicates != 1 || summary.Queued != 0 {
		t.Fatalf("summary = %+v, want 1 duplicate and nothing queued", summary)
	}

	// Second run: the entry is behind the cursor and recorded, so it is gone.
	again, err := syncer.SyncOnce(ctx)
	if err != nil {
		t.Fatalf("second SyncOnce() error: %v", err)
	}
	if again.New != 0 {
		t.Errorf("New = %d on the second run, want 0", again.New)
	}
}

// healthRecorder is a stand-in for healthchecks.io.
type healthRecorder struct {
	*httptest.Server

	mu    sync.Mutex
	paths []string
}

func newHealthRecorder(t *testing.T) *healthRecorder {
	t.Helper()

	rec := &healthRecorder{}
	rec.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.paths = append(rec.paths, r.URL.Path)
		rec.mu.Unlock()
		_, _ = io.WriteString(w, "OK")
	}))
	t.Cleanup(rec.Close)

	return rec
}

// signals reports the pings received as "ok" and "fail", in order.
func (r *healthRecorder) signals() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]string, 0, len(r.paths))
	for _, path := range r.paths {
		if strings.HasSuffix(path, "/fail") {
			out = append(out, "fail")
			continue
		}
		out = append(out, "ok")
	}
	return out
}

const healthUUID = "c38a1b6c-0607-4e4c-8bbf-fc2d50e1f0e1"

func TestSyncSignalsSuccess(t *testing.T) {
	fake := newFakeTrakt(t, [][]WatchlistItem{{
		movie(396109, "Extraction", 2020, "2026-08-04T13:38:29Z"),
	}})
	health := newHealthRecorder(t)

	syncer, _, _ := newSyncer(t, fake, func(cfg *config.Config) {
		cfg.Trakt.HealthcheckUUID = healthUUID
		cfg.Trakt.HealthcheckBaseURL = health.URL
	})

	syncer.syncAndLog(context.Background())

	if got := health.signals(); len(got) != 1 || got[0] != "ok" {
		t.Fatalf("signals = %v, want one success", got)
	}
}

// Four failures are a bad afternoon, not an outage: the fifth is what alerts.
func TestSyncSignalsFailureOnlyAfterFiveInARow(t *testing.T) {
	fake := newFakeTrakt(t, [][]WatchlistItem{{
		movie(396109, "Extraction", 2020, "2026-08-04T13:38:29Z"),
	}})
	health := newHealthRecorder(t)

	syncer, _, _ := newSyncer(t, fake, func(cfg *config.Config) {
		cfg.Trakt.HealthcheckUUID = healthUUID
		cfg.Trakt.HealthcheckBaseURL = health.URL
	})

	fake.setStatus(http.StatusInternalServerError)
	ctx := context.Background()

	for i := 1; i < failuresBeforeAlert; i++ {
		syncer.syncAndLog(ctx)
		if got := health.signals(); len(got) != 0 {
			t.Fatalf("after %d failure(s) signals = %v, want none yet", i, got)
		}
	}

	syncer.syncAndLog(ctx)
	if got := health.signals(); len(got) != 1 || got[0] != "fail" {
		t.Fatalf("signals = %v, want one failure after %d failed runs", got, failuresBeforeAlert)
	}

	// Still failing: the monitor keeps being told, so the check stays down.
	syncer.syncAndLog(ctx)
	if got := health.signals(); len(got) != 2 || got[1] != "fail" {
		t.Fatalf("signals = %v, want a second failure", got)
	}

	// Recovered: the streak resets, so the next four failures are quiet again.
	fake.setStatus(0)
	syncer.syncAndLog(ctx)
	if got := health.signals(); len(got) != 3 || got[2] != "ok" {
		t.Fatalf("signals = %v, want a success once the sync recovers", got)
	}
	if syncer.failures != 0 {
		t.Errorf("failures = %d after a successful run, want 0", syncer.failures)
	}
}

// A shutdown is not a failed run: it must not push the streak towards an alert.
func TestSyncDoesNotSignalOnShutdown(t *testing.T) {
	fake := newFakeTrakt(t, [][]WatchlistItem{{
		movie(396109, "Extraction", 2020, "2026-08-04T13:38:29Z"),
	}})
	health := newHealthRecorder(t)

	syncer, _, _ := newSyncer(t, fake, func(cfg *config.Config) {
		cfg.Trakt.HealthcheckUUID = healthUUID
		cfg.Trakt.HealthcheckBaseURL = health.URL
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	syncer.syncAndLog(ctx)

	if got := health.signals(); len(got) != 0 {
		t.Errorf("signals = %v, want none during shutdown", got)
	}
	if syncer.failures != 0 {
		t.Errorf("failures = %d, want 0: a cancelled run is not a failed run", syncer.failures)
	}
}

// Without a configured id nothing is ever sent, and the sync is unaffected.
func TestSyncWithoutAHealthcheckSendsNothing(t *testing.T) {
	fake := newFakeTrakt(t, [][]WatchlistItem{{
		movie(396109, "Extraction", 2020, "2026-08-04T13:38:29Z"),
	}})
	health := newHealthRecorder(t)

	// Base URL set, id left empty: the id alone decides.
	syncer, store, _ := newSyncer(t, fake, func(cfg *config.Config) {
		cfg.Trakt.HealthcheckBaseURL = health.URL
	})

	syncer.syncAndLog(context.Background())
	fake.setStatus(http.StatusInternalServerError)
	for range failuresBeforeAlert {
		syncer.syncAndLog(context.Background())
	}

	if got := health.signals(); len(got) != 0 {
		t.Errorf("signals = %v, want none when no UUID is configured", got)
	}
	if got := len(queries(t, store)); got != 1 {
		t.Errorf("got %d requests, want the sync itself to be unaffected", got)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func equalInts(got, want []int) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// An expiry the plugin recorded can be wrong — a revoked token, a clock that
// moved. One refresh and one retry: a pass discards partial results, so
// replaying the whole walk costs only the requests.
func TestSyncRefreshesAndRetriesOnceOnRejection(t *testing.T) {
	fake := newFakeTrakt(t, [][]WatchlistItem{{
		movie(396109, "Extraction", 2020, "2026-08-04T13:38:29Z"),
	}})

	// Jellyfin holds a token the fake trakt rejects, but claims it is good for
	// another month; only a refresh produces the token trakt accepts.
	document := fmt.Sprintf(
		`{"TraktUsers":[{"AccessToken":"revoked","RefreshToken":"stored-refresh","AccessTokenExpiration":%q}]}`,
		rfc3339(time.Now().Add(30*24*time.Hour)))
	oauth := newFakeOAuth(t)
	oauth.body = fmt.Sprintf(
		`{"access_token":%q,"refresh_token":"fresh-refresh","expires_in":7776000}`, testToken)

	syncer, _, _ := newSyncerWithTokens(t, fake, document, oauth, nil)

	summary, err := syncer.SyncOnce(context.Background())
	if err != nil {
		t.Fatalf("SyncOnce() error: %v", err)
	}
	if summary.Queued != 1 {
		t.Errorf("summary = %+v, want 1 queued", summary)
	}
	if oauth.calls != 1 {
		t.Errorf("refreshed %d times, want 1", oauth.calls)
	}
}

// A second rejection is not a refresh loop: one retry, then the run fails and
// the next tick tries again.
func TestSyncGivesUpAfterASecondRejection(t *testing.T) {
	fake := newFakeTrakt(t, [][]WatchlistItem{{
		movie(396109, "Extraction", 2020, "2026-08-04T13:38:29Z"),
	}})

	document := fmt.Sprintf(
		`{"TraktUsers":[{"AccessToken":"revoked","RefreshToken":"stored-refresh","AccessTokenExpiration":%q}]}`,
		rfc3339(time.Now().Add(30*24*time.Hour)))
	oauth := newFakeOAuth(t) // issues "fresh-access", which the fake trakt also rejects

	syncer, _, _ := newSyncerWithTokens(t, fake, document, oauth, nil)

	_, err := syncer.SyncOnce(context.Background())
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("error = %v, want ErrUnauthorized", err)
	}
	if oauth.calls != 1 {
		t.Errorf("refreshed %d times, want exactly 1", oauth.calls)
	}
}
