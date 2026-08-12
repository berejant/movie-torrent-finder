# AGENTS.md

## Project: Movie Torrent Downloader

### Goal
Build a utility service that searches torrent files for movies across the configured trackers and saves `.torrent` files for later use.

The service must run in Docker (primary target: Synology Docker), while remaining platform-agnostic.

> This document is the **source of truth**. Where `mazepa-torrent-download.go` disagrees with it, the prototype is wrong and must be updated. The prototype is an unfinished design sketch (it does not compile: `colly` is missing from the import block, `min` is redeclared against the Go builtin, and there is no `go.mod`).

---

## Tech Stack

| Concern | Choice |
|---|---|
| Language | Go |
| HTTP framework | Echo |
| Database | SQLite (`modernc.org/sqlite`, pure Go — no CGO, so the image stays small and cross-builds cleanly) |
| Scraping | `net/http` + `goquery` over a shared cookie-jar client (**not** `colly` — see note) |
| Templating | `html/template`, templates and static assets embedded via `embed.FS` |
| Frontend | htmx + Pico.css (classless), optional Alpine.js for selection state |
| Config | `github.com/caarlos0/env/v10` for binding env vars into structs |
| Env file loading | `github.com/joho/godotenv` — loads `.env` for local dev runs outside Docker |
| Logging | `log/slog`, JSON handler |

No Node build step. No SPA. Everything ships as one static binary plus embedded assets.

**Why not colly.** The original stack decision named `colly`, and the prototype used it. During implementation it turned out to work against every requirement in §4 and §7: colly owns request headers (via `OnRequest`), owns rate limiting (`LimitRule`), and owns concurrency (its own parallelism), while this service needs to control all three itself — a precise browser header set with a Referer chain, one shared token bucket across five workers, and singleflight login. None of colly's actual value (crawling, link following, queueing, caching) is used here, since every request is a single known URL. The result is `net/http` with a shared cookie jar plus `goquery` for parsing, which is fewer moving parts and total control over the wire format.

---

## Core Requirements

### 1) Tracker Search Utility
- Find torrent files for movie titles from the configured torrent trackers.
- Support tracker-specific auth and request options.
- Save selected `.torrent` files to a configured directory.
- **Every configured tracker is searched for every request**, concurrently, and the best release across all of them wins. One request is one unit of work: the queue is global, not per tracker, and only the winning tracker is recorded on the request.
- A tracker that fails does **not** hold back a release another tracker found: the failure is logged and the remaining candidates are ranked as usual. Only when no tracker produced a candidate does the failure reach the retry policy — retryable if any tracker failed in a retryable way, `NOT_FOUND` when every tracker simply had nothing.

**Presets, not per-tracker code.** A tracker is described by a `Preset` in `internal/config/presets.go` — paths, form fields, session selectors, row/topic/download/forum selectors and the size column index. The parser is preset-driven, so adding a tracker means adding a preset, not a code path. Shipped presets:

| Preset | Site | Engine | Result columns |
|---|---|---|---|
| `mazepa` | mazepa.to | TorrentPier | publish, status, forum, topic, author, **size/download**, seeders, leechers, replies, added |
| `torrentpier` | — | TorrentPier | same as above; for any other install |
| `toloka` | toloka.to | phpBB2-derived | icon, forum, topic, author, checked, download, **size**, status, completed, seeders, leechers, replies, added |

toloka specifics worth knowing: search results are gated behind a session (an anonymous visitor gets the table chrome and zero rows); relative hrefs carry no leading slash (`download.php?id=…`), so they only resolve against the search page URL; the logged-in header links `/login.php?logout=true`, so the register link — not a login link — is what distinguishes the two session states; and the login form ticks `autologin`, which the client posts via `login_extra_fields`.

Saved pages from both trackers live in `html-examples/<slug>/`, and the parser tests run against them, so a markup change can be diagnosed against the same input the parser saw.

