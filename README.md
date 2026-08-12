# Movie Torrent Finder

Searches one or more torrent trackers for a list of movie titles and saves the
matching `.torrent` files to a directory a download client watches. It never
downloads the movies themselves.

Built for Synology Docker, but nothing in it is Synology-specific.

See [AGENTS.md](AGENTS.md) for the full specification.

## What it does

1. You paste a list of movie titles into a web form (one per line).
2. Each line becomes a queued request.
3. Five workers take the requests. Each one searches **every configured tracker
   in parallel**, ranks the merged results, and saves the winner's `.torrent`
   file as `<title>-<tracker>-<quality>-<requestID>.torrent`.
4. The job table shows every request until it reaches a terminal state, with
   retry, edit-query, force, cancel and remove actions.

Release selection is deterministic: **quality tier** (`2160p` > `1080p` >
`720p` > `sd`), then **codec** (H.265 > H.264), then **tracker priority**, then
**larger file**. The picture wins over its source — a 2160p release on the
second-choice tracker beats a 1080p one on the first — and priority only
separates candidates that are otherwise equal. Seeder counts are ignored on
purpose: cross-posted results carry unreliable swarm numbers.

A tracker that is unreachable does not hold back a release another tracker
found; the failure is logged and the remaining results are ranked as usual.

## Supported trackers

Trackers are configured by slug, and each slug carries a **preset**: the paths,
form fields, selectors and column layout of that site. Adding a supported
tracker means listing its slug and supplying credentials.

| Preset | Site | Notes |
|---|---|---|
| `toloka` | toloka.to | phpBB2 engine; search requires a session |
| `mazepa` | mazepa.to | TorrentPier |
| `torrentpier` | — | the generic engine, for any other TorrentPier install |

```sh
TRACKERS=toloka,mazepa
TRACKER_TOLOKA_LOGIN=…
TRACKER_TOLOKA_PASSWORD=…
TRACKER_TOLOKA_PRIORITY=1
TRACKER_MAZEPA_LOGIN=…
TRACKER_MAZEPA_PASSWORD=…
TRACKER_MAZEPA_PRIORITY=2
```

Any preset value can be corrected without a code change via
`TRACKER_<SLUG>_EXTRA_OPTIONS` (see `.env.example`). Leaving `TRACKERS` unset
falls back to the legacy single-tracker layout on unprefixed `TRACKER_*`
variables, so an existing `.env` keeps working.

## Trakt.tv watchlist

