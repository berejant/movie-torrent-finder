# Trakt access token via the Jellyfin plugin API

## Problem

The trakt watchlist sync reads its access token from `Trakt.xml`, the on-disk
configuration of the Emby/Jellyfin trakt plugin, mounted read-only into the
container. That has two costs:

- The deployment needs a bind mount into another application's config directory.
  On Synology that means granting this service read access to the Jellyfin
  volume.
- The service is a passive reader. When the token expires it can only log a
  warning and wait for the plugin to refresh it. If the plugin is idle — nothing
  scrobbling, no scheduled sync — the refresh never happens and the watchlist
  sync stays broken.

## Solution

Read the token from Jellyfin's plugin-configuration HTTP API instead of the
file, and refresh it ourselves when it has expired, writing the new token back
through the same API so the plugin picks it up too.

The file path is removed entirely. There is one source of truth for the token,
and one code path.

## Facts this design is built on

Verified against a live Jellyfin (`http://192.168.10.180:8096`) and against the
plugin source at `jellyfin/jellyfin-plugin-trakt@master`.

The plugin GUID is `4fe3201ed6ae4f2e8917e12bda571281` and is stable.

`GET /Plugins/{guid}/Configuration` with
`Authorization: MediaBrowser Token="<api key>"` returns 200 and:

```json
{"TraktUsers":[{"AccessToken":"…","RefreshToken":"…","LinkedMbUserId":"…",
  "AccessTokenExpiration":"2026-08-17T00:40:08.1168339+03:00", "…":"…"}]}
```

`POST` to the same path with the full document as the body saves it, answering
204.

The plugin refreshes with (`Trakt/Api/TraktApi.cs:1019`):

- `POST https://api.trakt.tv/oauth/token`
- body `{client_id, client_secret, redirect_uri: "urn:ietf:wg:oauth:2.0:oob",
  refresh_token, grant_type: "refresh_token"}`
- on success stores `AccessTokenExpiration = now + expires_in * 3/4`
  (`ExpirationWithBuffer`, `Trakt/Api/DataContracts/TraktUserAccessToken.cs`)

and treats a token as needing refresh when `now > AccessTokenExpiration`
(`SetRequestHeaders`, same file).

The client id and secret are compiled into the plugin
(`Trakt/Api/TraktURIs.cs`), so a default install's refresh token belongs to that
application. Refreshing requires the same pair.

## Design

### `internal/jellyfin` — new package

```go
type Client struct { baseURL, apiKey string; http *http.Client; logger *slog.Logger }

func NewClient(cfg config.Jellyfin, logger *slog.Logger) (*Client, error)
func (c *Client) TraktConfig(ctx context.Context) (*TraktConfig, error)
func (c *Client) SaveTraktConfig(ctx context.Context, cfg *TraktConfig) error
```

`TraktConfig` holds the users as `[]map[string]json.RawMessage`, not as a typed
struct. Only `AccessToken`, `RefreshToken` and `AccessTokenExpiration` are ever
read or written; every other key is carried back to Jellyfin byte-identical.
This is the central constraint of the package: a save is a read-modify-write of
a document owned by another application, and a typed struct would silently reset
`Scrobble`, `LocationsExcluded`, and anything a future plugin version adds, to
their zero values.

Accessors on `TraktConfig` expose what the token source needs:

- `User(linkedMbUserID string) (*TraktUser, error)` — the entry whose
  `LinkedMbUserId` matches, or when the argument is empty, the first entry with
  a non-empty `AccessToken`. Errors when the list is empty or nothing matches.
- On `*TraktUser`: `AccessToken()`, `RefreshToken()`, `Expiration() time.Time`,
  and `SetTokens(access, refresh string, expiry time.Time)`.

`Expiration` parses the .NET timestamp shapes the existing `parseExpiration`
already handles (offset, `Z`, and no zone read as UTC); it returns the zero time
when the field is absent or unparseable. `SetTokens` writes the expiry as
RFC3339 with nanoseconds, matching what the plugin writes.

Both calls send `Authorization: MediaBrowser Token="…"`. A save accepts any 2xx.
A 401 from either is reported distinctly so the log can name the API key rather
than the trakt token.

### `internal/trakt` — `TokenSource` replaces `LoadToken`

`token.go` loses the XML parsing entirely and becomes:

```go
type TokenSource struct {
    jellyfin  *jellyfin.Client
    oauth     *OAuth
    userID    string        // optional LinkedMbUserId pin
    logger    *slog.Logger
    mu        sync.Mutex
}

func (s *TokenSource) Token(ctx context.Context) (string, error)
func (s *TokenSource) Refresh(ctx context.Context) (string, error)
```

`Token` fetches the configuration, selects the user, and returns its access
token — unless the recorded expiry is already past or falls within
`refreshBuffer` (1 hour), in which case it delegates to `Refresh`. When the
expiry field is missing or unparseable the token is returned as found, with a
warning: that is what the file-based implementation did, and trakt remains the
authority on whether a token works.

`Refresh` holds `mu` and re-reads the configuration before doing anything. If
the freshly read token is valid, it is returned and no refresh happens — the
plugin may have refreshed in the meantime, and a trakt refresh token is
single-use, so spending ours needlessly would invalidate the plugin's. Otherwise
it calls the OAuth endpoint, applies `SetTokens` with
`now + expires_in * 3/4`, and saves.