### 2) Docker-First Runtime
- Must run as a containerized app.
- Compatible with Synology Docker (volume mounts, env-file config, non-root).
- Avoid host-specific assumptions so it can run on any Linux-compatible Docker host.
- The container runs as a **non-root** user whose UID/GID are set from `PUID`/`PGID` at startup (entrypoint adjusts the runtime user and `chown`s the data/output dirs). Without this, Synology volume mounts are not writable.

### 3) Configuration via Environment Variables / Env File

#### Application
| Variable | Default | Notes |
|---|---|---|
| `HTTP_PORT` | `8080` | web server listening port |
| `TORRENT_FILES_DIR` | — | **required**, path to store downloaded `.torrent` files |
| `DB_PATH` | `/data/app.db` | SQLite file, must live on a mounted volume |
| `TZ` | `UTC` | display timezone; all timestamps are **stored in UTC** |
| `PUID` / `PGID` | `1000` / `1000` | runtime user for non-root operation |
| `LOG_LEVEL` | `info` | `debug`/`info`/`warn`/`error` |
| `BATCH_MAX_LINES` | `100` | max movie titles per batch submission |
| `AUTH_USER` / `AUTH_PASSWORD` | unset | HTTP Basic auth for UI + API. Enabled **only when both are set**; unset means no auth (LAN-only deployments) |

#### Trackers (prefix `TRACKER_<SLUG>_`)

`TRACKERS` is a comma-separated list of tracker slugs, in configuration order. Every listed tracker is searched for every request. Each slug also selects a **preset** — the paths, form fields, selectors and column layout of that site — so adding a supported tracker needs nothing but credentials.

| Variable | Default | Notes |
|---|---|---|
| `TRACKERS` | unset | e.g. `toloka,mazepa`; unset selects the legacy layout below |
| `WORKERS` | `5` | requests searched at once, across all trackers |
| `TRACKER_<SLUG>_PRESET` | the slug, else `torrentpier` | `toloka` \| `mazepa` \| `torrentpier` |
| `TRACKER_<SLUG>_BASE_URL` | from the preset | required when the preset names no site |
| `TRACKER_<SLUG>_LOGIN` | — | **required** |
| `TRACKER_<SLUG>_PASSWORD` | — | **required** |
| `TRACKER_<SLUG>_PRIORITY` | `1` | lower wins; only breaks ties between equal releases |
| `TRACKER_<SLUG>_TIMEOUT_SECONDS` | `30` | per-request timeout |
| `TRACKER_<SLUG>_RPS` | `1` | rate limit for this tracker, shared by all workers |
| `TRACKER_<SLUG>_MAX_SIZE_BYTES` | `0` | `0` = unlimited |
| `TRACKER_<SLUG>_USER_AGENT` | real browser UA (see below) | overridable |
| `TRACKER_<SLUG>_EXTRA_OPTIONS` | unset | JSON object merged over the preset (see below) |

The slug is the tracker's identity: it picks the variables, the preset, and the tracker token in saved filenames.

**Legacy layout.** With `TRACKERS` unset, one tracker is configured on the unprefixed variables (`TRACKER_NAME`, `TRACKER_BASE_URL`, `TRACKER_LOGIN`, `TRACKER_PASSWORD`, `TRACKER_PRIORITY`, `TRACKER_TIMEOUT_SECONDS`, `TRACKER_RPS`, `TRACKER_MAX_SIZE_BYTES`, `TRACKER_USER_AGENT`, `TRACKER_EXTRA_OPTIONS`), and `TRACKER_WORKERS` still sizes the pool while `WORKERS` is unset. Existing deployments therefore keep working untouched.

`TRACKER_<SLUG>_EXTRA_OPTIONS` is a JSON object merged over the preset key by key; every key is optional. The `mazepa`/`torrentpier` preset spelled out:

