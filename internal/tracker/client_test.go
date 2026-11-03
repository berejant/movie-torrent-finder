package tracker

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/berejant/movie-torrent-finder/internal/config"
	"github.com/berejant/movie-torrent-finder/internal/media"
)

// The fixture mirrors the markup the default selectors target on the real
// tracker: an anonymous visitor gets a #register_link, an authenticated one
// gets a logout link, and results live in #forum_table.
const (
	loggedOutPage = `<html><body><a href="login.php">Login</a>` +
		`<a id="register_link" href="profile.php?mode=register">Register</a></body></html>`
	loggedInMarkup = `<a href="logout.php">Logout</a>`
)

// fakeTracker is a minimal stand-in for a TorrentPier install: it checks the
// login form, sets a session cookie and renders a result table in the column
// layout the parser expects.
func fakeTracker(t *testing.T) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()

	mux.HandleFunc("/login.php", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.Form.Get("login_username") != "tester" || r.Form.Get("login_password") != "secret" {
			_, _ = io.WriteString(w, loggedOutPage)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "ok", Path: "/"})
		_, _ = io.WriteString(w, `<html><body>`+loggedInMarkup+`</body></html>`)
	})

	mux.HandleFunc("/tracker.php", func(w http.ResponseWriter, r *http.Request) {
		if cookie, err := r.Cookie("session"); err != nil || cookie.Value != "ok" {
			_, _ = io.WriteString(w, loggedOutPage)
			return
		}

		var body strings.Builder
		body.WriteString(`<html><body>` + loggedInMarkup + `<table id="forum_table">`)
		body.WriteString(`<tr><th>added</th><th>st</th><th>forum</th><th>topic</th>` +
			`<th>author</th><th>size</th><th>S</th><th>L</th><th>R</th><th>date</th></tr>`)

		if r.URL.Query().Get("nm") != "" {
			for id, release := range []struct {
				title string
				size  string
			}{
				{"Dune Part Two (2024) 1080p BDRip x264", "8.5 GB"},
				{"Dune Part Two (2024) 2160p UHD BluRay x265 HDR", "22.4 GB"},
				{"Dune Part Two (2024) 2160p WEB-DL x264", "18.1 GB"},
				{"Dune Part Two (2024) 2160p BluRay x265", "45.9 GB"},
				{"Dune Part Two (2024) 720p WEBRip", "2.1 GB"},
			} {
				fmt.Fprintf(&body,
					`<tr><td>x</td><td>ok</td><td><a href="forum-12">HD Movies</a></td>`+
						`<td><a href="topic-%d">%s</a></td><td>uploader</td>`+
						`<td>%s <a href="dl.php?id=%d">DL</a></td>`+
						`<td>10</td><td>2</td><td>5</td><td>today</td></tr>`,
					id+100, release.title, release.size, id+100)
			}
		}

		body.WriteString(`</table></body></html>`)
		_, _ = io.WriteString(w, body.String())
	})

	mux.HandleFunc("/dl.php", func(w http.ResponseWriter, r *http.Request) {
		if cookie, err := r.Cookie("session"); err != nil || cookie.Value != "ok" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/x-bittorrent")
		_, _ = io.WriteString(w, "d8:announce20:http://tracker/annce4:infod4:name4:dunee e")
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func newTestClient(t *testing.T, baseURL string) *Client {
	t.Helper()

	cfg := config.Tracker{
		Name:           "mazepa",
		BaseURL:        baseURL,
		Login:          "tester",
		Password:       "secret",
		Priority:       1,
		TimeoutSeconds: 5,
		RPS:            50, // keep the test fast; production defaults to 1
		UserAgent:      config.DefaultUserAgent,
		Options:        config.DefaultTrackerOptions(),
	}

	client, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	return client
}

func TestSearchRanksBestReleaseFirst(t *testing.T) {
	server := fakeTracker(t)
	client := newTestClient(t, server.URL)

	torrents, err := client.Search(context.Background(), "Dune Part Two")
	if err != nil {
		t.Fatalf("Search() error: %v", err)
	}
	if len(torrents) != 5 {
		t.Fatalf("parsed %d rows, want 5", len(torrents))
	}

	best, err := client.SelectBest(torrents)
	if err != nil {
		t.Fatalf("SelectBest() error: %v", err)
	}

	// 2160p beats everything; among the 2160p releases h265 wins; among the two
	// h265 releases the larger file wins.
	if want := "Dune Part Two (2024) 2160p BluRay x265"; best.Title != want {
		t.Errorf("best = %q, want %q", best.Title, want)
	}
	if best.Quality != media.Quality2160 || best.Codec != media.CodecH265 {
		t.Errorf("best attributes = %s/%s, want 2160p/h265", best.Quality, best.Codec)
	}
	if best.SizeBytes == 0 {
		t.Error("best release has no parsed size")
	}
}

func TestSearchWithNoMatchesReturnsErrNoResults(t *testing.T) {
	server := fakeTracker(t)
	client := newTestClient(t, server.URL)

	// The fake renders an empty table when the query parameter is blank; an
	// unknown title behaves the same way through the search path.
	client.options.SearchQueryField = "unused"

	if _, err := client.Search(context.Background(), "nothing here"); err != ErrNoResults {
		t.Fatalf("Search() error = %v, want ErrNoResults", err)
	}
}

func TestDownloadWritesNamedTorrentFile(t *testing.T) {
	server := fakeTracker(t)
	client := newTestClient(t, server.URL)
	dir := t.TempDir()

	torrents, err := client.Search(context.Background(), "Dune Part Two")
	if err != nil {
		t.Fatalf("Search() error: %v", err)
	}
	best, _ := client.SelectBest(torrents)

	path, err := client.Download(context.Background(), best, dir, "01JQ8X4M7ZK3RN")
	if err != nil {
		t.Fatalf("Download() error: %v", err)
	}

	name := filepath.Base(path)
	if !strings.HasSuffix(name, "-mazepa-2160p-01JQ8X4M7ZK3RN.torrent") {
		t.Errorf("filename = %q, want the <title>-<tracker>-<quality>-<id> pattern", name)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}
	if !looksLikeTorrent(content) {
		t.Error("saved file is not a bencoded torrent")
	}

	// No temporary files may survive a successful download.
	leftovers, _ := filepath.Glob(filepath.Join(dir, ".torrent-*.tmp"))
	if len(leftovers) != 0 {
		t.Errorf("temp files left behind: %v", leftovers)
	}
}

func TestDownloadRejectsNonTorrentPayload(t *testing.T) {
	server := fakeTracker(t)
	client := newTestClient(t, server.URL)

	if err := client.EnsureSession(context.Background()); err != nil {
		t.Fatalf("EnsureSession() error: %v", err)
	}

	// An HTML error page behind HTTP 200 must not be saved as a .torrent.
	bogus := Torrent{Title: "bogus", DownloadURL: server.URL + "/tracker.php"}
	if _, err := client.Download(context.Background(), bogus, t.TempDir(), "01JQ8X4M7ZK3RN"); err != ErrInvalidTorrent {
		t.Fatalf("Download() error = %v, want ErrInvalidTorrent", err)
	}
}

func TestLoginFailureIsNotTransient(t *testing.T) {
	server := fakeTracker(t)
	client := newTestClient(t, server.URL)
	client.cfg.Password = "wrong"

	err := client.EnsureSession(context.Background())
	if err == nil {
		t.Fatal("EnsureSession() succeeded with bad credentials")
	}
	if IsTransient(err) {
		t.Error("bad credentials must not be retried as a transient failure")
	}
}

func TestApplyQueryModifiers(t *testing.T) {
	client := &Client{
		options: config.TrackerOptions{
			SearchQuerySuffix: "ukr",
		},
	}

	tests := []struct {
		input string
		want  string
	}{
		{"Inception", "Inception ukr"},
		{"Inception 2010", "Inception 2010 ukr"},
		{"Inception ukr", "Inception ukr"},
		{"Inception UKR", "Inception UKR"},
		{"Inception [ukr]", "Inception [ukr]"},
		{"Inception (ukr)", "Inception (ukr)"},
		{"ukr Inception", "ukr Inception"},
		{"Початок", "Початок ukr"},
		{"Початок ukr", "Початок ukr"},
		{"", ""},
	}

	for _, tt := range tests {
		got := client.applyQueryModifiers(tt.input)
		if got != tt.want {
			t.Errorf("applyQueryModifiers(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestExtractInfoHash(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{
			input: "magnet:?xt=urn:btih:EAB42BC99049DD581A0AF9DC381FCAB94718EF49&dn=Avatar",
			want:  "EAB42BC99049DD581A0AF9DC381FCAB94718EF49",
		},
		{
			input: "magnet:?xt=urn:btih:eab42bc99049dd581a0af9dc381fcab94718ef49&dn=Avatar",
			want:  "EAB42BC99049DD581A0AF9DC381FCAB94718EF49",
		},
		{
			input: "https://itorrents.org/torrent/EAB42BC99049DD581A0AF9DC381FCAB94718EF49.torrent",
			want:  "EAB42BC99049DD581A0AF9DC381FCAB94718EF49",
		},
		{
			input: "EAB42BC99049DD581A0AF9DC381FCAB94718EF49",
			want:  "EAB42BC99049DD581A0AF9DC381FCAB94718EF49",
		},
		{
			input: "https://toloka.to/download.php?id=123",
			want:  "",
		},
	}

	for _, tt := range tests {
		got := extractInfoHash(tt.input)
		if got != tt.want {
			t.Errorf("extractInfoHash(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestPirateBaySearchAndDownload(t *testing.T) {
	var requestedQuery string

	mux := http.NewServeMux()
	mux.HandleFunc("/q.php", func(w http.ResponseWriter, r *http.Request) {
		requestedQuery = r.URL.Query().Get("q")
		w.Header().Set("Content-Type", "application/json")
		// Simulate apibay.org response
		_, _ = io.WriteString(w, `[
			{
				"id": "84785121",
				"name": "Primetime (2026) 720p WEB-DL [Ukr Dub]",
				"info_hash": "1225555D60598DE8BF688BDA3905E5E4DB3FB6AE",
				"size": "4893697434",
				"category": "207"
			},
			{
				"id": "84785124",
				"name": "Primetime (2026) 1080p WEB-DL [Ukr Dub]",
				"info_hash": "611AC6B64A2A0E421E2C9CDB70AD2C7EC3F85304",
				"size": "6440740683",
				"category": "207"
			}
		]`)
	})

	mux.HandleFunc("/torrent/611AC6B64A2A0E421E2C9CDB70AD2C7EC3F85304.torrent", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-bittorrent")
		_, _ = io.WriteString(w, "d8:announce20:http://tracker/annce4:infod4:name9:primetimeee")
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	preset, ok := config.LookupPreset("piratebay")
	if !ok {
		t.Fatal("piratebay preset not found")
	}

	cfg := config.Tracker{
		Name:           "piratebay",
		BaseURL:        server.URL,
		Priority:       1,
		TimeoutSeconds: 5,
		RPS:            50,
		UserAgent:      config.DefaultUserAgent,
		Options:        preset.Options,
	}

	client, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	torrents, err := client.Search(context.Background(), "Primetime")
	if err != nil {
		t.Fatalf("Search() error: %v", err)
	}

	if want := "Primetime ukr"; requestedQuery != want {
		t.Errorf("requestedQuery = %q, want %q", requestedQuery, want)
	}

	if len(torrents) != 2 {
		t.Fatalf("len(torrents) = %d, want 2", len(torrents))
	}

	// 1080p must rank ahead of 720p
	best := torrents[0]
	if best.Quality != media.Quality1080 {
		t.Errorf("best.Quality = %q, want 1080p", best.Quality)
	}
	if want := "Primetime (2026) 1080p WEB-DL [Ukr Dub]"; best.Title != want {
		t.Errorf("best.Title = %q, want %q", best.Title, want)
	}

	// Override DownloadURL to point to test server's torrent path
	best.DownloadURL = server.URL + "/torrent/611AC6B64A2A0E421E2C9CDB70AD2C7EC3F85304.torrent"
	dir := t.TempDir()
	path, err := client.Download(context.Background(), best, dir, "01KYYJA6CMF8N3Q77NAM0VJ4DQ")
	if err != nil {
		t.Fatalf("Download() error: %v", err)
	}

	if !strings.HasSuffix(path, "Primetime 2026 1080p WEB-DL Ukr Dub-piratebay-1080p-01KYYJA6CMF8N3Q77NAM0VJ4DQ.torrent") {
		t.Errorf("unexpected saved file path: %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error: %v", err)
	}
	if !looksLikeTorrent(data) {
		t.Error("saved file is not a valid torrent")
	}
}

func TestPirateBayNoResults(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/q.php", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[{"id":"0","name":"No results returned","info_hash":"0000000000000000000000000000000000000000"}]`)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	preset, _ := config.LookupPreset("piratebay")
	cfg := config.Tracker{
		Name:      "piratebay",
		BaseURL:   server.URL,
		Priority:  1,
		Options:   preset.Options,
		UserAgent: config.DefaultUserAgent,
	}

	client, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	_, err = client.Search(context.Background(), "Nonexistent")
	if err != ErrNoResults {
		t.Fatalf("Search() error = %v, want ErrNoResults", err)
	}
}
