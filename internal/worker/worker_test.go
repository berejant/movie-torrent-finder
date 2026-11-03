package worker

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/berejant/movie-torrent-finder/internal/config"
	"github.com/berejant/movie-torrent-finder/internal/media"
	"github.com/berejant/movie-torrent-finder/internal/storage"
	"github.com/berejant/movie-torrent-finder/internal/tracker"
)

// release is one row a fake tracker renders.
type release struct {
	id      int
	title   string
	size    string
	company string
}

// fakeTracker is a minimal TorrentPier stand-in: it authenticates, renders the
// result table in the column layout the torrentpier preset expects, and serves
// a valid .torrent. Setting fail makes every page 500, which is the transient
// failure the retry policy is supposed to notice.
func fakeTracker(t *testing.T, releases []release, fail bool) *httptest.Server {
	t.Helper()

	const loggedIn = `<a href="logout.php">Logout</a>`
	const loggedOut = `<html><body><a id="register_link" href="profile.php?mode=register">Register</a></body></html>`

	mux := http.NewServeMux()

	mux.HandleFunc("/login.php", func(w http.ResponseWriter, r *http.Request) {
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = r.ParseForm()
		if r.Form.Get("login_username") != "tester" || r.Form.Get("login_password") != "secret" {
			_, _ = io.WriteString(w, loggedOut)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "ok", Path: "/"})
		_, _ = io.WriteString(w, `<html><body>`+loggedIn+`</body></html>`)
	})

	mux.HandleFunc("/tracker.php", func(w http.ResponseWriter, r *http.Request) {
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if cookie, err := r.Cookie("session"); err != nil || cookie.Value != "ok" {
			_, _ = io.WriteString(w, loggedOut)
			return
		}

		var body strings.Builder
		body.WriteString(`<html><body>` + loggedIn + `<table id="forum_table"><tbody>`)
		if r.URL.Query().Get("nm") != "" {
			for _, item := range releases {
				fmt.Fprintf(&body,
					`<tr><td>added</td><td>ok</td>`+
						`<td><a href="forum-1">Movies</a></td>`+
						`<td><a href="topic-%d">%s</a></td>`+
						`<td>uploader</td>`+
						`<td>%s <a href="dl.php?id=%d">DL</a></td>`+
						`<td>10</td><td>1</td><td>0</td><td>today</td></tr>`,
					item.id, item.title, item.size, item.id)
			}
		}
		body.WriteString(`</tbody></table></body></html>`)
		_, _ = io.WriteString(w, body.String())
	})

	mux.HandleFunc("/dl.php", func(w http.ResponseWriter, _ *http.Request) {
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, "d8:announce20:http://tracker/annce4:infod4:name4:teseee")
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/topic-") {
			if fail {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			topicID := strings.TrimPrefix(r.URL.Path, "/topic-")
			comp := "Warner Bros"
			for _, item := range releases {
				if fmt.Sprintf("%d", item.id) == topicID && item.company != "" {
					comp = item.company
					break
				}
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprintf(w, `<html><body>%s<div class="post_body">Кінокомпанія: %s<br>Сюжет: A movie.</div></body></html>`, loggedIn, comp)
			return
		}
		http.NotFound(w, r)
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// testPool wires a pool over the given trackers, each entry becoming one
// tracker whose priority is its position in the slice.
func testPool(t *testing.T, store *storage.Store, servers map[string]*httptest.Server, order []string) *Pool {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	cfg := config.Config{
		TorrentFilesDir:         t.TempDir(),
		Workers:                 1,
		Retry:                   config.Retry{MaxAttempts: 5, BaseSeconds: 3, MaxBackoffSeconds: 60},
		MockbusterFilterEnabled: true,
	}

	clients := make([]*tracker.Client, 0, len(order))
	for i, name := range order {
		trackerCfg := config.Tracker{
			Name:           name,
			BaseURL:        servers[name].URL,
			Login:          "tester",
			Password:       "secret",
			Priority:       i + 1, // first in the list is the preferred tracker
			TimeoutSeconds: 5,
			RPS:            50,
			UserAgent:      config.DefaultUserAgent,
			Options:        config.DefaultTrackerOptions(),
		}
		cfg.Trackers = append(cfg.Trackers, trackerCfg)

		client, err := tracker.New(trackerCfg, logger)
		if err != nil {
			t.Fatalf("tracker.New(%s) error: %v", name, err)
		}
		clients = append(clients, client)
	}

	return New(store, clients, cfg, logger)
}

// runOne claims the single queued request and processes it synchronously, so
// the assertions never race the pool's goroutines.
func runOne(t *testing.T, pool *Pool) storage.Request {
	t.Helper()

	ctx := context.Background()
	request, ok, err := pool.store.ClaimNext(ctx)
	if err != nil {
		t.Fatalf("ClaimNext() error: %v", err)
	}
	if !ok {
		t.Fatal("ClaimNext() found nothing to do")
	}

	pool.process(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), request)

	processed, err := pool.store.Get(ctx, request.ID)
	if err != nil {
		t.Fatalf("Get() error: %v", err)
	}
	return processed
}

func queueOne(t *testing.T, query string) *storage.Store {
	t.Helper()

	store, err := storage.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("storage.Open() error: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	_, err = store.CreateBatch(context.Background(), []storage.NewRequest{{
		RawTitle:        query,
		Query:           query,
		NormalizedQuery: media.NormalizeQuery(query),
	}}, true)
	if err != nil {
		t.Fatalf("CreateBatch() error: %v", err)
	}
	return store
}