```json
{
  "tracker_path": "/tracker.php",
  "login_path": "/login.php",
  "login_username_field": "login_username",
  "login_password_field": "login_password",
  "login_submit_field": "login",
  "login_submit_value": "Увійти",
  "search_query_field": "nm",
  "logged_in_selector": "a[href*='logout']",
  "logged_out_selector": "#register_link",
  "result_row_selector": "#forum_table tbody tr",
  "topic_link_selector": "a[href*='topic-']",
  "download_link_selector": "a[href*='dl.php?id=']",
  "forum_link_selector": "a[href*='forum-']",
  "size_cell_index": 5
}
```

...and the `toloka` preset:

```json
{
  "tracker_path": "/tracker.php",
  "login_path": "/login.php",
  "login_username_field": "username",
  "login_password_field": "password",
  "login_submit_field": "login",
  "login_submit_value": "Вхід",
  "login_extra_fields": {"autologin": "1"},
  "search_query_field": "nm",
  "logged_in_selector": "a[href*='logout']",
  "logged_out_selector": "a[href*='mode=register']",
  "result_row_selector": "table.forumline tr.prow1, table.forumline tr.prow2",
  "topic_link_selector": "td.topictitle a",
  "download_link_selector": "a[href*='download.php?id=']",
  "forum_link_selector": "a[href*='tracker.php?f=']",
  "size_cell_index": 6
}
```

`size_cell_index` is the zero-based `<td>` holding the size; set it to `-1` to skip the direct lookup and scan the whole row for the first cell that parses as a size. `login_extra_fields` are additional form fields posted with the credentials — toloka's login form ticks `autologin` by default, and without it the session cookie expires with the browser session.

#### Retry
| Variable | Default |
|---|---|
| `RETRY_MAX_ATTEMPTS` | `5` |
| `RETRY_BASE_SECONDS` | `3` |
| `RETRY_MAX_BACKOFF_SECONDS` | `60` |

#### Duplicates
| Variable | Default | Notes |
|---|---|---|
| `DUPLICATE_CHECK_ENABLED` | `true` | set `false` to globally disable uniqueness checks |

#### Trakt.tv watchlist (prefix `TRAKT_`)
| Variable | Default | Notes |
|---|---|---|
| `TRAKT_ENABLED` | `false` | when false nothing else here is read or validated |
| `TRAKT_CLIENT_ID` | — | **required when enabled**, sent as the `trakt-api-key` header |
| `TRAKT_CLIENT_SECRET` | — | **required when enabled**, used to refresh the plugin's token |
| `JELLYFIN_HOST` | — | **required when enabled**, the jellyfin holding the trakt plugin |
| `JELLYFIN_API_KEY` | — | **required when enabled**, minted at Dashboard -> Advanced -> API Keys |
| `JELLYFIN_USER_ID` | unset | linked user to sync, by `LinkedMbUserId`; unset = first with a token |
| `JELLYFIN_TIMEOUT_SECONDS` | `30` | per-request timeout for the jellyfin API |
| `TRAKT_BASE_URL` | `https://api.trakt.tv` | override only for a proxy or a test double |
| `TRAKT_INTERVAL_MINUTES` | `15` | poll period; the first sync runs at startup |
| `TRAKT_TIMEOUT_SECONDS` | `30` | per-request timeout |
| `TRAKT_PAGE_LIMIT` | `100` | watchlist page size, max `1000` |
| `TRAKT_MAX_PAGES` | `10` | pages per run; the remainder follows on the next run |
| `TRAKT_QUERY_WITH_YEAR` | `true` | search `Extraction 2020` rather than `Extraction` |
| `TRAKT_HEALTHCHECK_UUID` | unset | healthchecks.io check id; **unset means no signals are sent** |
| `TRAKT_HEALTHCHECK_BASE_URL` | `https://hc-ping.com` | ping endpoint, for a self-hosted healthchecks |

Configuration behavior:
- Validate required env vars on startup.
- Fail fast with clear error messages if required config is missing/invalid.
- Never log secrets (`TRACKER_PASSWORD`, `AUTH_PASSWORD`, tokens, cookies).