A background worker can poll a [trakt.tv](https://trakt.tv) watchlist and queue
every new movie for download, exactly as if it had been typed into the form.

```sh
TRAKT_ENABLED=true
TRAKT_CLIENT_ID=…              # your trakt application's client id
TRAKT_CLIENT_SECRET=…          # your trakt application's client secret
JELLYFIN_HOST=http://jellyfin:8096
JELLYFIN_API_KEY=…             # Dashboard -> Advanced -> API Keys
TRAKT_INTERVAL_MINUTES=15
```

It reads `GET /sync/watchlist/movies/listed_at/desc` with the headers trakt
requires (`trakt-api-key`, `trakt-api-version: 2`, `Content-Type` and the bearer
token) and turns each entry into a normal request — same queue, same ranking,
same job table.

**The access token comes from the Emby/Jellyfin trakt plugin, not from this
service.** That plugin owns the OAuth grant; this service reads it out of the
plugin's configuration with `GET /Plugins/4fe3201ed6ae4f2e8917e12bda571281/Configuration`
before every sync, so a refresh performed by the plugin is picked up without a
restart. When the recorded expiry has passed or is close, this service
refreshes the token itself — using `TRAKT_CLIENT_SECRET`, the same credentials
the plugin would use — and writes the new pair back to that same endpoint, so
the plugin's own copy stays current too. Mint the API key at Jellyfin's
**Dashboard → Advanced → API Keys → +**.

**Each movie is scheduled once.** Processed movies are recorded by their trakt
movie id, so removing a title from the watchlist and adding it back does not
download it again. Because the list is read newest-addition-first, a sync stops
at the last entry it already knows: the steady-state cost is a single API call
every 15 minutes, whatever the length of the watchlist. A first import longer
than `TRAKT_PAGE_LIMIT × TRAKT_MAX_PAGES` (10 000 movies by default) continues
on the following runs.

### Monitoring the sync

Nothing runs in front of a background worker, so set `TRAKT_HEALTHCHECK_UUID` to
a [healthchecks.io](https://healthchecks.io) check id (or your own installation,
via `TRAKT_HEALTHCHECK_BASE_URL`) and the sync will report itself:

- every **successful** sync pings `<base>/<uuid>`, with the counts as the event
  log entry;
- **failures are counted, not reported** — the first four in a row are logged
  and left to the next run, and only the fifth pings `<base>/<uuid>/fail`. A
  success resets the count. At the default interval that means an alert stands
  for "the watchlist has been unread for over an hour", not for one bad request;
- a shutdown is neither, and pings are retried three times so a blip reaching
  the monitor cannot raise a false alarm.

Leave the UUID unset and no signals are sent at all. Give the check a period of
`TRAKT_INTERVAL_MINUTES` and a grace period of a few intervals, so the monitor
also catches the case this cannot report on itself: the sync not running.

Queries include the year — `Extraction 2020` — because that is what separates
two films sharing a title; set `TRAKT_QUERY_WITH_YEAR=false` if a tracker's
search matches titles literally and the year costs matches. Anything the
trackers cannot find lands in the job table as `NOT_FOUND`, where the query can
be edited and retried as usual.

## Quick start (Docker Compose)

```sh
cp .env.example .env
# set the tracker credentials, and PUID/PGID to the owner of your shares
docker compose up -d
```

Open <http://localhost:8080>.

## Quick start (docker run)

```sh
docker build -t movie-torrent-finder .

docker run -d \
  --name movie-torrent-finder \
  --restart unless-stopped \
  --env-file .env \
  -p 8080:8080 \
  -v /volume1/downloads/torrents:/torrents \
  -v /volume1/docker/mtd/db:/data \
  movie-torrent-finder
```

### Synology notes

- `PUID`/`PGID` must match the owner of the mounted shares, or the container
  cannot write to them. Find them over SSH with `id your-user`.
- Mount the `.torrent` output at a path your download client watches.
- `/data` holds the SQLite database. Losing it loses request history, not the
  saved files.

## Local development (no Docker)

```sh
cp .env.example .env
# point TORRENT_FILES_DIR and DB_PATH at local directories
go run ./cmd/server
```

`.env` is loaded automatically by godotenv; real environment variables take
precedence over it.

```sh
go test ./...     # ranking, normalization and the tracker pipeline
go vet ./...
```

## CI and published images

`.github/workflows/ci.yml` runs on every push and pull request:

1. **Test** — `gofmt` check, `go vet`, `go test -race`, `go build`.
2. **Docker** — builds `linux/amd64` and `linux/arm64` and pushes to GHCR.

Pull requests build the image but never publish it. Pushes to the default
branch publish `latest`; version tags (`v1.2.3`) publish `1.2.3` and `1.2`.
Every build is also tagged with its short commit SHA.

Pull a published image with:

```sh
docker pull ghcr.io/<owner>/movie-torrent-finder:latest
```

Packages are private by default — make the package public (or log the NAS in
with a personal access token that has `read:packages`) before pulling from
Synology.

## Configuration

Every setting is an environment variable — see [.env.example](.env.example) for
the annotated list. The ones that matter most:

| Variable | Default | Purpose |
|---|---|---|
| `TORRENT_FILES_DIR` | *required* | where `.torrent` files are written |
| `TRACKERS` | unset | comma-separated tracker slugs; unset = legacy single tracker |
| `TRACKER_<SLUG>_LOGIN` / `_PASSWORD` | *required* | that tracker's credentials |
| `TRACKER_<SLUG>_PRIORITY` | `1` | lower wins; breaks ties between equal releases |
| `TRACKER_<SLUG>_BASE_URL` | from preset | tracker root URL |
| `DB_PATH` | `/data/app.db` | SQLite file |
| `WORKERS` | `5` | requests searched at once, across all trackers |
| `TRACKER_<SLUG>_RPS` | `1` | request rate for that tracker |
| `AUTH_USER` / `AUTH_PASSWORD` | unset | enables basic auth when both are set |
| `PUID` / `PGID` | `1000` | container user, must own the mounts |
| `TRAKT_ENABLED` | `false` | poll a trakt.tv watchlist for movies |
| `TRAKT_CLIENT_ID` | unset | trakt application client id, sent as `trakt-api-key` |
| `TRAKT_CLIENT_SECRET` | unset | trakt application client secret, used to refresh the token |
| `JELLYFIN_HOST` | unset | jellyfin holding the trakt plugin, e.g. `http://jellyfin:8096` |
| `JELLYFIN_API_KEY` | unset | Dashboard -> Advanced -> API Keys |
| `JELLYFIN_USER_ID` | unset | linked user to sync, by `LinkedMbUserId`; unset = first with a token |
| `JELLYFIN_TIMEOUT_SECONDS` | `30` | per-request timeout for the jellyfin API |
| `TRAKT_INTERVAL_MINUTES` | `15` | how often the watchlist is polled |
| `TRAKT_HEALTHCHECK_UUID` | unset | healthchecks.io check id; unset = no signals |

## Endpoints

| Path | Purpose |
|---|---|
| `/` | operator UI |
| `/health/live` | process is up |
| `/health/ready` | database open **and** output directory writable |

Readiness deliberately excludes tracker reachability, so a tracker outage does
not restart the container.

## Behaviour worth knowing

- **Duplicates** are rejected only against requests that reached `DOWNLOADED`.
  A rejected line is still saved with status `DUPLICATE` so you can edit its
  query or force it. Failed and not-found titles can be resubmitted freely.
- **Retries** are persisted (`next_attempt_at`), not slept in memory: 5
  attempts, 3s → 6s → 12s → 24s → 48s with jitter, capped at 60s. A restart
  resumes the schedule.
- **Restart** re-queues anything that was mid-flight and deletes orphaned
  temporary files. Tasks are cheap to repeat, so they rerun from the start.
- **Cancel** applies to `NEW` and `QUEUED` only; in-flight work runs to
  completion.
- **The job table stops polling** when nothing is in flight, and pauses while
  rows are selected so a refresh cannot wipe a batch selection.

## Troubleshooting

**Nothing is found and every request fails.** Each preset targets one table
layout. If a tracker changes its markup, override the selectors with
`TRACKER_<SLUG>_EXTRA_OPTIONS` (see `.env.example`) rather than editing code.
Saved copies of the pages the presets were written against live in
`html-examples/`, and the parser tests run against them.

**One tracker stopped contributing.** Look for `tracker unavailable, ranking the
remaining results` in the logs: results from the other trackers are still used,
so requests keep completing while one source is broken.

**Permission denied writing torrents.** `PUID`/`PGID` do not match the share
owner. `/health/ready` reports this directly.

**The trakt watchlist is not producing anything.** Every sync logs one line
(`trakt watchlist synced`) with what it scanned and queued. `trakt rejected the
credentials` after a refresh means the grant is gone — re-authorize the trakt
plugin in Jellyfin. Jellyfin being unreachable or the API key being wrong is
logged the same way and retried on the next interval, so the container keeps
serving the UI either way.

**Login fails.** Check the credentials first; if they are right, the login form
field names may have changed — they are overridable in
`TRACKER_<SLUG>_EXTRA_OPTIONS`, including any extra checkbox the form submits
(`login_extra_fields`).
