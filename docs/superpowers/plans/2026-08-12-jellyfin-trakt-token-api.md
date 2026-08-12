# Trakt access token via the Jellyfin plugin API — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Read the trakt access token from Jellyfin's plugin-configuration HTTP API instead of a mounted `Trakt.xml`, and refresh an expired token in place instead of waiting for the plugin to do it.

**Architecture:** A new `internal/jellyfin` package wraps the plugin-configuration API and models the document as raw JSON so that saving it back cannot clobber fields this service does not own. A new `trakt.TokenSource` sits between the syncer and that API: it reads the token on every pass, refreshes through trakt's OAuth endpoint when the recorded expiry has passed, and stores the result back through the same API. The syncer additionally refreshes and retries once when trakt rejects a token it believed was valid.

**Tech Stack:** Go, standard library only (`net/http`, `encoding/json`, `log/slog`), `github.com/caarlos0/env/v10` for config binding, `net/http/httptest` for tests. No new dependencies.

**Spec:** `docs/superpowers/specs/2026-08-12-jellyfin-trakt-token-api-design.md`

## Global Constraints

- Module path is `github.com/berejant/movie-torrent-finder`. Imports use that prefix.
- The trakt plugin GUID is `4fe3201ed6ae4f2e8917e12bda571281`, stable across installs.
- Jellyfin auth header on every request: `Authorization: MediaBrowser Token="<api key>"` (the key is quoted).
- Trakt refresh request, field for field as the plugin sends it: `POST {TRAKT_BASE_URL}/oauth/token` with a JSON body of `client_id`, `client_secret`, `redirect_uri` = `urn:ietf:wg:oauth:2.0:oob`, `refresh_token`, `grant_type` = `refresh_token`.
- A refreshed token's recorded expiry is `now + expires_in * 3/4`, matching the plugin's `ExpirationWithBuffer`.
- Never decode the plugin's users into a typed struct. Unknown keys must round-trip byte-identical.
- Every commit leaves `go build ./... && go vet ./... && go test ./...` green.
- Comments explain **why**, not what — match the density and voice of the surrounding files.

## File Structure

| File | Responsibility |
|---|---|
| `internal/config/config.go` (modify) | Add `Jellyfin`, add `Trakt.ClientSecret`, drop `Trakt.TokenFile`, validate, redact |
| `internal/jellyfin/config.go` (create) | The plugin-configuration document: user selection, the three token fields, .NET timestamps |
| `internal/jellyfin/client.go` (create) | HTTP: GET and POST `/Plugins/{guid}/Configuration` |
| `internal/trakt/oauth.go` (create) | The trakt OAuth refresh call |
| `internal/trakt/token.go` (rewrite) | `TokenSource`: read, decide, refresh, store |
| `internal/trakt/sync.go` (modify) | Take the token from `TokenSource`; refresh and retry once on 401 |
| `cmd/server/main.go` (modify) | Wiring |

---

### Task 1: Configuration for Jellyfin and the trakt client secret

Adds the new variables and their validation without removing anything, so the build stays green. `TRAKT_TOKEN_FILE` is removed in Task 6, once nothing reads it.

**Files:**
- Modify: `internal/config/config.go` (the `Trakt` struct ~`:67`, `Validate` ~`:446`, `LogValue` ~`:568`)
- Test: `internal/config/config_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `config.Jellyfin{Host, APIKey, UserID string; TimeoutSeconds int}` with `Timeout() time.Duration`; `config.Config.Jellyfin`; `config.Trakt.ClientSecret string`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/config/config_test.go`:

```go
func TestLoadJellyfinDefaults(t *testing.T) {
	baseEnv(t)
	t.Setenv("TRACKERS", "toloka")
	t.Setenv("TRACKER_TOLOKA_LOGIN", "tester")
	t.Setenv("TRACKER_TOLOKA_PASSWORD", "secret")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	// Nothing about jellyfin is required while the sync is off.
	if got := cfg.Jellyfin.Timeout(); got != 30*time.Second {
		t.Errorf("Jellyfin.Timeout() = %v, want 30s", got)
	}
}

func TestLoadJellyfinEnabled(t *testing.T) {
	baseEnv(t)
	t.Setenv("TRACKERS", "toloka")
	t.Setenv("TRACKER_TOLOKA_LOGIN", "tester")
	t.Setenv("TRACKER_TOLOKA_PASSWORD", "secret")
	t.Setenv("TRAKT_ENABLED", "true")
	t.Setenv("TRAKT_CLIENT_ID", "client-id")
	t.Setenv("TRAKT_CLIENT_SECRET", "client-secret")
	t.Setenv("TRAKT_TOKEN_FILE", "/config/Trakt.xml")
	t.Setenv("JELLYFIN_HOST", "http://jellyfin:8096")
	t.Setenv("JELLYFIN_API_KEY", "api-key")
	t.Setenv("JELLYFIN_USER_ID", "c38a1b6c06074e4c8bbffc2d50e1f0e1")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.Trakt.ClientSecret != "client-secret" {
		t.Errorf("Trakt.ClientSecret = %q, want client-secret", cfg.Trakt.ClientSecret)
	}
	if cfg.Jellyfin.Host != "http://jellyfin:8096" || cfg.Jellyfin.APIKey != "api-key" {
		t.Errorf("Jellyfin = %+v, want the host and api key bound", cfg.Jellyfin)
	}
	if cfg.Jellyfin.UserID != "c38a1b6c06074e4c8bbffc2d50e1f0e1" {
		t.Errorf("Jellyfin.UserID = %q, want the linked user id bound", cfg.Jellyfin.UserID)
	}
}

// Each variable the sync needs is named on its own, so a half-finished
// configuration says which half is missing.
func TestLoadRejectsIncompleteJellyfin(t *testing.T) {
	// name of the variable left unset -> the text the error must contain
	tests := map[string]string{
		"TRAKT_CLIENT_SECRET": "TRAKT_CLIENT_SECRET",
		"JELLYFIN_HOST":       "JELLYFIN_HOST",
		"JELLYFIN_API_KEY":    "JELLYFIN_API_KEY",
	}

	for unset, want := range tests {
		t.Run("without "+unset, func(t *testing.T) {
			baseEnv(t)
			t.Setenv("TRACKERS", "toloka")
			t.Setenv("TRACKER_TOLOKA_LOGIN", "tester")
			t.Setenv("TRACKER_TOLOKA_PASSWORD", "secret")
			t.Setenv("TRAKT_ENABLED", "true")
			t.Setenv("TRAKT_CLIENT_ID", "client-id")
			t.Setenv("TRAKT_CLIENT_SECRET", "client-secret")
			t.Setenv("TRAKT_TOKEN_FILE", "/config/Trakt.xml")
			t.Setenv("JELLYFIN_HOST", "http://jellyfin:8096")
			t.Setenv("JELLYFIN_API_KEY", "api-key")
			t.Setenv(unset, "")

			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("error = %v, want it to name %s", err, want)
			}
		})
	}
}

func TestLoadRejectsABadJellyfinHost(t *testing.T) {
	tests := map[string]string{
		"no scheme": "jellyfin:8096/x",
		"ftp":       "ftp://jellyfin:8096",
		"no host":   "http://",
	}

	for name, host := range tests {
		t.Run(name, func(t *testing.T) {
			baseEnv(t)
			t.Setenv("TRACKERS", "toloka")
			t.Setenv("TRACKER_TOLOKA_LOGIN", "tester")
			t.Setenv("TRACKER_TOLOKA_PASSWORD", "secret")
			t.Setenv("TRAKT_ENABLED", "true")
			t.Setenv("TRAKT_CLIENT_ID", "client-id")
			t.Setenv("TRAKT_CLIENT_SECRET", "client-secret")
			t.Setenv("TRAKT_TOKEN_FILE", "/config/Trakt.xml")
			t.Setenv("JELLYFIN_API_KEY", "api-key")
			t.Setenv("JELLYFIN_HOST", host)

			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), "JELLYFIN_HOST") {
				t.Fatalf("error = %v, want it to name JELLYFIN_HOST", err)
			}
		})
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/config/ -run 'Jellyfin' -v`
Expected: compile failure — `cfg.Jellyfin` undefined, `Trakt.ClientSecret` undefined.