**Local development without Docker:** on startup the app calls `godotenv.Load()` before binding config, so a `.env` file in the working directory populates the environment for a plain `go run .`. Loading is best-effort — a missing `.env` is **not** an error, since in Docker the values come from the env-file/compose environment instead. Real environment variables always win over `.env` entries (`godotenv.Load` does not overwrite what is already set). Ship a `.env.example` documenting every variable above with safe placeholder values, and keep `.env` out of version control.

### 4) HTTP Client Behavior

All tracker requests must look like a real browser.

- **User-Agent:** a current desktop Chrome string, e.g.
  `Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36`
- **Page requests** send:
  - `Accept: text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8`
  - `Accept-Language: uk-UA,uk;q=0.9,ru;q=0.8,en-US;q=0.7,en;q=0.6`
  - `Upgrade-Insecure-Requests: 1`
  - `Sec-Fetch-Dest: document`, `Sec-Fetch-Mode: navigate`, `Sec-Fetch-Site: same-origin`
  - `Referer` set to the plausible previous page (login form → login POST, search page → topic, topic → file download)
- **`.torrent` download** sends `Accept: application/x-bittorrent,*/*;q=0.8` and `Referer: <topic URL>`.
- **Do not set `Accept-Encoding` manually.** Go's transport adds gzip and transparently decompresses only when it owns the header; setting it by hand means handling decompression yourself.
- One cookie jar per tracker, shared by login/search/download so the session is reused.

### 5) Web Interface for Batch Scheduling

Server-rendered HTML fragments driven by htmx. No JSON API is required for the UI (a machine API remains post-MVP).

Provide a web UI to schedule torrent-search jobs:
- Textarea input for batch requests (copy/paste movie list, one per line).
- Submit creates queued jobs.
- Show job list with statuses and basic metadata.

**Batch input rules:**
- Max `BATCH_MAX_LINES` (100) lines per submission; reject the whole batch with a clear message if exceeded.
- Trim each line; drop empty lines.
- Deduplicate identical normalized queries **within the batch** before creating tasks.

Job list actions:
- Retry failed request manually.
- For `NOT_FOUND`, allow editing search query and retry.
- For `DUPLICATE`, allow editing search query and/or forcing the retry.
- Cancel a task that has not started yet.
- Remove task from list.
- Remove related downloaded `.torrent` file when removing task (if file exists).
- Force processing of duplicate task (default duplicate behavior is reject).
- Group/batch actions for selected tasks.

**Refresh strategy — polling, not SSE:**
- The job table is an htmx fragment polled via `hx-get="/jobs/table" hx-trigger="every 3s"`.
- **Pause polling while any row checkbox is selected**, so a batch selection is not wiped by a swap.
- **Emit no polling trigger when every job is in a terminal state** (`DOWNLOADED`, `NOT_FOUND`, `FAILED`, `CANCELLED`, `DUPLICATE`), so an idle page goes quiet. A submit or row action re-arms polling.

Minimum UI flows:
- Create batch request
- View queue/history
- Check status per movie request
- Retry failed request
- Edit query and retry for `NOT_FOUND`
- Edit query and/or force retry for `DUPLICATE`
- Cancel a queued task
- Remove task and related file
- Force duplicate request
- Run batch actions for selected tasks

### 6) Request Processing Policies

#### Query normalization
The normalized query is used for duplicate detection and is derived as:
1. Unicode NFC normalize
2. Lowercase
3. Strip punctuation
4. Collapse internal whitespace runs to a single space
5. Trim

#### Duplicate handling policy
- A request is a duplicate when its normalized query matches an existing request that reached **`DOWNLOADED`**. Requests in `NOT_FOUND`, `FAILED`, `CANCELLED`, or `DUPLICATE` are **ignored** by the check — the same title can be resubmitted freely after a failure.
- A duplicate is **always persisted as a task** with status `DUPLICATE`, never silently discarded. It can then be edited and/or forced from the list.
- UI must expose an explicit `Force` action to bypass the check for a specific request.
- `DUPLICATE_CHECK_ENABLED=false` disables the check globally.