A failed save does not fail the call: the new token is returned and used for
this pass, logged at error level. A Jellyfin write problem should not cost a
working token. The consequence is that the refresh token stored in Jellyfin is
then stale, and the next refresh will fail until the plugin re-authenticates —
so the error message says exactly that.

The mutex plus the re-read narrows, but cannot close, the window where the
plugin and this service refresh concurrently and one invalidates the other's
token. The next sync tick recovers.

### `internal/trakt/oauth.go` — new file

```go
type OAuth struct { baseURL, clientID, clientSecret string; http *http.Client }
func (o *OAuth) Refresh(ctx context.Context, refreshToken string) (AccessToken, error)
```

`AccessToken` carries `Token`, `RefreshToken` and `ExpiresIn`. The request is
the plugin's, field for field, against `{TRAKT_BASE_URL}/oauth/token`. A 4xx is
wrapped as `ErrUnauthorized` so the caller can tell "trakt rejected the refresh
token" from "trakt is down".

### Syncer: refresh and retry once on 401

`SyncOnce` takes its token from the `TokenSource`. When `collect` returns
`ErrUnauthorized`, it calls `Refresh` and runs the pass again, once. A pass is
already all-or-nothing — `collect` discards partial results so the cursor only
moves on a complete run — so retrying the whole walk is safe.

This covers the case the expiry field cannot: a token revoked at trakt, or an
`AccessTokenExpiration` that is simply wrong.

`ErrUnauthorized`'s doc comment and the "waiting for the token file to be
refreshed" log line in `syncAndLog` are rewritten; the `token_file` field is
dropped from the startup log line in `Start`.

### Configuration

`config.Trakt` drops `TokenFile` and gains:

```go
ClientSecret string `env:"CLIENT_SECRET"`
```

A new struct, bound at `envPrefix:"JELLYFIN_"`:

```go
type Jellyfin struct {
    Host           string `env:"HOST"`
    APIKey         string `env:"API_KEY"`
    UserID         string `env:"USER_ID"`                       // optional
    TimeoutSeconds int    `env:"TIMEOUT_SECONDS" envDefault:"30"`
}
```

Validation, only when `TRAKT_ENABLED=true` (an install with the sync off must
not be forced to configure Jellyfin):

- `TRAKT_CLIENT_ID` and `TRAKT_CLIENT_SECRET` must be set
- `JELLYFIN_API_KEY` must be set
- `JELLYFIN_HOST` must be set, parse, use http or https, and have a host
- `JELLYFIN_TIMEOUT_SECONDS` must be >= 1

`LogValue` gains a `jellyfin` group — host, redacted API key, redacted user id,
timeout — and redacts the trakt client secret alongside the client id.

### Wiring

`cmd/server/main.go` builds the Jellyfin client and the `TokenSource` inside the
existing `if cfg.Trakt.Enabled` block and passes the source to `NewSyncer`. A
construction failure is fatal, as `trakt.NewClient`'s already is.

## Testing

`internal/jellyfin/client_test.go`, against `httptest`:

- a GET is sent with the right path and auth header; the response decodes
- a save posts the whole document back and **unknown fields survive verbatim** —
  the test asserts on the raw body, including a key the struct does not know
- non-2xx on either call errors; 401 is distinguishable
- user selection: by `LinkedMbUserId`; first-with-a-token when unpinned; an
  entry with an empty token is skipped; empty list and no-match both error
- `.NET` timestamp shapes parse; a malformed one yields the zero time

`internal/trakt/token_test.go`, rewritten against `httptest` servers for
Jellyfin and for the OAuth endpoint:

- an unexpired token is returned untouched and no OAuth call is made
- an expired token triggers a refresh; the saved document carries the new
  access token, refresh token and an expiry of `now + expires_in * 3/4`
- a token inside the 1h buffer refreshes; one just outside it does not
- a missing or unparseable expiry returns the token as-is
- `Refresh` re-reads first: when the second read shows a valid token, no OAuth
  call is made
- a failing save still returns the new token
- OAuth 4xx surfaces as `ErrUnauthorized`

`internal/trakt/sync_test.go` gains: a 401 on the first pass refreshes once and
retries; a second 401 fails the sync without a further refresh.

`internal/config/config_test.go`: the new required variables, the URL checks,
and that none of them are required while `TRAKT_ENABLED` is false.

## Documentation

- `.env.example` — describe `TRAKT_CLIENT_SECRET`, `JELLYFIN_HOST`,
  `JELLYFIN_API_KEY`, `JELLYFIN_USER_ID`, `JELLYFIN_TIMEOUT_SECONDS`; delete
  `TRAKT_TOKEN_FILE`; note that the credentials must be the pair that issued the
  refresh token, which for a stock plugin install is the plugin's own.
- `README.md` — rewrite the token setup (`:66`, `:76-82`), the variable table row
  (`:207`), and the stale-token troubleshooting entry (`:254`) around the API,
  including how to mint a Jellyfin API key.
- `docker-compose.yml` — delete the commented Trakt.xml mount (`:21-23`).
- `AGENTS.md` — the variable table (`:160`) and the "Access token" paragraph
  (`:348`), which describes the file, the first-user rule and "never writes the
  file"; all three change.
- `Trakt.xml.example` — delete; nothing reads that shape any more.

## Out of scope

- Performing the initial OAuth authorization. The plugin owns that; this service
  only refreshes an existing grant.
- Supporting more than one linked trakt user. One token, one watchlist.
- Reading anything else out of Jellyfin.