// The point of searching several trackers at once: the better release wins even
// when it sits on the tracker configured second.
func TestBestQualityWinsAcrossTrackers(t *testing.T) {
	servers := map[string]*httptest.Server{
		"toloka": fakeTracker(t, []release{{1, "Dune Part Two 2024 1080p x265", "12 GB", ""}}, false),
		"mazepa": fakeTracker(t, []release{{2, "Dune Part Two 2024 2160p x265", "40 GB", ""}}, false),
	}

	store := queueOne(t, "Dune Part Two")
	pool := testPool(t, store, servers, []string{"toloka", "mazepa"})

	request := runOne(t, pool)

	if request.Status != storage.StatusDownloaded {
		t.Fatalf("status = %s (%s), want DOWNLOADED", request.Status, request.LastError)
	}
	if request.Tracker != "mazepa" {
		t.Errorf("tracker = %q, want mazepa: 2160p must beat the preferred tracker's 1080p", request.Tracker)
	}
	if request.ResultQuality != media.Quality2160 {
		t.Errorf("quality = %q, want 2160p", request.ResultQuality)
	}
	// The saved filename carries the tracker, so the winner has to be the one
	// the file actually came from.
	if !strings.Contains(filepath.Base(request.FilePath), "-mazepa-") {
		t.Errorf("file %q does not name the winning tracker", request.FilePath)
	}
}

// With nothing to choose between them, the priority order decides.
func TestTrackerPriorityBreaksTiesAcrossTrackers(t *testing.T) {
	servers := map[string]*httptest.Server{
		"toloka": fakeTracker(t, []release{{1, "Dune Part Two 2024 1080p x265", "12 GB", ""}}, false),
		"mazepa": fakeTracker(t, []release{{2, "Dune Part Two 2024 1080p x265", "40 GB", ""}}, false),
	}

	store := queueOne(t, "Dune Part Two")
	pool := testPool(t, store, servers, []string{"toloka", "mazepa"})

	request := runOne(t, pool)

	if request.Status != storage.StatusDownloaded {
		t.Fatalf("status = %s (%s), want DOWNLOADED", request.Status, request.LastError)
	}
	if request.Tracker != "toloka" {
		t.Errorf("tracker = %q, want toloka: equal releases go to the preferred tracker", request.Tracker)
	}
}

// One tracker being down must not hold back a release another tracker found.
func TestPartialTrackerFailureStillDownloads(t *testing.T) {
	servers := map[string]*httptest.Server{
		"toloka": fakeTracker(t, nil, true),
		"mazepa": fakeTracker(t, []release{{2, "Dune Part Two 2024 1080p x265", "12 GB", ""}}, false),
	}

	store := queueOne(t, "Dune Part Two")
	pool := testPool(t, store, servers, []string{"toloka", "mazepa"})

	request := runOne(t, pool)

	if request.Status != storage.StatusDownloaded {
		t.Fatalf("status = %s (%s), want DOWNLOADED", request.Status, request.LastError)
	}
	if request.Tracker != "mazepa" {
		t.Errorf("tracker = %q, want mazepa", request.Tracker)
	}
}

// When every tracker fails in a retryable way the request goes back to the
// queue rather than being written off.
func TestAllTrackersFailingSchedulesRetry(t *testing.T) {
	servers := map[string]*httptest.Server{
		"toloka": fakeTracker(t, nil, true),
		"mazepa": fakeTracker(t, nil, true),
	}

	store := queueOne(t, "Dune Part Two")
	pool := testPool(t, store, servers, []string{"toloka", "mazepa"})

	request := runOne(t, pool)

	if request.Status != storage.StatusQueued {
		t.Fatalf("status = %s, want QUEUED for a retry", request.Status)
	}
	if request.NextAttemptAt == nil {
		t.Error("a retry must be scheduled")
	}
	// The recorded error has to say which trackers failed, since that is all
	// the operator sees in the table.
	for _, name := range []string{"toloka", "mazepa"} {
		if !strings.Contains(request.LastError, name) {
			t.Errorf("last error %q does not mention %s", request.LastError, name)
		}
	}
}