#### Matching policy
Selection is deterministic and **ignores seeders entirely** — results are frequently cross-posted from other trackers with wrong or stale swarm counts. There is **no minimum-seeders filter** and **no language preference**; tracker priority replaces language ranking.

Candidates from **all trackers are merged into one list** and ordered by, in strict precedence:
1. **Quality tier** — `2160p` > `1080p` > `720p` > `sd`
2. **Codec** — H.265/HEVC > H.264/AVC > anything else (better compression at equal size)
3. **Tracker priority** (`TRACKER_<SLUG>_PRIORITY`, lower first)
4. **Larger `SizeBytes`** (proxy for bitrate, still bounded by `TRACKER_<SLUG>_MAX_SIZE_BYTES`)
5. **First-seen order** (stable sort, so ties are reproducible)

The picture wins over its source: a 2160p release on the second-choice tracker beats a 1080p one on the first, and tracker priority only separates candidates that are otherwise equally good. Priority does outrank size, so a smaller release on the preferred tracker wins at equal quality and codec.

**Canonical quality token** — parsed from the release title and normalized, because it is embedded in the saved filename:

| Token | Matches (case-insensitive) |
|---|---|
| `2160p` | `2160p`, `4k`, `uhd` |
| `1080p` | `1080p`, `fullhd`, `full hd` |
| `720p` | `720p`, `hd` |
| `sd` | everything else |

**Canonical codec token:** `h265` (`h.265`, `h265`, `x265`, `hevc`), `h264` (`h.264`, `h264`, `x264`, `avc`), otherwise `other`.

#### Saved filename
```
<title>-<tracker>-<quality>-<requestID>.torrent
```
e.g. `dune part two-mazepa-2160p-01JQ8X4M7ZK3RN.torrent`

- `<title>` is the cleaned release title: letters, digits, `-` and `.` are kept, **every other character becomes a space**, whitespace runs collapse, and only the **first 7 words** survive (140-char backstop). Tracker titles carry the whole release description, and everything past the first few words is either noise or already captured by the quality token:

  ```
  Сікаріо 2 / Sicario: Day of the Soldado (2018) UHD BDRemux 4K 2160p HDR 2xUkr/Eng | Sub Eng
  -> Сікаріо 2 Sicario Day of the Soldado-mazepa-2160p-01KYYJA6CMF8N3Q77NAM0VJ4DQ.torrent
  ```
- `<tracker>` is the slug of the tracker the release was downloaded from — the winner of the cross-tracker ranking, not a fixed value.
- `<requestID>` is the task's ULID — sortable, filename-safe, and **guarantees one file per task**, so removing a task never deletes another task's file.
- Write to a temp file in the same directory, then `os.Rename` into place.
- Deletion on task removal must verify the resolved path is inside `TORRENT_FILES_DIR` before unlinking.

### 7) Concurrency and Rate Limiting
- **One shared pool of `WORKERS` workers** (default 5). A worker takes a request and searches every tracker in parallel, so the number is per request in flight, not per tracker.
- Every tracker has **its own rate limiter** (token bucket at `TRACKER_<SLUG>_RPS`) and **its own cookie session**, shared by all workers. The limiter, not per-request sleeps, is what protects the tracker — and it keeps the fan-out from multiplying load on any one site.
- The tracker client must **not** hold a global mutex across search/download — that would serialize the workers into 1. Only session establishment is guarded, via singleflight, so several workers hitting an expired session trigger exactly one re-login.
- Per-request timeout via `TRACKER_<SLUG>_TIMEOUT_SECONDS` and `context.Context`; one attempt is bounded by four times the **longest** configured tracker timeout, since the trackers are searched in parallel.