- [ ] **Step 3: Add the fields**

In `internal/config/config.go`, add to `Config` right after the `Trakt` field:

```go
	Jellyfin Jellyfin `envPrefix:"JELLYFIN_"`
```

Add `ClientSecret` to `Trakt`, directly under `ClientID`:

```go
	// ClientSecret is the matching secret. It is only used to refresh the
	// access token, and it must belong to the same application as the refresh
	// token the plugin stored — for a stock plugin install, that is the
	// plugin's own compiled-in pair.
	ClientSecret string `env:"CLIENT_SECRET"`
```

Add the new struct after the `Trakt` methods (`Interval`, `Timeout`):

```go
// Jellyfin is the media server whose trakt plugin holds the OAuth tokens. The
// plugin's configuration is read over Jellyfin's API rather than off disk, so
// this service needs no access to the plugin's config volume — and, unlike a
// read-only file, it can write a refreshed token back.
type Jellyfin struct {
	Host   string `env:"HOST"`
	APIKey string `env:"API_KEY"`

	// UserID pins which linked media-server user's trakt account to use, by
	// LinkedMbUserId. Unset means the first entry carrying an access token,
	// which is the whole answer on a single-user install.
	UserID string `env:"USER_ID"`

	TimeoutSeconds int `env:"TIMEOUT_SECONDS" envDefault:"30"`
}

// Timeout returns the per-request timeout for the Jellyfin API.
func (j Jellyfin) Timeout() time.Duration {
	return time.Duration(j.TimeoutSeconds) * time.Second
}
```

- [ ] **Step 4: Add the validation**

Inside `Validate`, in the `if c.Trakt.Enabled` block, right after the existing `TRAKT_CLIENT_ID` check:

```go
		if strings.TrimSpace(c.Trakt.ClientSecret) == "" {
			problems = append(problems, "TRAKT_CLIENT_SECRET must be set when TRAKT_ENABLED is true")
		}
		if strings.TrimSpace(c.Jellyfin.APIKey) == "" {
			problems = append(problems, "JELLYFIN_API_KEY must be set when TRAKT_ENABLED is true")
		}

		host, err := url.Parse(strings.TrimRight(c.Jellyfin.Host, "/"))
		switch {
		case strings.TrimSpace(c.Jellyfin.Host) == "":
			problems = append(problems, "JELLYFIN_HOST must be set when TRAKT_ENABLED is true")
		case err != nil:
			problems = append(problems, fmt.Sprintf("JELLYFIN_HOST is not a valid URL: %v", err))
		case host.Scheme != "http" && host.Scheme != "https":
			problems = append(problems, "JELLYFIN_HOST must use http or https")
		case host.Host == "":
			problems = append(problems, "JELLYFIN_HOST must include a host")
		}

		if c.Jellyfin.TimeoutSeconds < 1 {
			problems = append(problems, fmt.Sprintf("JELLYFIN_TIMEOUT_SECONDS must be >= 1, got %d", c.Jellyfin.TimeoutSeconds))
		}
```

- [ ] **Step 5: Add the log fields**

In `LogValue`, add to the `trakt` group after `client_id`:

```go
			slog.String("client_secret", redact(c.Trakt.ClientSecret)),
```

and add a group after the whole `trakt` group:

```go
		slog.Group("jellyfin",
			slog.String("host", c.Jellyfin.Host),
			slog.String("api_key", redact(c.Jellyfin.APIKey)),
			slog.String("user_id", redact(c.Jellyfin.UserID)),
			slog.Int("timeout_seconds", c.Jellyfin.TimeoutSeconds),
		),
```

- [ ] **Step 6: Run the tests**

Run: `go test ./internal/config/ -v`
Expected: PASS, including the pre-existing trakt tests.

- [ ] **Step 7: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "Add jellyfin API settings and the trakt client secret"
```

---

### Task 2: The plugin-configuration document

The pure part of `internal/jellyfin`: parsing, user selection, and the three fields this service owns. No HTTP yet, so it is testable on literal JSON.

**Files:**
- Create: `internal/jellyfin/config.go`
- Test: `internal/jellyfin/config_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `jellyfin.TraktConfig` with `Users []map[string]json.RawMessage` (JSON key `TraktUsers`)
  - `func (c *TraktConfig) User(linkedMbUserID string) (*TraktUser, error)`
  - `func (u *TraktUser) AccessToken() string`, `RefreshToken() string`, `Expiration() time.Time`, `SetTokens(access, refresh string, expiry time.Time)`

- [ ] **Step 1: Write the failing test**

Create `internal/jellyfin/config_test.go`:

