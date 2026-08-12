package trakt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/berejant/movie-torrent-finder/internal/config"
	"github.com/berejant/movie-torrent-finder/internal/jellyfin"
)

const testJellyfinKey = "jellyfin-api-key"

// jellyfinConfigPath is the plugin-configuration endpoint. It is spelled out
// here rather than imported: this test is a stand-in for a real Jellyfin, and
// hard-coding the path is what makes it catch a change to it.
const jellyfinConfigPath = "/Plugins/4fe3201ed6ae4f2e8917e12bda571281/Configuration"

// fakeJellyfin serves the trakt plugin's configuration and applies saves to
// what it serves, so a second read sees what was written.
type fakeJellyfin struct {
	*httptest.Server

	document   string
	saves      int
	saveStatus int // 0 means 204
}

func newFakeJellyfin(t *testing.T, document string) *fakeJellyfin {
	t.Helper()

	fake := &fakeJellyfin{document: document}
	fake.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != jellyfinConfigPath {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Authorization"); got != `MediaBrowser Token="`+testJellyfinKey+`"` {
			http.Error(w, "bad auth header "+got, http.StatusUnauthorized)
			return
		}

		if r.Method == http.MethodPost {
			fake.saves++
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if fake.saveStatus != 0 {
				w.WriteHeader(fake.saveStatus)
				return
			}
			fake.document = string(body)
			w.WriteHeader(http.StatusNoContent)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, fake.document)
	}))
	t.Cleanup(fake.Close)

	return fake
}

// user reads a field back out of whatever the fake is currently serving.
func (f *fakeJellyfin) user(t *testing.T) map[string]json.RawMessage {
	t.Helper()

	var parsed struct {
		Users []map[string]json.RawMessage `json:"TraktUsers"`
	}
	if err := json.Unmarshal([]byte(f.document), &parsed); err != nil {
		t.Fatalf("stored document is not json: %v", err)
	}
	if len(parsed.Users) == 0 {
		t.Fatal("stored document has no users")
	}
	return parsed.Users[0]
}

// document builds a plugin configuration with the given expiry.
func document(expiry string) string {
	return fmt.Sprintf(`{"TraktUsers":[{
	  "AccessToken":"stored-access","RefreshToken":"stored-refresh",
	  "LinkedMbUserId":"c38a1b6c06074e4c8bbffc2d50e1f0e1",
	  "Scrobble":true,"LocationsExcluded":["/mnt/private"],
	  "AccessTokenExpiration":%q}]}`, expiry)
}

func rfc3339(t time.Time) string { return t.Format(time.RFC3339Nano) }

func newTokenSource(t *testing.T, jelly *fakeJellyfin, oauth *fakeOAuth, userID string) *TokenSource {
	t.Helper()

	client, err := jellyfin.NewClient(config.Jellyfin{
		Host:           jelly.URL,
		APIKey:         testJellyfinKey,
		UserID:         userID,
		TimeoutSeconds: 5,
	}, testLogger())
	if err != nil {
		t.Fatalf("jellyfin.NewClient() error: %v", err)
	}

	cfg := testConfig(oauth.URL)
	cfg.ClientSecret = "client-secret"

	return NewTokenSource(client, NewOAuth(cfg), userID, testLogger())
}

func TestTokenUsesAnUnexpiredToken(t *testing.T) {
	jelly := newFakeJellyfin(t, document(rfc3339(time.Now().Add(30*24*time.Hour))))
	oauth := newFakeOAuth(t)

	token, err := newTokenSource(t, jelly, oauth, "").Token(context.Background())
	if err != nil {
		t.Fatalf("Token() error: %v", err)
	}

	if token != "stored-access" {
		t.Errorf("Token() = %q, want stored-access", token)
	}
	if oauth.calls != 0 {
		t.Errorf("refreshed %d times, want 0", oauth.calls)
	}
	if jelly.saves != 0 {
		t.Errorf("saved %d times, want 0", jelly.saves)
	}
}

