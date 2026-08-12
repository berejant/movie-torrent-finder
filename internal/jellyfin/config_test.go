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
		"missing":      `{"TraktUsers":[{"AccessToken":"a"}]}`,
		"empty":        `{"TraktUsers":[{"AccessToken":"a","AccessTokenExpiration":""}]}`,
		"not a date":   `{"TraktUsers":[{"AccessToken":"a","AccessTokenExpiration":"soon"}]}`,
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