```go
package jellyfin

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// exampleConfig is the document a real Jellyfin returns, trimmed to the fields
// that matter here plus a few that do not — those are what the round-trip test
// is about.
const exampleConfig = `{"TraktUsers":[{
  "AccessToken":"FkWYeJxODFyDNgjTShhmwUfAYWdChfhv",
  "RefreshToken":"JkVVKTacvIdxCZgUCRdDiJulIFbxwDsE",
  "LinkedMbUserId":"c38a1b6c06074e4c8bbffc2d50e1f0e1",
  "SkipUnwatchedImportFromTrakt":true,
  "LocationsExcluded":["/mnt/private"],
  "Scrobble":true,
  "AccessTokenExpiration":"2026-08-17T00:40:08.1168339+03:00",
  "DontRemoveItemFromTrakt":true
}]}`

func parseConfig(t *testing.T, document string) *TraktConfig {
	t.Helper()

	var cfg TraktConfig
	if err := json.Unmarshal([]byte(document), &cfg); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	return &cfg
}

func TestUserReadsTheTokenFields(t *testing.T) {
	user, err := parseConfig(t, exampleConfig).User("")
	if err != nil {
		t.Fatalf("User() error: %v", err)
	}

	if user.AccessToken() != "FkWYeJxODFyDNgjTShhmwUfAYWdChfhv" {
		t.Errorf("AccessToken() = %q", user.AccessToken())
	}
	if user.RefreshToken() != "JkVVKTacvIdxCZgUCRdDiJulIFbxwDsE" {
		t.Errorf("RefreshToken() = %q", user.RefreshToken())
	}

	want := time.Date(2026, 8, 16, 21, 40, 8, 116833900, time.UTC)
	if !user.Expiration().Equal(want) {
		t.Errorf("Expiration() = %v, want %v", user.Expiration(), want)
	}
}

// The plugin owns this document. Writing back a field it knows and this service
// does not would reset the operator's settings, so everything untouched has to
// survive the round trip exactly.
func TestSetTokensPreservesUnknownFields(t *testing.T) {
	cfg := parseConfig(t, exampleConfig)
	user, err := cfg.User("")
	if err != nil {
		t.Fatalf("User() error: %v", err)
	}

	expiry := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	user.SetTokens("new-access", "new-refresh", expiry)

	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}

	var got struct {
		Users []map[string]json.RawMessage `json:"TraktUsers"`
	}
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("unmarshal saved config: %v", err)
	}
	if len(got.Users) != 1 {
		t.Fatalf("saved %d users, want 1", len(got.Users))
	}

	for field, want := range map[string]string{
		"AccessToken":                  `"new-access"`,
		"RefreshToken":                 `"new-refresh"`,
		"LinkedMbUserId":               `"c38a1b6c06074e4c8bbffc2d50e1f0e1"`,
		"SkipUnwatchedImportFromTrakt": `true`,
		"LocationsExcluded":            `["/mnt/private"]`,
		"Scrobble":                     `true`,
		"DontRemoveItemFromTrakt":      `true`,
	} {
		if got := string(got.Users[0][field]); got != want {
			t.Errorf("saved %s = %s, want %s", field, got, want)
		}
	}

	// The expiry is written in the layout the plugin reads back.
	if got := string(got.Users[0]["AccessTokenExpiration"]); got != `"2026-11-01T12:00:00Z"` {
		t.Errorf("saved AccessTokenExpiration = %s", got)
	}
}

func TestSetTokensRoundTripsThroughExpiration(t *testing.T) {
	cfg := parseConfig(t, exampleConfig)
	user, _ := cfg.User("")

	want := time.Date(2026, 11, 1, 12, 30, 45, 123456700, time.UTC)
	user.SetTokens("a", "b", want)

	if !user.Expiration().Equal(want) {
		t.Errorf("Expiration() = %v, want %v", user.Expiration(), want)
	}
}

// The plugin writes the media-server id undashed in JSON, while Jellyfin's own
// UI and URLs show it dashed; an operator will copy either one.
func TestUserSelectsByLinkedMbUserID(t *testing.T) {
	const document = `{"TraktUsers":[
	  {"AccessToken":"first","LinkedMbUserId":"11111111111111111111111111111111"},
	  {"AccessToken":"second","LinkedMbUserId":"c38a1b6c06074e4c8bbffc2d50e1f0e1"}
	]}`

	for _, pinned := range []string{
		"c38a1b6c06074e4c8bbffc2d50e1f0e1",
		"c38a1b6c-0607-4e4c-8bbf-fc2d50e1f0e1",
		"C38A1B6C06074E4C8BBFFC2D50E1F0E1",
	} {
		user, err := parseConfig(t, document).User(pinned)
		if err != nil {
			t.Fatalf("User(%q) error: %v", pinned, err)
		}
		if user.AccessToken() != "second" {
			t.Errorf("User(%q).AccessToken() = %q, want second", pinned, user.AccessToken())
		}
	}
}

// Unpinned, the first usable entry wins: the list identifies media-server
// users, and a single-user install has nothing to choose between.
func TestUserSkipsEntriesWithoutAToken(t *testing.T) {
	const document = `{"TraktUsers":[{"AccessToken":""},{"AccessToken":"second"}]}`

	user, err := parseConfig(t, document).User("")
	if err != nil {
		t.Fatalf("User() error: %v", err)
	}
	if user.AccessToken() != "second" {
		t.Errorf("AccessToken() = %q, want second", user.AccessToken())
	}
}

// A pinned id that is not there must not silently fall back to another user's
// account: that would sync the wrong watchlist.
func TestUserErrors(t *testing.T) {
	tests := map[string]struct {
		document string
		pinned   string
		want     string
	}{
		"no users":      {document: `{"TraktUsers":[]}`, want: "no linked users"},
		"missing field": {document: `{}`, want: "no linked users"},
		"no token":      {document: `{"TraktUsers":[{"AccessToken":""}]}`, want: "no linked user has an access token"},
		"unknown id": {
			document: `{"TraktUsers":[{"AccessToken":"a","LinkedMbUserId":"aaaa"}]}`,
			pinned:   "bbbb",
			want:     "bbbb",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := parseConfig(t, tc.document).User(tc.pinned)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// A token with no readable expiry is still a usable token: trakt decides.
func TestExpirationToleratesJunk(t *testing.T) {
	tests := map[string]string{
		"missing":     `{"TraktUsers":[{"AccessToken":"a"}]}`,
		"empty":       `{"TraktUsers":[{"AccessToken":"a","AccessTokenExpiration":""}]}`,
		"not a date":  `{"TraktUsers":[{"AccessToken":"a","AccessTokenExpiration":"soon"}]}`,
		"not a string": `{"TraktUsers":[{"AccessToken":"a","AccessTokenExpiration":42}]}`,
	}

	for name, document := range tests {
		t.Run(name, func(t *testing.T) {
			user, err := parseConfig(t, document).User("")
			if err != nil {
				t.Fatalf("User() error: %v", err)
			}
			if !user.Expiration().IsZero() {
				t.Errorf("Expiration() = %v, want the zero time", user.Expiration())
			}
		})
	}
}

// .NET's serializers write any of these, depending on version and kind.
func TestExpirationAcceptsDotNetLayouts(t *testing.T) {
	tests := map[string]time.Time{
		"2026-08-17T00:40:08.1168339+03:00": time.Date(2026, 8, 16, 21, 40, 8, 116833900, time.UTC),
		"2026-08-17T00:40:08Z":              time.Date(2026, 8, 17, 0, 40, 8, 0, time.UTC),
		"2026-08-17T00:40:08.1168339":       time.Date(2026, 8, 17, 0, 40, 8, 116833900, time.UTC),
		"2026-08-17T00:40:08":               time.Date(2026, 8, 17, 0, 40, 8, 0, time.UTC),
	}

	for value, want := range tests {
		t.Run(value, func(t *testing.T) {
			document := `{"TraktUsers":[{"AccessToken":"a","AccessTokenExpiration":"` + value + `"}]}`
			user, err := parseConfig(t, document).User("")
			if err != nil {
				t.Fatalf("User() error: %v", err)
			}
			if !user.Expiration().Equal(want) {
				t.Errorf("Expiration() = %v, want %v", user.Expiration(), want)
			}
		})
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/jellyfin/ -v`
Expected: build failure — no non-test Go files / `TraktConfig` undefined.

- [ ] **Step 3: Write the implementation**

Create `internal/jellyfin/config.go`:

```go
// Package jellyfin reads and writes the configuration of the Emby/Jellyfin
// trakt plugin over Jellyfin's HTTP API.
//
// The plugin owns the OAuth grant for a trakt account; this service borrows the
// access token out of it and, when the token has expired, puts a fresh one
// back. Nothing else in the document belongs to this service.
package jellyfin

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The three fields this service reads and writes, plus the one it selects on.
// Everything else in a user object is carried through untouched.
const (
	fieldAccessToken  = "AccessToken"
	fieldRefreshToken = "RefreshToken"
	fieldExpiration   = "AccessTokenExpiration"
	fieldLinkedUser   = "LinkedMbUserId"
)

// TraktConfig is the trakt plugin's configuration document.
//
// The users are held as raw JSON rather than as a struct because a save is a
// read-modify-write of a document another application owns: decoding into a
// struct would drop every key this service does not know — Scrobble,
// LocationsExcluded, whatever a later plugin version adds — and write them back
// as absent, resetting the operator's settings.
type TraktConfig struct {
	Users []map[string]json.RawMessage `json:"TraktUsers"`
}

// TraktUser is one linked media-server user's trakt account. It points into the
// document it came from, so SetTokens edits that document in place.
type TraktUser struct {
	fields map[string]json.RawMessage
}

// User picks the account to use. An empty linkedMbUserID means the first entry
// carrying an access token, which is the whole answer on a single-user install;
// otherwise the entry has to match, because falling back to another user would
// quietly sync the wrong watchlist.
func (c *TraktConfig) User(linkedMbUserID string) (*TraktUser, error) {
	if len(c.Users) == 0 {
		return nil, errors.New("jellyfin: the trakt plugin has no linked users; authorize it first")
	}

	pinned := normalizeUserID(linkedMbUserID)
	for _, fields := range c.Users {
		user := &TraktUser{fields: fields}

		if pinned != "" {
			if normalizeUserID(user.str(fieldLinkedUser)) == pinned {
				return user, nil
			}
			continue
		}
		if user.AccessToken() != "" {
			return user, nil
		}
	}

	if pinned != "" {
		return nil, fmt.Errorf("jellyfin: the trakt plugin has no linked user %q", linkedMbUserID)
	}
	return nil, errors.New("jellyfin: no linked user has an access token; authorize the trakt plugin first")
}

// AccessToken is the trakt access token, empty when the plugin has none.
func (u *TraktUser) AccessToken() string { return u.str(fieldAccessToken) }

// RefreshToken is the token that buys a new access token.
func (u *TraktUser) RefreshToken() string { return u.str(fieldRefreshToken) }

// Expiration is the expiry the plugin last recorded, or the zero time when the
// field is absent or unreadable. A zero expiry is not an error: trakt is the
// authority on whether a token still works.
func (u *TraktUser) Expiration() time.Time {
	value := u.str(fieldExpiration)
	if value == "" {
		return time.Time{}
	}

	for _, layout := range expirationLayouts {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}

// SetTokens replaces the three fields this service owns and leaves the rest of
// the document alone.
func (u *TraktUser) SetTokens(access, refresh string, expiry time.Time) {
	u.fields[fieldAccessToken] = jsonString(access)
	u.fields[fieldRefreshToken] = jsonString(refresh)
	u.fields[fieldExpiration] = jsonString(expiry.Format(expirationLayout))
}

// str reads a string field, treating a missing field and a field of the wrong
// type alike: this service is a guest in someone else's document, and a
// surprise there should not be fatal.
func (u *TraktUser) str(name string) string {
	raw, ok := u.fields[name]
	if !ok {
		return ""
	}

	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return strings.TrimSpace(value)
}

// expirationLayouts are the timestamp shapes .NET's serializers write: an
// offset, a UTC "Z", or no zone at all, which is read as UTC.
var expirationLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05.999999999",
	"2006-01-02T15:04:05",
}

// expirationLayout is what this service writes. It is the first of the layouts
// above, so the plugin and this service always read back each other's work.
const expirationLayout = time.RFC3339Nano

// normalizeUserID strips what is only formatting in a media-server id: the
// plugin writes the guid undashed in JSON while Jellyfin's URLs show it dashed,
// and an operator will copy whichever one they are looking at.
func normalizeUserID(value string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(value), "-", ""))
}

// jsonString encodes a string for storage in the raw document. Marshalling a
// string cannot fail, so the error is not worth propagating to every caller.
func jsonString(value string) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/jellyfin/ -v`
Expected: PASS.

- [ ] **Step 5: Vet and commit**

```bash
go vet ./internal/jellyfin/
git add internal/jellyfin/
git commit -m "Add the jellyfin trakt plugin configuration document"
```

---

### Task 3: The Jellyfin API client

**Files:**
- Create: `internal/jellyfin/client.go`
- Test: `internal/jellyfin/client_test.go`

**Interfaces:**
- Consumes: `jellyfin.TraktConfig` (Task 2), `config.Jellyfin` (Task 1).
- Produces: `func NewClient(cfg config.Jellyfin, logger *slog.Logger) (*Client, error)`, `func (c *Client) TraktConfig(ctx context.Context) (*TraktConfig, error)`, `func (c *Client) SaveTraktConfig(ctx context.Context, cfg *TraktConfig) error`, `var ErrUnauthorized error`, `const configPath`.

- [ ] **Step 1: Write the failing test**

Create `internal/jellyfin/client_test.go`:

```go
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
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/jellyfin/ -run 'Client|TraktConfig|Save' -v`
Expected: compile failure — `configPath`, `NewClient`, `Client` undefined.

- [ ] **Step 3: Write the implementation**

Create `internal/jellyfin/client.go`:

```go
package jellyfin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/berejant/movie-torrent-finder/internal/config"
)

// pluginID is the trakt plugin's GUID. A plugin's id is generated once, for the
// plugin and not for the install, so hard-coding it is correct.
const pluginID = "4fe3201ed6ae4f2e8917e12bda571281"

// configPath serves the plugin's configuration: GET reads it, POST replaces it.
const configPath = "/Plugins/" + pluginID + "/Configuration"

// maxResponseSize caps the configuration document, which is a few kilobytes at
// most. It bounds the damage from pointing JELLYFIN_HOST at the wrong service.
const maxResponseSize = 4 << 20

// ErrUnauthorized means Jellyfin rejected the API key. It is worth its own
// error: the fix is a Jellyfin setting, and nothing about the trakt token.
var ErrUnauthorized = errors.New("jellyfin: api key rejected")

// Client talks to Jellyfin's plugin-configuration API.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
	logger  *slog.Logger
}

// NewClient builds a client. It does not contact Jellyfin.
func NewClient(cfg config.Jellyfin, logger *slog.Logger) (*Client, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.Host), "/")
	if base == "" {
		return nil, errors.New("jellyfin: JELLYFIN_HOST is empty")
	}
	if _, err := url.Parse(base); err != nil {
		return nil, fmt.Errorf("jellyfin: parse host: %w", err)
	}

	return &Client{
		baseURL: base,
		apiKey:  strings.TrimSpace(cfg.APIKey),
		http:    &http.Client{Timeout: cfg.Timeout()},
		logger:  logger.With("component", "jellyfin"),
	}, nil
}

// TraktConfig reads the trakt plugin's configuration.
func (c *Client) TraktConfig(ctx context.Context) (*TraktConfig, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+configPath, nil)
	if err != nil {
		return nil, fmt.Errorf("jellyfin: build request: %w", err)
	}
	req.Header.Set("Authorization", c.authorization())
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jellyfin: GET %s: %w", configPath, err)
	}
	defer drain(resp)

	if err := checkStatus(resp, http.MethodGet); err != nil {
		return nil, err
	}

	var cfg TraktConfig
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseSize)).Decode(&cfg); err != nil {
		return nil, fmt.Errorf("jellyfin: decode plugin configuration: %w", err)
	}
	return &cfg, nil
}

// SaveTraktConfig writes the configuration back. The whole document is sent:
// the endpoint replaces it rather than patching it.
func (c *Client) SaveTraktConfig(ctx context.Context, cfg *TraktConfig) error {
	body, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("jellyfin: encode plugin configuration: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+configPath, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("jellyfin: build request: %w", err)
	}
	req.Header.Set("Authorization", c.authorization())
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("jellyfin: POST %s: %w", configPath, err)
	}
	defer drain(resp)

	return checkStatus(resp, http.MethodPost)
}

// authorization is Jellyfin's API-key scheme. The key is quoted.
func (c *Client) authorization() string {
	return fmt.Sprintf("MediaBrowser Token=%q", c.apiKey)
}

// checkStatus maps a response to an error. Any 2xx is a success: the read
// answers 200 and the save 204, and a later version is free to pick another.
func checkStatus(resp *http.Response, method string) error {
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w (%s %s, status %d)", ErrUnauthorized, method, configPath, resp.StatusCode)
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return fmt.Errorf("jellyfin: %s %s: unexpected status %d", method, configPath, resp.StatusCode)
	}
	return nil
}

// drain empties and closes the body so the connection can be reused.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseSize))
	_ = resp.Body.Close()
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/jellyfin/ -v`
Expected: PASS, both files' tests.

