package jellyfin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/berejant/movie-torrent-finder/internal/config"
)

const testAPIKey = "2f7ef9163f21454b8f4d9c9376eb09ec"

// fakeJellyfin serves the plugin-configuration endpoint and keeps whatever was
// saved back to it, so a test can assert on the exact bytes.
type fakeJellyfin struct {
	*httptest.Server

	document  string
	saved     []byte
	saveCalls int

	getStatus  int // 0 means 200 with the document
	saveStatus int // 0 means 204
}

func newFakeJellyfin(t *testing.T, document string) *fakeJellyfin {
	t.Helper()

	fake := &fakeJellyfin{document: document}
	fake.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != configPath {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Authorization"); got != `MediaBrowser Token="`+testAPIKey+`"` {
			http.Error(w, "bad auth header "+got, http.StatusUnauthorized)
			return
		}

		if r.Method == http.MethodPost {
			fake.saveCalls++
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			fake.saved = body
			if fake.saveStatus != 0 {
				w.WriteHeader(fake.saveStatus)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}

		if fake.getStatus != 0 {
			w.WriteHeader(fake.getStatus)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, fake.document)
	}))
	t.Cleanup(fake.Close)

	return fake
}

func testClient(t *testing.T, fake *fakeJellyfin) *Client {
	t.Helper()

	client, err := NewClient(config.Jellyfin{
		Host:           fake.URL,
		APIKey:         testAPIKey,
		TimeoutSeconds: 5,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewClient() error: %v", err)
	}
	return client
}

func TestTraktConfigReadsTheDocument(t *testing.T) {
	client := testClient(t, newFakeJellyfin(t, exampleConfig))

	cfg, err := client.TraktConfig(context.Background())
	if err != nil {
		t.Fatalf("TraktConfig() error: %v", err)
	}

	user, err := cfg.User("")
	if err != nil {
		t.Fatalf("User() error: %v", err)
	}
	if user.AccessToken() != "FkWYeJxODFyDNgjTShhmwUfAYWdChfhv" {
		t.Errorf("AccessToken() = %q", user.AccessToken())
	}
}

func TestSaveTraktConfigPostsTheWholeDocument(t *testing.T) {
	fake := newFakeJellyfin(t, exampleConfig)
	client := testClient(t, fake)

	cfg, err := client.TraktConfig(context.Background())
	if err != nil {
		t.Fatalf("TraktConfig() error: %v", err)
	}
	user, _ := cfg.User("")
	user.SetTokens("fresh-access", "fresh-refresh", user.Expiration())

	if err := client.SaveTraktConfig(context.Background(), cfg); err != nil {
		t.Fatalf("SaveTraktConfig() error: %v", err)
	}
	if fake.saveCalls != 1 {
		t.Fatalf("save called %d times, want 1", fake.saveCalls)
	}

	var saved struct {
		Users []map[string]json.RawMessage `json:"TraktUsers"`
	}
	if err := json.Unmarshal(fake.saved, &saved); err != nil {
		t.Fatalf("saved body is not the configuration document: %v", err)
	}
	if len(saved.Users) != 1 {
		t.Fatalf("saved %d users, want 1", len(saved.Users))
	}
	if got := string(saved.Users[0]["AccessToken"]); got != `"fresh-access"` {
		t.Errorf("saved AccessToken = %s", got)
	}
	// The fields this service does not own came back untouched.
	if got := string(saved.Users[0]["LocationsExcluded"]); got != `["/mnt/private"]` {
		t.Errorf("saved LocationsExcluded = %s, want the original value", got)
	}
}

// A rejected API key is worth telling apart: the fix is a Jellyfin setting, not
// anything to do with trakt.
func TestClientReportsUnauthorized(t *testing.T) {
	fake := newFakeJellyfin(t, exampleConfig)
	fake.getStatus = http.StatusUnauthorized
	client := testClient(t, fake)

	_, err := client.TraktConfig(context.Background())
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("error = %v, want ErrUnauthorized", err)
	}
}

func TestClientReportsBadStatuses(t *testing.T) {
	fake := newFakeJellyfin(t, exampleConfig)
	fake.getStatus = http.StatusInternalServerError
	client := testClient(t, fake)

	if _, err := client.TraktConfig(context.Background()); err == nil {
		t.Fatal("TraktConfig() succeeded, want an error")
	}

	fake.getStatus = 0
	fake.saveStatus = http.StatusInternalServerError
	cfg, err := client.TraktConfig(context.Background())
	if err != nil {
		t.Fatalf("TraktConfig() error: %v", err)
	}
	if err := client.SaveTraktConfig(context.Background(), cfg); err == nil {
		t.Fatal("SaveTraktConfig() succeeded, want an error")
	}
}

// Jellyfin answers 204 today; a future version answering 200 is not a failure.
func TestSaveAcceptsAny2xx(t *testing.T) {
	fake := newFakeJellyfin(t, exampleConfig)
	fake.saveStatus = http.StatusOK
	client := testClient(t, fake)

	cfg, err := client.TraktConfig(context.Background())
	if err != nil {
		t.Fatalf("TraktConfig() error: %v", err)
	}
	if err := client.SaveTraktConfig(context.Background(), cfg); err != nil {
		t.Fatalf("SaveTraktConfig() error: %v", err)
	}
}

func TestNewClientRejectsAnEmptyHost(t *testing.T) {
	_, err := NewClient(config.Jellyfin{APIKey: testAPIKey, TimeoutSeconds: 5},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		t.Fatal("NewClient() succeeded with no host, want an error")
	}
}