### 8) Retry Policy
- Max 5 attempts (`RETRY_MAX_ATTEMPTS`).
- Exponential backoff with jitter, starting at 3s: 3s → 6s → 12s → 24s → 48s, capped at `RETRY_MAX_BACKOFF_SECONDS` (60s).
- Retries are **persisted**, not slept in memory: the task row carries `attempt_count` and `next_attempt_at`, and the worker picks up due tasks. This is what makes retries survive a restart.
- **Retryable:** network errors, timeouts, HTTP 5xx, HTTP 429. Auth failure triggers one re-login, then fails.
- **Not retryable:** `NOT_FOUND` (no results is an answer, not a failure).

### 9) Persistent Storage for Requests/Statuses
Store movie search requests and lifecycle state in SQLite.

Minimum data to keep:
- Request ID (ULID)
- Movie title (raw input)
- Normalized query
- Current status
- Created/updated timestamps (UTC)
- Last error (if any)
- Attempt count, `next_attempt_at`
- Tracker name — the tracker whose release was selected. Empty until `FOUND`: every tracker is searched, so a queued request belongs to none of them.
- Tracker result metadata (matched torrent name, size, quality token, codec token, topic URL)
- Saved file path (if downloaded)
- Batch ID (groups the tasks created by one submission)

A second table, `trakt_movies`, records which watchlist movies have already been scheduled (trakt movie id, watchlist entry id, title, year, `listed_at`, the request it produced). See section 10.

Statuses:

| Status | Meaning | Terminal |
|---|---|---|
| `NEW` | created, not yet queued | no |
| `QUEUED` | waiting for a worker (includes waiting on `next_attempt_at`) | no |
| `SEARCHING` | worker is querying the tracker | no |
| `FOUND` | matching topic selected, `.torrent` not yet fetched — a **transition step**, never a resting state | no |
| `DOWNLOADED` | `.torrent` saved to `TORRENT_FILES_DIR` | yes |
| `NOT_FOUND` | tracker returned no usable match | yes |
| `FAILED` | retries exhausted | yes |
| `DUPLICATE` | rejected by the uniqueness check | yes |
| `CANCELLED` | cancelled by the operator before it started | yes |

Cancellation: allowed from `NEW` and `QUEUED` only. **In-flight tasks (`SEARCHING`/`FOUND`) are not cancellable** — they run to completion.

### 10) Trakt.tv Watchlist Sync

An optional background worker that turns a trakt.tv watchlist into requests. It is a *source* of requests only: what happens to them afterwards — ranking, retries, duplicate handling, the job table — is unchanged.