- [ ] **Step 5: Vet and commit**

```bash
go vet ./internal/jellyfin/
git add internal/jellyfin/
git commit -m "Add the jellyfin plugin configuration API client"
```

---

### Task 4: The trakt OAuth refresh call

**Files:**
- Create: `internal/trakt/oauth.go`
- Test: `internal/trakt/oauth_test.go`

**Interfaces:**
- Consumes: `config.Trakt` (with `ClientSecret` from Task 1), the existing `trakt.ErrUnauthorized` and `apiVersion` in `internal/trakt/client.go`.
- Produces: `type AccessToken struct { Token, RefreshToken string; ExpiresIn int }` with `Expiry(now time.Time) time.Time`; `func NewOAuth(cfg config.Trakt) *OAuth`; `func (o *OAuth) Refresh(ctx context.Context, refreshToken string) (AccessToken, error)`.

- [ ] **Step 1: Write the failing test**

Create `internal/trakt/oauth_test.go`:

```go
package trakt

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// fakeOAuth serves trakt's token endpoint and keeps the request it was sent.
type fakeOAuth struct {
	*httptest.Server

	request map[string]string
	calls   int
	status  int // 0 means 200 with a fresh token
	body    string
}

func newFakeOAuth(t *testing.T) *fakeOAuth {
	t.Helper()

	fake := &fakeOAuth{
		body: `{"access_token":"fresh-access","refresh_token":"fresh-refresh",` +
			`"expires_in":7776000,"token_type":"bearer","scope":"public"}`,
	}
	fake.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != oauthTokenPath {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}

		fake.calls++
		if err := json.NewDecoder(r.Body).Decode(&fake.request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if fake.status != 0 {
			w.WriteHeader(fake.status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, fake.body)
	}))
	t.Cleanup(fake.Close)

	return fake
}

func TestOAuthRefreshSendsThePluginsRequest(t *testing.T) {
	fake := newFakeOAuth(t)

	cfg := testConfig(fake.URL)
	cfg.ClientSecret = "client-secret"

	issued, err := NewOAuth(cfg).Refresh(context.Background(), "old-refresh")
	if err != nil {
		t.Fatalf("Refresh() error: %v", err)
	}

	// Field for field what the Emby/Jellyfin plugin sends: the grant was issued
	// against these values, so a refresh has to name them too.
	for field, want := range map[string]string{
		"client_id":     testClientID,
		"client_secret": "client-secret",
		"redirect_uri":  "urn:ietf:wg:oauth:2.0:oob",
		"refresh_token": "old-refresh",
		"grant_type":    "refresh_token",
	} {
		if got := fake.request[field]; got != want {
			t.Errorf("request %s = %q, want %q", field, got, want)
		}
	}

	if issued.Token != "fresh-access" || issued.RefreshToken != "fresh-refresh" {
		t.Errorf("issued = %+v, want the fresh pair", issued)
	}
	if issued.ExpiresIn != 7776000 {
		t.Errorf("ExpiresIn = %d, want 7776000", issued.ExpiresIn)
	}
}

// Three quarters of the token's life, the same margin the plugin leaves: trakt
// does not say when a refresh token dies, so both sides renew early.
func TestAccessTokenExpiry(t *testing.T) {
	now := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	issued := AccessToken{ExpiresIn: 7776000} // 90 days

	want := now.Add(5832000 * time.Second) // 67.5 days
	if got := issued.Expiry(now); !got.Equal(want) {
		t.Errorf("Expiry() = %v, want %v", got, want)
	}
}

func TestOAuthRefreshRejection(t *testing.T) {
	tests := map[string]struct {
		status int
		body   string
		wantIs error
	}{
		"spent refresh token": {status: http.StatusUnauthorized, wantIs: ErrUnauthorized},
		"bad request":         {status: http.StatusBadRequest, wantIs: ErrUnauthorized},
		"trakt is down":       {status: http.StatusBadGateway},
		"empty token":         {body: `{"access_token":"","refresh_token":"x","expires_in":1}`},
		"not json":            {body: `<html>`},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			fake := newFakeOAuth(t)
			fake.status = tc.status
			if tc.body != "" {
				fake.body = tc.body
			}

			cfg := testConfig(fake.URL)
			cfg.ClientSecret = "client-secret"

			_, err := NewOAuth(cfg).Refresh(context.Background(), "old-refresh")
			if err == nil {
				t.Fatal("Refresh() succeeded, want an error")
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Fatalf("error = %v, want it to wrap %v", err, tc.wantIs)
			}
		})
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/trakt/ -run OAuth -v`
Expected: compile failure — `oauthTokenPath`, `NewOAuth`, `AccessToken` undefined.

- [ ] **Step 3: Write the implementation**

Create `internal/trakt/oauth.go`:

```go
package trakt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/berejant/movie-torrent-finder/internal/config"
)

// oauthTokenPath exchanges a refresh token for a new access token.
const oauthTokenPath = "/oauth/token"

// oauthRedirectURI is the out-of-band redirect the Emby/Jellyfin trakt plugin
// authorized with. The grant being refreshed was issued against it, so the
// refresh has to name it too.
const oauthRedirectURI = "urn:ietf:wg:oauth:2.0:oob"

// AccessToken is what trakt hands back for a refresh.
type AccessToken struct {
	Token        string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	// ExpiresIn is the lifetime in seconds; trakt currently issues 90 days.
	ExpiresIn int `json:"expires_in"`
}

// Expiry is when the token should be treated as spent: three quarters of the
// way through its life. That is the margin the Emby/Jellyfin plugin records,
// and matching it keeps the two sides from disagreeing about the same token.
// The margin exists because trakt documents when the access token expires but
// not when the refresh token does.
func (a AccessToken) Expiry(now time.Time) time.Time {
	return now.Add(time.Duration(a.ExpiresIn) * time.Second * 3 / 4)
}

// OAuth refreshes the access token held by the Emby/Jellyfin trakt plugin.
type OAuth struct {
	baseURL      string
	clientID     string
	clientSecret string
	http         *http.Client
}

// NewOAuth builds the refresher. It does not contact trakt.
func NewOAuth(cfg config.Trakt) *OAuth {
	return &OAuth{
		baseURL:      strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/"),
		clientID:     strings.TrimSpace(cfg.ClientID),
		clientSecret: strings.TrimSpace(cfg.ClientSecret),
		http:         &http.Client{Timeout: cfg.Timeout()},
	}
}

// Refresh exchanges refreshToken for a new pair. Trakt invalidates the old
// refresh token the moment this succeeds, so the result must be stored.
func (o *OAuth) Refresh(ctx context.Context, refreshToken string) (AccessToken, error) {
	payload, err := json.Marshal(map[string]string{
		"client_id":     o.clientID,
		"client_secret": o.clientSecret,
		"redirect_uri":  oauthRedirectURI,
		"refresh_token": refreshToken,
		"grant_type":    "refresh_token",
	})
	if err != nil {
		return AccessToken{}, fmt.Errorf("trakt: encode refresh request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+oauthTokenPath, bytes.NewReader(payload))
	if err != nil {
		return AccessToken{}, fmt.Errorf("trakt: build refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("trakt-api-version", apiVersion)
	req.Header.Set("trakt-api-key", o.clientID)

	resp, err := o.http.Do(req)
	if err != nil {
		return AccessToken{}, fmt.Errorf("trakt: POST %s: %w", oauthTokenPath, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseSize))
		_ = resp.Body.Close()
	}()

	switch {
	// Trakt answers 400 for a refresh token it has already spent, which is the
	// same problem as a 401: the grant is gone and only re-authorizing the
	// plugin brings it back.
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return AccessToken{}, fmt.Errorf("%w (refresh, status %d)", ErrUnauthorized, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return AccessToken{}, fmt.Errorf("trakt: POST %s: unexpected status %d", oauthTokenPath, resp.StatusCode)
	}

	var issued AccessToken
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseSize)).Decode(&issued); err != nil {
		return AccessToken{}, fmt.Errorf("trakt: decode refresh response: %w", err)
	}
	if issued.Token == "" || issued.RefreshToken == "" {
		return AccessToken{}, errors.New("trakt: refresh returned an empty token pair")
	}
	return issued, nil
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/trakt/ -run 'OAuth|AccessToken' -v`
Expected: PASS.