// Silence from every tracker is an answer, not a failure, so it is never
// retried automatically.
func TestNoResultsAnywhereIsNotFound(t *testing.T) {
	servers := map[string]*httptest.Server{
		"toloka": fakeTracker(t, nil, false),
		"mazepa": fakeTracker(t, nil, false),
	}

	store := queueOne(t, "Dune Part Two")
	pool := testPool(t, store, servers, []string{"toloka", "mazepa"})

	request := runOne(t, pool)

	if request.Status != storage.StatusNotFound {
		t.Fatalf("status = %s, want NOT_FOUND", request.Status)
	}
	if request.NextAttemptAt != nil {
		t.Error("NOT_FOUND must not schedule a retry")
	}
}

func TestCombineFailures(t *testing.T) {
	transient := tracker.Transient(fmt.Errorf("tracker: HTTP 503"))
	permanent := tracker.ErrLoginFailed

	cases := map[string]struct {
		failures      []trackerFailure
		wantNoResults bool
		wantTransient bool
	}{
		"nothing anywhere": {
			failures:      []trackerFailure{{"a", tracker.ErrNoResults}, {"b", tracker.ErrNoResults}},
			wantNoResults: true,
		},
		// A tracker that could not answer may have been holding the better
		// release, so the attempt is worth repeating even though another
		// tracker did answer.
		"one down, one empty": {
			failures:      []trackerFailure{{"a", transient}, {"b", tracker.ErrNoResults}},
			wantTransient: true,
		},
		"bad credentials only": {
			failures: []trackerFailure{{"a", permanent}},
		},
		"mixed permanent and transient": {
			failures:      []trackerFailure{{"a", permanent}, {"b", transient}},
			wantTransient: true,
		},
	}

	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			err := combineFailures(testCase.failures)

			if got := err == tracker.ErrNoResults; got != testCase.wantNoResults {
				t.Errorf("ErrNoResults = %v, want %v (err = %v)", got, testCase.wantNoResults, err)
			}
			if got := tracker.IsTransient(err); got != testCase.wantTransient {
				t.Errorf("IsTransient = %v, want %v (err = %v)", got, testCase.wantTransient, err)
			}
		})
	}
}

func TestMockbusterReleaseIsSkippedForLegitimate(t *testing.T) {
	// Release 1 is 2160p (better quality) but produced by The Asylum (mockbuster).
	// Release 2 is 1080p from Universal Pictures (legitimate).
	servers := map[string]*httptest.Server{
		"toloka": fakeTracker(t, []release{
			{id: 1, title: "The Odyssey 2026 2160p", size: "20 GB", company: "The Asylum"},
			{id: 2, title: "The Odyssey 2026 1080p", size: "8 GB", company: "Universal Pictures"},
		}, false),
	}
	store := queueOne(t, "The Odyssey")
	pool := testPool(t, store, servers, []string{"toloka"})

	request := runOne(t, pool)

	if request.Status != storage.StatusDownloaded {
		t.Fatalf("status = %s, want DOWNLOADED", request.Status)
	}
	// The 1080p release should be selected because the 2160p one is from The Asylum
	if request.ResultQuality != media.Quality1080 {
		t.Errorf("selected quality = %q, want %q (The Asylum mockbuster should have been skipped)", request.ResultQuality, media.Quality1080)
	}
	if !strings.Contains(request.ResultTitle, "1080p") {
		t.Errorf("selected title = %q, want 1080p release", request.ResultTitle)
	}
}

func TestAllMockbustersResultsInNotFound(t *testing.T) {
	// Only The Asylum release is found on the tracker.
	servers := map[string]*httptest.Server{
		"toloka": fakeTracker(t, []release{
			{id: 1, title: "The Odyssey 2026 1080p", size: "5 GB", company: "The Asylum"},
		}, false),
	}
	store := queueOne(t, "The Odyssey")
	pool := testPool(t, store, servers, []string{"toloka"})

	request := runOne(t, pool)

	if request.Status != storage.StatusNotFound {
		t.Fatalf("status = %s, want NOT_FOUND", request.Status)
	}
	if request.NextAttemptAt != nil {
		t.Error("NOT_FOUND must not schedule a retry")
	}
}