func TestTokenRefreshesAnExpiredToken(t *testing.T) {
	jelly := newFakeJellyfin(t, document(rfc3339(time.Now().Add(-time.Minute))))
	oauth := newFakeOAuth(t)

	before := time.Now()
	token, err := newTokenSource(t, jelly, oauth, "").Token(context.Background())
	if err != nil {
		t.Fatalf("Token() error: %v", err)
	}

	if token != "fresh-access" {
		t.Errorf("Token() = %q, want fresh-access", token)
	}
	if oauth.calls != 1 {
		t.Fatalf("refreshed %d times, want 1", oauth.calls)
	}
	if oauth.request["refresh_token"] != "stored-refresh" {
		t.Errorf("refreshed with %q, want stored-refresh", oauth.request["refresh_token"])
	}
	if jelly.saves != 1 {
		t.Fatalf("saved %d times, want 1", jelly.saves)
	}

	// The new pair reached jellyfin, and the plugin's own settings survived.
	stored := jelly.user(t)
	if got := string(stored["AccessToken"]); got != `"fresh-access"` {
		t.Errorf("stored AccessToken = %s", got)
	}
	if got := string(stored["RefreshToken"]); got != `"fresh-refresh"` {
		t.Errorf("stored RefreshToken = %s", got)
	}
	if got := string(stored["LocationsExcluded"]); got != `["/mnt/private"]` {
		t.Errorf("stored LocationsExcluded = %s, want the original value", got)
	}

	// The expiry is three quarters of the 90 days the fake issues.
	var expiry string
	if err := json.Unmarshal(stored["AccessTokenExpiration"], &expiry); err != nil {
		t.Fatalf("stored expiry is not a string: %v", err)
	}
	parsed, err := time.Parse(time.RFC3339Nano, expiry)
	if err != nil {
		t.Fatalf("stored expiry %q does not parse: %v", expiry, err)
	}
	want := before.Add(5832000 * time.Second)
	if parsed.Sub(want) > time.Minute || want.Sub(parsed) > time.Minute {
		t.Errorf("stored expiry = %v, want about %v", parsed, want)
	}
}

// A token dying during the sync would fail it halfway through, so it is renewed
// while it still has an hour to live.
func TestTokenRefreshesInsideTheBuffer(t *testing.T) {
	tests := map[string]struct {
		expiry time.Duration
		want   string
	}{
		"inside the buffer":  {expiry: refreshBuffer / 2, want: "fresh-access"},
		"outside the buffer": {expiry: refreshBuffer * 2, want: "stored-access"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			jelly := newFakeJellyfin(t, document(rfc3339(time.Now().Add(tc.expiry))))
			oauth := newFakeOAuth(t)

			token, err := newTokenSource(t, jelly, oauth, "").Token(context.Background())
			if err != nil {
				t.Fatalf("Token() error: %v", err)
			}
			if token != tc.want {
				t.Errorf("Token() = %q, want %q", token, tc.want)
			}
		})
	}
}

// A token with no readable expiry is used as found: the plugin records one for
// every token it writes, and trakt is the authority on whether it works.
func TestTokenUsesATokenWithNoExpiry(t *testing.T) {
	jelly := newFakeJellyfin(t, `{"TraktUsers":[{"AccessToken":"stored-access","RefreshToken":"r"}]}`)
	oauth := newFakeOAuth(t)

	token, err := newTokenSource(t, jelly, oauth, "").Token(context.Background())
	if err != nil {
		t.Fatalf("Token() error: %v", err)
	}
	if token != "stored-access" {
		t.Errorf("Token() = %q, want stored-access", token)
	}
	if oauth.calls != 0 {
		t.Errorf("refreshed %d times, want 0", oauth.calls)
	}
}

// The plugin refreshes the same grant. If it got there first its token is the
// live one and spending ours would kill it, so Refresh re-reads before acting.
func TestRefreshUsesATokenThePluginAlreadyRenewed(t *testing.T) {
	jelly := newFakeJellyfin(t, document(rfc3339(time.Now().Add(30*24*time.Hour))))
	oauth := newFakeOAuth(t)

	token, err := newTokenSource(t, jelly, oauth, "").Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh() error: %v", err)
	}

	if token != "stored-access" {
		t.Errorf("Refresh() = %q, want the stored token", token)
	}
	if oauth.calls != 0 {
		t.Errorf("refreshed %d times, want 0", oauth.calls)
	}
}