- [ ] **Step 5: Run the whole package and commit**

Run: `go test ./... && go vet ./...`
Expected: PASS.

```bash
git add internal/trakt/oauth.go internal/trakt/oauth_test.go
git commit -m "Add the trakt OAuth refresh call"
```

---

### Task 5: `TokenSource`

The decision logic. It is added alongside the existing `LoadToken`, which Task 6 deletes; keeping both for one commit is what lets this task be tested on its own.

**Files:**
- Create: `internal/trakt/tokensource.go`
- Test: `internal/trakt/tokensource_test.go`

**Interfaces:**
- Consumes: `jellyfin.Client`, `jellyfin.TraktConfig` (Tasks 2–3); `trakt.OAuth`, `trakt.AccessToken` (Task 4).
- Produces: `func NewTokenSource(client *jellyfin.Client, oauth *OAuth, userID string, logger *slog.Logger) *TokenSource`; `func (s *TokenSource) Token(ctx context.Context) (string, error)`; `func (s *TokenSource) Refresh(ctx context.Context) (string, error)`; `const refreshBuffer = time.Hour`.

- [ ] **Step 1: Write the failing test**

Create `internal/trakt/tokensource_test.go`:

```go
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
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/trakt/ -run 'Token|Refresh' -v`
Expected: compile failure — `NewTokenSource`, `refreshBuffer` undefined.

- [ ] **Step 3: Write the implementation**

Create `internal/trakt/tokensource.go`:

```go
package trakt

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/berejant/movie-torrent-finder/internal/jellyfin"
)

// refreshBuffer is how long before the recorded expiry the token is renewed. A
// sync that starts with a nearly-dead token would otherwise walk several pages
// and fail somewhere in the middle.
const refreshBuffer = time.Hour

// TokenSource supplies the trakt access token the syncer sends.
//
// The token lives in the Emby/Jellyfin trakt plugin's configuration, which both
// the plugin and this service read and write. Nothing is cached here: every
// call re-reads that configuration, so a refresh performed by the plugin is
// picked up without a restart.
type TokenSource struct {
	jellyfin *jellyfin.Client
	oauth    *OAuth
	userID   string
	logger   *slog.Logger

	// mu serialises refreshes. Trakt invalidates a refresh token the moment it
	// is spent, so two refreshes in flight would leave one of them holding a
	// pair that no longer works.
	mu sync.Mutex
}

// NewTokenSource builds the source. userID is the LinkedMbUserId to use, empty
// for the first linked account carrying a token.
func NewTokenSource(client *jellyfin.Client, oauth *OAuth, userID string, logger *slog.Logger) *TokenSource {
	return &TokenSource{
		jellyfin: client,
		oauth:    oauth,
		userID:   userID,
		logger:   logger.With("component", "trakt"),
	}
}

// Token returns the access token to send now, refreshing it first when the
// plugin's recorded expiry has passed or is about to.
func (s *TokenSource) Token(ctx context.Context) (string, error) {
	_, user, err := s.load(ctx)
	if err != nil {
		return "", err
	}

	expiry := user.Expiration()
	switch {
	case user.AccessToken() == "":
		s.logger.Info("the jellyfin trakt plugin holds no access token; refreshing")
	case expiry.IsZero():
		// The plugin records an expiry for every token it writes, so this is an
		// odd document rather than an expired token — and trakt is the
		// authority on whether the token still works.
		s.logger.Warn("the trakt access token has no readable expiry; using it as found")
		return user.AccessToken(), nil
	case time.Now().Add(refreshBuffer).Before(expiry):
		return user.AccessToken(), nil
	default:
		s.logger.Info("the trakt access token is expired or close to it; refreshing",
			"expires_at", expiry)
	}

	return s.Refresh(ctx)
}

// Refresh exchanges the plugin's refresh token for a new access token and
// stores the result back. Call it when trakt rejects a token Token believed was
// still good.
func (s *TokenSource) Refresh(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Re-read rather than trust what the caller saw: the plugin refreshes the
	// same grant, and if it got there first its token is the live one while the
	// refresh token we were about to spend is already dead.
	cfg, user, err := s.load(ctx)
	if err != nil {
		return "", err
	}
	if expiry := user.Expiration(); user.AccessToken() != "" && !expiry.IsZero() &&
		time.Now().Add(refreshBuffer).Before(expiry) {
		s.logger.Info("the trakt access token was already refreshed by the plugin; using it",
			"expires_at", expiry)
		return user.AccessToken(), nil
	}

	refreshToken := user.RefreshToken()
	if refreshToken == "" {
		return "", errors.New("trakt: the jellyfin trakt plugin holds no refresh token; re-authorize the plugin")
	}

	issued, err := s.oauth.Refresh(ctx, refreshToken)
	if err != nil {
		return "", err
	}

	// user points into cfg, so this edits the document that is about to be sent.
	user.SetTokens(issued.Token, issued.RefreshToken, issued.Expiry(time.Now()))

	if err := s.jellyfin.SaveTraktConfig(ctx, cfg); err != nil {
		// The token works; storing it did not. Using it for this sync beats
		// failing — but trakt has already retired the refresh token that
		// jellyfin still holds, so the next refresh will fail until the plugin
		// is re-authorized. That is worth saying out loud.
		s.logger.Error("refreshed the trakt access token but could not store it in jellyfin; "+
			"the refresh token jellyfin holds is now spent and the plugin will need re-authorizing",
			"err", err)
		return issued.Token, nil
	}

	s.logger.Info("refreshed the trakt access token", "expires_at", issued.Expiry(time.Now()))
	return issued.Token, nil
}

// load reads the plugin configuration and picks the account to use. The user
// points into the returned configuration, so editing it and saving that
// configuration writes the edit back.
func (s *TokenSource) load(ctx context.Context) (*jellyfin.TraktConfig, *jellyfin.TraktUser, error) {
	cfg, err := s.jellyfin.TraktConfig(ctx)
	if err != nil {
		return nil, nil, err
	}

	user, err := cfg.User(s.userID)
	if err != nil {
		return nil, nil, err
	}
	return cfg, user, nil
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/trakt/ -v`
Expected: PASS, including the still-present `LoadToken` tests.