**API contract** (per [trakt's required headers](https://docs.trakt.tv/docs/required-headers) and [get watchlist](https://docs.trakt.tv/reference/getsyncwatchlistget)):

- `GET {TRAKT_BASE_URL}/sync/watchlist/movies/listed_at/desc?page=<n>&limit=<TRAKT_PAGE_LIMIT>`
- Headers on every call: `Content-Type: application/json`, `trakt-api-version: 2`, `trakt-api-key: <TRAKT_CLIENT_ID>`, `Authorization: Bearer <access token>`.
- `X-Pagination-Page-Count` ends the walk; a short page is the fallback when the header is absent.
- `401`/`403` triggers one token refresh and retry of the pass (see "Access token" below); if it recurs after that, it is reported as a rejected credential and the pass gives up until the next tick — only re-authorizing the plugin in jellyfin can fix that. Everything else is logged and retried on the next interval.

**Access token.** Read from the Emby/Jellyfin trakt plugin's own configuration, over the jellyfin API: `GET {JELLYFIN_HOST}/Plugins/4fe3201ed6ae4f2e8917e12bda571281/Configuration` with `Authorization: MediaBrowser Token="{JELLYFIN_API_KEY}"`. Nothing is cached — the configuration is re-read **before every sync**, so a refresh performed by the plugin is picked up without a restart. The account used is `JELLYFIN_USER_ID` matched against `LinkedMbUserId` (dashes and case ignored), or, when that is unset, the first entry carrying an access token — the document identifies media-server users, not trakt applications.

A recorded `AccessTokenExpiration` that has passed, or is within an hour of passing, is refreshed here rather than left to trakt to reject: `POST {TRAKT_BASE_URL}/oauth/token` with `grant_type=refresh_token` and `TRAKT_CLIENT_ID`/`TRAKT_CLIENT_SECRET`, exactly as the plugin itself would. The new pair is written back with a `POST` to the same Configuration endpoint — the document is round-tripped as raw JSON, so the plugin's other settings survive untouched — and the recorded expiry is set to `now + expires_in * 3/4`, the same margin the plugin uses. If trakt rejects a token the plugin's copy claimed was still good, the sync refreshes once and retries the pass before giving up until the next tick. If jellyfin cannot store a refreshed pair, it is held in memory rather than lost, and the save is retried on the next sync.

The `TRAKT_CLIENT_ID`/`TRAKT_CLIENT_SECRET` pair must be the one that issued the plugin's refresh token — for a stock install that is the plugin's own compiled-in pair (`Trakt/Api/TraktURIs.cs` in the plugin's source), not an operator's own trakt application, unless the plugin was re-authorized against one.

**Scheduling.** Every entry becomes a normal request: `RawTitle` is `Title (Year)`, the query is `Title Year` (or just `Title` when `TRAKT_QUERY_WITH_YEAR=false`), and the normalized query drives the usual duplicate check. Entries are queued oldest-addition-first.

**Never twice.** A processed movie is recorded in `trakt_movies`, keyed by the **trakt movie id** rather than the watchlist entry id: removing a movie from the watchlist and adding it again mints a new entry id and must not read as a new movie. The record is written whether the request was queued or rejected as a duplicate — in both cases the entry has been dealt with — and in the same transaction as the request itself, so the two can never disagree.

**Bounded rescan.** Sorting by `listed_at desc` is what keeps the scan shallow: the walk stops at the first entry older than `MAX(listed_at)` of the movies already processed. That cursor is derived from the rows rather than stored separately, so it cannot drift away from what was actually done. Entries *equal* to the cursor are re-read on purpose (a bulk add gives several entries the same timestamp) and dropped by movie id. Steady state is one API call per interval.

**Failure handling.** A run is all-or-nothing: if a page fails part-way, nothing is queued, because a half-applied run would move the cursor past entries that were never scheduled. A run that stops at `TRAKT_MAX_PAGES` says so in the log rather than silently reporting success.

**Healthcheck signalling.** Nothing runs in front of a background worker, so the sync reports itself to a healthchecks.io-style monitor when `TRAKT_HEALTHCHECK_UUID` is set. Unset is not an error — it means no signals are sent at all, and the sync behaves identically otherwise.

- Success pings `<base>/<uuid>` after every successful run, with the run's counts as the ping body (the monitor's event log).
- Failure is **counted, not reported**: consecutive failures below 5 are logged and left for the next run, and the 5th consecutive failure pings `<base>/<uuid>/fail` with the streak length and the last error. Every further failure keeps pinging, so the check stays down. A successful run resets the counter.
- A context cancelled by shutdown is neither a success nor a failure, and must not move the counter.
- The counter is in memory: a restart starts a fresh streak, which is correct — a restarted process has not failed 5 times.
- A ping is retried 3 times, 2s apart, with a 10s timeout. An unreachable monitor is a warning in the log and never propagates: the job it reports on has already done its work. This is why the monitor should also carry a period and grace period — the one thing a self-report cannot cover is the sync not running at all.

---

## Non-Functional Requirements

### Reliability
- Idempotent scheduling support (avoid duplicate processing of identical request unless explicitly allowed).
- **Graceful restart:** on startup, every task in `SEARCHING` or `FOUND` is re-queued to `QUEUED` and executed again from the beginning. Tasks are short and cheap to repeat, so there is no mid-task resume. Orphaned temp files in `TORRENT_FILES_DIR` are cleaned at startup.
- Manual retry must be available from UI list for failed jobs.
- For `NOT_FOUND`, operator must be able to edit query and retry from UI list.

### Performance / Rate-Limiting
See §7. Configurable worker concurrency, shared per-tracker throttle, per-request timeouts.

### Security
- Do not expose credentials in logs or UI.
- HTTP Basic auth for UI/API, enabled when `AUTH_USER` and `AUTH_PASSWORD` are both set.
- Validate/sanitize user input from batch textarea (line count, trimming, normalization).
- Handle tracker sessions/cookies securely; cookie jar stays in memory, never persisted to disk or logged.
- Path-traversal guard on any file deletion.

### Observability
- Structured logs (`slog`, JSON) with request IDs.
- Health endpoints:
    - `/health/live` — process is up
    - `/health/ready` — SQLite is open **and** `TORRENT_FILES_DIR` is writable. **Tracker reachability/login is deliberately excluded** — a tracker outage must not kill the container.
- Optional metrics endpoint (Prometheus format preferred).

### Portability / Operations
- Container runs with mounted volumes for torrent output and the SQLite DB.
- Timezone configurable via `TZ`; storage is always UTC.
- Provide sample `.env.example`.
- Provide explicit Docker run / compose examples.
- DB backup/restore is not required for MVP; potential DB loss is acceptable.
- Anti-bot/CAPTCHA bypass strategies are out of scope for now.

### Testing
No formal test suite is required for MVP. This is a self-hosted personal tool, validated by manual use. Pure functions (query normalization, quality/codec parsing, ranking, size parsing) get lightweight table-driven tests where convenient.

Two exceptions earn their upkeep, both added with the second tracker:
- **Saved tracker pages in `html-examples/<slug>/`.** With more than one preset, the selectors are the thing most likely to be silently wrong, and a real page is the only honest input. The parser tests assert row counts, resolved URLs, sizes and the session selectors against them.
- **Cross-tracker selection** (`internal/worker`). Fake trackers over `httptest` cover the decisions that only exist once several trackers are searched: quality beating the preferred tracker, priority breaking a tie, a partial failure still downloading, and every tracker failing being retryable rather than `NOT_FOUND`.

---

## Suggested Technical Scope (MVP)

### MVP includes
- Tracker integration (`mazepa.to`, `toloka.to`), preset-driven
- Batch input UI (htmx, polled table)
- Queue + 5 workers, each searching every tracker behind its own rate limiter
- Persistent status tracking in SQLite with DB-backed retries
- Save `.torrent` files to configured dir
- Docker image + env-based config + PUID/PGID

### Post-MVP (nice to have)
- JSON API endpoints for automation
- Notification hooks (email/Telegram/webhook)
- Manual selection among search results
- History retention/cleanup policy

---

## Acceptance Criteria

- Given valid tracker credentials and movie list, user can submit batch request via web UI.
- Each movie request receives a persisted status transition until terminal state.
- Duplicate requests are persisted as `DUPLICATE`, with explicit `Force` and edit-query actions to override.
- Every configured tracker is searched for each request, and the results are ranked as one list: quality (`2160p`/`1080p`/`720p`/`sd`) → codec (H.265 > H.264) → tracker priority → larger size, ignoring seeders.
- One tracker being unreachable still yields a download when another tracker has a usable release.
- For `NOT_FOUND`, user can edit search query and retry from request list.
- User can cancel a queued task, remove a task, and delete its related `.torrent` file.
- User can execute batch actions for selected tasks.
- Downloaded `.torrent` files appear in `TORRENT_FILES_DIR` named `<title>-<tracker>-<quality>-<requestID>.torrent`.
- Failed requests retry up to 5 times with exponential backoff, surviving a container restart.
- Service runs correctly in Docker as a non-root user with only env-file and mounted volumes.
- Restart re-queues in-flight tasks and does not lose request history or corrupt queue state.

---

## Out of Scope (for now)
- Downloading actual movie content via BitTorrent client.
- Media library management.
- Advanced user/role management.
- Automated tests that hit the live trackers over the network. (Tests against **saved** pages in `html-examples/` are in scope — see Testing.)