// A jellyfin that will not take the new token has not made it a bad token.
func TestRefreshReturnsTheTokenWhenTheSaveFails(t *testing.T) {
	jelly := newFakeJellyfin(t, document(rfc3339(time.Now().Add(-time.Minute))))
	jelly.saveStatus = http.StatusInternalServerError
	oauth := newFakeOAuth(t)

	token, err := newTokenSource(t, jelly, oauth, "").Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh() error: %v", err)
	}
	if token != "fresh-access" {
		t.Errorf("Refresh() = %q, want fresh-access", token)
	}
	if jelly.saves != 1 {
		t.Errorf("saved %d times, want 1", jelly.saves)
	}

	// The save failed, so the document must still show the old pair. Without
	// this the test cannot tell "save failed and we carried on" from "save
	// worked".
	stored := jelly.user(t)
	if got := string(stored["AccessToken"]); got != `"stored-access"` {
		t.Errorf("stored AccessToken = %s, want the document unchanged", got)
	}
}

// A save failure must not cost the grant: trakt already retired the refresh
// token the moment the fresh pair was issued, so spending it a second time
// would fail. The fresh pair is held in memory and stored, not re-requested,
// once jellyfin recovers.
func TestRefreshRecoversTheHeldPairOnTheNextCall(t *testing.T) {
	jelly := newFakeJellyfin(t, document(rfc3339(time.Now().Add(-time.Minute))))
	jelly.saveStatus = http.StatusInternalServerError
	oauth := newFakeOAuth(t)

	source := newTokenSource(t, jelly, oauth, "")

	token, err := source.Refresh(context.Background())
	if err != nil {
		t.Fatalf("first Refresh() error: %v", err)
	}
	if token != "fresh-access" {
		t.Errorf("first Refresh() = %q, want fresh-access", token)
	}

	jelly.saveStatus = 0

	token, err = source.Refresh(context.Background())
	if err != nil {
		t.Fatalf("second Refresh() error: %v", err)
	}
	if token != "fresh-access" {
		t.Errorf("second Refresh() = %q, want fresh-access", token)
	}

	// The held pair was stored, not a second grant spent against a refresh
	// token trakt had already retired.
	if oauth.calls != 1 {
		t.Errorf("refreshed %d times, want 1", oauth.calls)
	}

	stored := jelly.user(t)
	if got := string(stored["AccessToken"]); got != `"fresh-access"` {
		t.Errorf("stored AccessToken = %s", got)
	}
	if got := string(stored["RefreshToken"]); got != `"fresh-refresh"` {
		t.Errorf("stored RefreshToken = %s", got)
	}
	if got := string(stored["LocationsExcluded"]); got != `["/mnt/private"]` {
		t.Errorf("stored LocationsExcluded = %s, want the original value", got)
	}
}

func TestRefreshWithoutARefreshToken(t *testing.T) {
	jelly := newFakeJellyfin(t, `{"TraktUsers":[{"AccessToken":"stored-access","AccessTokenExpiration":"2020-01-01T00:00:00Z"}]}`)
	oauth := newFakeOAuth(t)

	_, err := newTokenSource(t, jelly, oauth, "").Refresh(context.Background())
	if err == nil {
		t.Fatal("Refresh() succeeded with no refresh token, want an error")
	}
	if oauth.calls != 0 {
		t.Errorf("refreshed %d times, want 0", oauth.calls)
	}
}

func TestTokenSelectsThePinnedUser(t *testing.T) {
	const twoUsers = `{"TraktUsers":[
	  {"AccessToken":"first","LinkedMbUserId":"11111111111111111111111111111111"},
	  {"AccessToken":"second","LinkedMbUserId":"c38a1b6c06074e4c8bbffc2d50e1f0e1"}
	]}`

	jelly := newFakeJellyfin(t, twoUsers)
	oauth := newFakeOAuth(t)

	token, err := newTokenSource(t, jelly, oauth, "c38a1b6c-0607-4e4c-8bbf-fc2d50e1f0e1").Token(context.Background())
	if err != nil {
		t.Fatalf("Token() error: %v", err)
	}
	if token != "second" {
		t.Errorf("Token() = %q, want second", token)
	}
}

// A rejected refresh token means the plugin has to be re-authorized; the error
// has to say so rather than look like a transient failure.
func TestTokenSurfacesARejectedRefresh(t *testing.T) {
	jelly := newFakeJellyfin(t, document(rfc3339(time.Now().Add(-time.Minute))))
	oauth := newFakeOAuth(t)
	oauth.status = http.StatusUnauthorized

	_, err := newTokenSource(t, jelly, oauth, "").Token(context.Background())
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("error = %v, want ErrUnauthorized", err)
	}
}