- [ ] **Step 5: Vet and commit**

```bash
go vet ./... && go test ./...
git add internal/trakt/tokensource.go internal/trakt/tokensource_test.go
git commit -m "Add the trakt token source backed by the jellyfin API"
```

---

### Task 6: Switch the syncer over and delete the file reader

The cut-over. `LoadToken`, `Token`, `parseExpiration` and `TRAKT_TOKEN_FILE` all go in this commit, because the build is only green either side of it.

**Files:**
- Delete: `internal/trakt/token.go`, `internal/trakt/token_test.go`
- Modify: `internal/trakt/sync.go` (`Syncer` ~`:35`, `NewSyncer` ~`:61`, `Start` ~`:74`, `syncAndLog` ~`:113`, `SyncOnce` ~`:181`)
- Modify: `internal/trakt/client.go:25-28` (the `ErrUnauthorized` comment)
- Modify: `internal/config/config.go` (delete `Trakt.TokenFile` and its validation and log field)
- Modify: `cmd/server/main.go:74-87`
- Test: `internal/trakt/sync_test.go`, `internal/config/config_test.go`

**Interfaces:**
- Consumes: `TokenSource` (Task 5), `jellyfin.NewClient` (Task 3), `NewOAuth` (Task 4).
- Produces: `func NewSyncer(store Store, client *Client, tokens *TokenSource, cfg config.Config, notifier Notifier, logger *slog.Logger) *Syncer` — note the new third parameter.

- [ ] **Step 1: Write the failing test**

In `internal/trakt/sync_test.go`, replace the two token-file lines in `newSyncer`:

```go
	cfg := config.Config{DuplicateCheckEnabled: true, Trakt: testConfig(fake.URL)}
	cfg.Trakt.TokenFile = writeTokenFile(t, exampleFile)
```

with a jellyfin-backed token source. `newSyncer` grows a `*fakeJellyfin` return so a test can inspect saves:

```go
// newSyncer wires a syncer onto a real SQLite store, a fake trakt and a fake
// jellyfin holding an unexpired token.
func newSyncer(t *testing.T, fake *fakeTrakt, tweak func(*config.Config)) (*Syncer, *storage.Store, *countingNotifier) {
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

	// The fake trakt only accepts testToken, so that is what jellyfin holds.
	jelly := newFakeJellyfin(t, fmt.Sprintf(
		`{"TraktUsers":[{"AccessToken":%q,"RefreshToken":"stored-refresh","AccessTokenExpiration":%q}]}`,
		testToken, rfc3339(time.Now().Add(30*24*time.Hour))))
	tokens := newTokenSource(t, jelly, newFakeOAuth(t), "")

	notifier := &countingNotifier{}
	return NewSyncer(store, client, tokens, cfg, notifier, testLogger()), store, notifier
}
```

Add the new behaviour test at the end of `internal/trakt/sync_test.go`:

```go
// An expiry the plugin recorded can be wrong — a revoked token, a clock that
// moved. One refresh and one retry: a pass discards partial results, so
// replaying the whole walk costs only the requests.
func TestSyncRefreshesAndRetriesOnceOnRejection(t *testing.T) {
	fake := newFakeTrakt(t, [][]WatchlistItem{{
		movie(396109, "Extraction", 2020, "2026-08-04T13:38:29Z"),
	}})

	store, err := storage.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("storage.Open() error: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cfg := config.Config{DuplicateCheckEnabled: true, Trakt: testConfig(fake.URL)}
	client, err := NewClient(cfg.Trakt, testLogger())
	if err != nil {
		t.Fatalf("NewClient() error: %v", err)
	}

	// Jellyfin holds a token the fake trakt rejects, but claims it is good for
	// another month; only a refresh produces the token trakt accepts.
	jelly := newFakeJellyfin(t, fmt.Sprintf(
		`{"TraktUsers":[{"AccessToken":"revoked","RefreshToken":"stored-refresh","AccessTokenExpiration":%q}]}`,
		rfc3339(time.Now().Add(30*24*time.Hour))))
	oauth := newFakeOAuth(t)
	oauth.body = fmt.Sprintf(
		`{"access_token":%q,"refresh_token":"fresh-refresh","expires_in":7776000}`, testToken)

	syncer := NewSyncer(store, client, newTokenSource(t, jelly, oauth, ""), cfg, &countingNotifier{}, testLogger())

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

	store, err := storage.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("storage.Open() error: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cfg := config.Config{DuplicateCheckEnabled: true, Trakt: testConfig(fake.URL)}
	client, err := NewClient(cfg.Trakt, testLogger())
	if err != nil {
		t.Fatalf("NewClient() error: %v", err)
	}

	jelly := newFakeJellyfin(t, fmt.Sprintf(
		`{"TraktUsers":[{"AccessToken":"revoked","RefreshToken":"stored-refresh","AccessTokenExpiration":%q}]}`,
		rfc3339(time.Now().Add(30*24*time.Hour))))
	oauth := newFakeOAuth(t) // issues "fresh-access", which the fake trakt also rejects

	syncer := NewSyncer(store, client, newTokenSource(t, jelly, oauth, ""), cfg, &countingNotifier{}, testLogger())

	_, err = syncer.SyncOnce(context.Background())
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("error = %v, want ErrUnauthorized", err)
	}
	if oauth.calls != 1 {
		t.Errorf("refreshed %d times, want exactly 1", oauth.calls)
	}
}
```

Add `errors`, `fmt` and `time` to that file's imports if they are not already there.

In `internal/config/config_test.go`, delete the `TRAKT_TOKEN_FILE` lines from `TestLoadTraktEnabled` (`:220`), `TestLoadJellyfinEnabled`, `TestLoadRejectsIncompleteJellyfin`, `TestLoadRejectsABadJellyfinHost` and `TestLoadRejectsABadHealthcheckUUID` (`:269`); change the `TestLoadTraktEnabled` assertion at `:228` to check only the client id; and replace `TestLoadRejectsIncompleteTrakt`'s table (`:238-241`) with:

```go
	tests := map[string]string{
		"TRAKT_CLIENT_ID":     "TRAKT_CLIENT_SECRET",
		"TRAKT_CLIENT_SECRET": "TRAKT_CLIENT_ID",
	}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/trakt/ ./internal/config/`
Expected: compile failure — `NewSyncer` takes five arguments, `writeTokenFile` undefined.

- [ ] **Step 3: Delete the file reader**

```bash
git rm internal/trakt/token.go internal/trakt/token_test.go
```

- [ ] **Step 4: Rewire the syncer**

In `internal/trakt/sync.go`:

Add the field to `Syncer`, after `client`:

```go
	tokens   *TokenSource
```

Update the `Syncer` doc comment — the paragraph beginning "It owns no state of its own" says the token is re-read from disk. Replace "the access token is re-read from disk" with "the access token is re-read from the jellyfin trakt plugin".

`NewSyncer` gains the parameter and sets the field:

```go
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
```

In `Start`, drop the `"token_file", s.cfg.TokenFile,` line from the log call.

In `syncAndLog`, replace the `ErrUnauthorized` branch:

```go
		if errors.Is(err, ErrUnauthorized) {
			s.logger.Error("trakt rejected the credentials and refreshing did not help; "+
				"re-authorize the trakt plugin in jellyfin", "err", err)
		} else {
```

In `failuresBeforeAlert`'s comment, "the token file may be mid-rewrite" is no longer a reason; replace that clause with "jellyfin may be restarting".

Replace the head of `SyncOnce` (the `LoadToken` block and the `collect` call) with:

```go
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

		if token, err = s.tokens.Refresh(ctx); err != nil {
			return Summary{}, err
		}
		items, summary, err = s.collect(ctx, token, cursor)
	}
	if err != nil {
		return Summary{}, err
	}
```

The rest of `SyncOnce`, from `if len(items) == 0`, is unchanged.

In `internal/trakt/client.go`, rewrite the `ErrUnauthorized` comment:

```go
// ErrUnauthorized means trakt rejected the access token or the client id. The
// syncer answers it by refreshing the token once; a second one means the grant
// is gone and the trakt plugin has to be re-authorized in jellyfin.
var ErrUnauthorized = errors.New("trakt: credentials rejected")
```

- [ ] **Step 5: Drop `TRAKT_TOKEN_FILE` from the config**

In `internal/config/config.go`, delete the `TokenFile` field and its comment from `Trakt`, the `TRAKT_TOKEN_FILE` check in `Validate`, and `slog.String("token_file", c.Trakt.TokenFile),` from `LogValue`.

- [ ] **Step 6: Rewire main**

In `cmd/server/main.go`, add `"github.com/berejant/movie-torrent-finder/internal/jellyfin"` to the imports and replace the body of the `if cfg.Trakt.Enabled` block:

```go
	if cfg.Trakt.Enabled {
		client, err := trakt.NewClient(cfg.Trakt, logger)
		if err != nil {
			return err
		}

		// The token lives in the jellyfin trakt plugin's configuration, which
		// this service reads over the API and writes back to when it refreshes.
		media, err := jellyfin.NewClient(cfg.Jellyfin, logger)
		if err != nil {
			return err
		}
		tokens := trakt.NewTokenSource(media, trakt.NewOAuth(cfg.Trakt), cfg.Jellyfin.UserID, logger)

		syncer = trakt.NewSyncer(store, client, tokens, cfg, pool, logger)
		syncer.Start(ctx)
	} else {
```

- [ ] **Step 7: Run everything**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS. `grep -rn "TokenFile\|LoadToken\|Trakt.xml" --include='*.go' .` returns nothing.

- [ ] **Step 8: Commit**

```bash
git add -A internal/trakt internal/config cmd/server
git commit -m "Read the trakt token from the jellyfin API instead of Trakt.xml"
```

---

### Task 7: Documentation

**Files:**
- Modify: `.env.example` (~`:126-142`), `README.md` (`:66`, `:76-82`, `:207`, `:254`), `AGENTS.md` (`:160`, `:348`), `docker-compose.yml` (`:21-23`)
- Delete: `Trakt.xml.example`

- [ ] **Step 1: Rewrite the `.env.example` block**

Replace the trakt credential and token-file block with:

```sh
# Credentials of the trakt application whose OAuth grant the Emby/Jellyfin trakt
# plugin holds. They must be the pair that issued the plugin's refresh token —
# for a stock plugin install, the plugin's own compiled-in pair:
# https://github.com/jellyfin/jellyfin-plugin-trakt/blob/master/Trakt/Api/TraktURIs.cs
# Use your own only if you re-authorized the plugin against your own application
# (https://trakt.tv/oauth/applications).
TRAKT_CLIENT_ID=
TRAKT_CLIENT_SECRET=

# The jellyfin holding that plugin. This service reads the access token out of
# the plugin's configuration over the API, and writes a refreshed one back, so
# no access to jellyfin's config volume is needed.
JELLYFIN_HOST=http://jellyfin:8096

# An API key from Dashboard -> Advanced -> API Keys.
JELLYFIN_API_KEY=

# Which linked jellyfin user's trakt account to sync, by LinkedMbUserId. Leave
# it unset unless several users have linked trakt accounts to the plugin; the
# first account carrying a token is used then.
JELLYFIN_USER_ID=

# JELLYFIN_TIMEOUT_SECONDS=30
```

Delete the `TRAKT_TOKEN_FILE` entry and its comment.

- [ ] **Step 2: Rewrite the README**

- `:66` — replace the `TRAKT_TOKEN_FILE` line in the example with `TRAKT_CLIENT_SECRET`, `JELLYFIN_HOST` and `JELLYFIN_API_KEY`.
- `:76-82` — replace the paragraph and the `-v` mount with: the plugin owns the OAuth grant; this service reads the token from `GET /Plugins/4fe3201ed6ae4f2e8917e12bda571281/Configuration` and, when it has expired, refreshes it with `TRAKT_CLIENT_SECRET` and stores it back, so the plugin picks it up too. Say how to mint the key: Jellyfin Dashboard → Advanced → API Keys → `+`.
- `:207` — replace the `TRAKT_TOKEN_FILE` table row with rows for `TRAKT_CLIENT_SECRET`, `JELLYFIN_HOST`, `JELLYFIN_API_KEY`, `JELLYFIN_USER_ID`, `JELLYFIN_TIMEOUT_SECONDS`.
- `:254` — the troubleshooting entry now reads: `trakt rejected the credentials` after a refresh means the grant is gone; re-authorize the trakt plugin in Jellyfin.

- [ ] **Step 3: Rewrite AGENTS.md**

- `:160` — replace the `TRAKT_TOKEN_FILE` row with `TRAKT_CLIENT_SECRET`, `JELLYFIN_HOST`, `JELLYFIN_API_KEY` (all **required when enabled**) and the optional `JELLYFIN_USER_ID`, `JELLYFIN_TIMEOUT_SECONDS`.
- `:348` — rewrite the "Access token" paragraph: read over the plugin API before every sync, never cached; the account is `JELLYFIN_USER_ID` by `LinkedMbUserId` or the first with a token; an expired or nearly-expired token is refreshed here and written back, and the document is round-tripped as raw JSON so the plugin's other settings survive; a 401 from trakt refreshes once and retries the pass.

- [ ] **Step 4: Drop the mount and the example file**

In `docker-compose.yml`, delete the commented `Trakt.xml` mount and its two comment lines.

```bash
git rm Trakt.xml.example
```

- [ ] **Step 5: Verify and commit**

Run: `grep -rn "Trakt.xml\|TRAKT_TOKEN_FILE" . --exclude-dir=.git --exclude-dir=docs`
Expected: no matches.

Run: `go build ./... && go test ./...`
Expected: PASS.

```bash
git add -A
git commit -m "Document the jellyfin API token source"
```

---

## Manual verification

Against the operator's real Jellyfin, after Task 6:

```bash
curl -s -H 'Authorization: MediaBrowser Token="<api key>"' \
  http://192.168.10.180:8096/Plugins/4fe3201ed6ae4f2e8917e12bda571281/Configuration
```

Confirm the `AccessToken` the service logs at startup matches, and that running with a hand-edited past `AccessTokenExpiration` produces one refresh, a new token in the document, and every other field unchanged.