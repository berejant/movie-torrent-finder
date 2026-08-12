// Package config loads and validates all runtime configuration.
//
// Values come from the process environment. For local development outside
// Docker a .env file in the working directory is loaded first, best effort:
// a missing file is not an error, and real environment variables always win
// over .env entries.
package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/caarlos0/env/v10"
	"github.com/joho/godotenv"
)

// reUUID matches the canonical 8-4-4-4-12 form, which is what a healthchecks
// check id is.
var reUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// DefaultUserAgent makes tracker requests look like a current desktop Chrome.
const DefaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) " +
	"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"

// Config is the fully resolved application configuration.
type Config struct {
	HTTPPort        int    `env:"HTTP_PORT" envDefault:"8080"`
	TorrentFilesDir string `env:"TORRENT_FILES_DIR,required"`
	DBPath          string `env:"DB_PATH" envDefault:"/data/app.db"`
	TZ              string `env:"TZ" envDefault:"UTC"`
	LogLevel        string `env:"LOG_LEVEL" envDefault:"info"`
	BatchMaxLines   int    `env:"BATCH_MAX_LINES" envDefault:"100"`

	AuthUser     string `env:"AUTH_USER"`
	AuthPassword string `env:"AUTH_PASSWORD"`

	DuplicateCheckEnabled bool `env:"DUPLICATE_CHECK_ENABLED" envDefault:"true"`

	// Workers is the size of the shared worker pool: how many requests are
	// searched at once. It is not per tracker — one request queries every
	// tracker — and per-tracker load stays bounded by that tracker's RPS.
	Workers int `env:"WORKERS" envDefault:"5"`

	// TrackerNames lists the enabled tracker slugs in configuration order.
	// Empty selects the legacy single-tracker layout (unprefixed TRACKER_*).
	TrackerNames []string `env:"TRACKERS" envSeparator:","`

	// Trackers is TrackerNames resolved against the presets and the
	// TRACKER_<SLUG>_* variables.
	Trackers []Tracker `env:"-"`

	Retry Retry `envPrefix:"RETRY_"`

	Trakt Trakt `envPrefix:"TRAKT_"`

	Jellyfin Jellyfin `envPrefix:"JELLYFIN_"`
}

// Trakt configures the background sync that turns a trakt.tv watchlist into
// download requests. It is off unless TRAKT_ENABLED is set, so an existing
// deployment is unaffected by the upgrade.
type Trakt struct {
	Enabled bool `env:"ENABLED" envDefault:"false"`

	// ClientID is the trakt application client id, sent as trakt-api-key.
	ClientID string `env:"CLIENT_ID"`

	// ClientSecret is the matching secret. It is only used to refresh the
	// access token, and it must belong to the same application as the refresh
	// token the plugin stored — for a stock plugin install, that is the
	// plugin's own compiled-in pair.
	ClientSecret string `env:"CLIENT_SECRET"`

	BaseURL string `env:"BASE_URL" envDefault:"https://api.trakt.tv"`

	// IntervalMinutes is how often the watchlist is polled. The first sync runs
	// at startup, not after the first interval.
	IntervalMinutes int `env:"INTERVAL_MINUTES" envDefault:"15"`

	TimeoutSeconds int `env:"TIMEOUT_SECONDS" envDefault:"30"`

	// PageLimit is the watchlist page size. MaxPages bounds one sync, so an
	// enormous watchlist cannot turn a single run into an unbounded crawl; the
	// remainder is picked up on the following runs.
	PageLimit int `env:"PAGE_LIMIT" envDefault:"100"`
	MaxPages  int `env:"MAX_PAGES" envDefault:"10"`

	// HealthcheckUUID is the check id at healthchecks.io (or a self-hosted
	// install) that the sync signals. Unset means no signals are sent at all:
	// the sync is not something the operator watches, so it either reports to a
	// monitor or goes unmonitored.
	HealthcheckUUID string `env:"HEALTHCHECK_UUID"`

	// HealthcheckBaseURL is the ping endpoint; override it for a self-hosted
	// healthchecks install.
	HealthcheckBaseURL string `env:"HEALTHCHECK_BASE_URL" envDefault:"https://hc-ping.com"`

	// QueryWithYear appends the release year to the search query. It is on by
	// default because the year is what separates two movies sharing a title:
	// without it the duplicate check would reject the remake of a film that was
	// already downloaded. Turn it off if a tracker's search matches titles
	// literally and the year costs matches.
	QueryWithYear bool `env:"QUERY_WITH_YEAR" envDefault:"true"`
}

// Interval returns the polling period.
func (t Trakt) Interval() time.Duration {
	return time.Duration(t.IntervalMinutes) * time.Minute
}

// Timeout returns the per-request timeout for the trakt API.
func (t Trakt) Timeout() time.Duration {
	return time.Duration(t.TimeoutSeconds) * time.Second
}

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

// Tracker holds the configuration of a single tracker source. Its variables are
// read from TRACKER_<SLUG>_*, or from plain TRACKER_* in the legacy layout.
type Tracker struct {
	Name string `env:"NAME"`
	// Preset selects the built-in selector profile; see presets.go. When unset
	// the tracker slug is tried as a preset name before falling back to
	// DefaultPresetName.
	Preset         string  `env:"PRESET"`
	BaseURL        string  `env:"BASE_URL"`
	Login          string  `env:"LOGIN"`
	Password       string  `env:"PASSWORD"`
	Priority       int     `env:"PRIORITY" envDefault:"1"`
	TimeoutSeconds int     `env:"TIMEOUT_SECONDS" envDefault:"30"`
	RPS            float64 `env:"RPS" envDefault:"1"`
	MaxSizeBytes   int64   `env:"MAX_SIZE_BYTES" envDefault:"0"`
	UserAgent      string  `env:"USER_AGENT"`

	// ExtraOptions is the raw JSON blob from TRACKER_<SLUG>_EXTRA_OPTIONS.
	ExtraOptions string `env:"EXTRA_OPTIONS"`

	// Options is ExtraOptions parsed and merged over the preset.
	Options TrackerOptions `env:"-"`

	// EnvPrefix is where this tracker's variables were read from, so validation
	// messages can name the variable the operator actually has to fix.
	EnvPrefix string `env:"-"`
}

// Timeout returns the per-request timeout.
func (t Tracker) Timeout() time.Duration {
	return time.Duration(t.TimeoutSeconds) * time.Second
}

// TrackerOptions are the tracker-specific paths, form fields and selectors.
// Every field is overridable because any tracker theme or engine version can
// differ; the built-in profiles live in presets.go.
type TrackerOptions struct {
	TrackerPath string `json:"tracker_path"`
	LoginPath   string `json:"login_path"`

	LoginUsernameField string `json:"login_username_field"`
	LoginPasswordField string `json:"login_password_field"`
	LoginSubmitField   string `json:"login_submit_field"`
	LoginSubmitValue   string `json:"login_submit_value"`

	SearchQueryField string `json:"search_query_field"`

	LoggedInSelector  string `json:"logged_in_selector"`
	LoggedOutSelector string `json:"logged_out_selector"`

	// LoginExtraFields are additional form fields posted with the credentials,
	// such as toloka's autologin checkbox. Keys are sent verbatim.
	LoginExtraFields map[string]string `json:"login_extra_fields"`

	ResultRowSelector    string `json:"result_row_selector"`
	TopicLinkSelector    string `json:"topic_link_selector"`
	DownloadLinkSelector string `json:"download_link_selector"`
	ForumLinkSelector    string `json:"forum_link_selector"`

	// SizeCellIndex is the zero-based <td> holding the size and the download
	// link. TorrentPier renders 0 publish, 1 status, 2 forum, 3 topic,
	// 4 author, 5 size/download, 6 seeders, 7 leechers, 8 replies, 9 added,
	// but column order is the knob most likely to differ between trackers.
	// A negative value skips the direct lookup and scans the whole row.
	SizeCellIndex int `json:"size_cell_index"`
}

// DefaultTrackerOptions returns the TorrentPier profile, which is what a
// tracker gets when no preset matches.
func DefaultTrackerOptions() TrackerOptions {
	return torrentPierOptions()
}

// Retry describes the persisted retry policy for transient failures.
type Retry struct {
	MaxAttempts       int `env:"MAX_ATTEMPTS" envDefault:"5"`
	BaseSeconds       int `env:"BASE_SECONDS" envDefault:"3"`
	MaxBackoffSeconds int `env:"MAX_BACKOFF_SECONDS" envDefault:"60"`
}

// Load reads .env (if present), binds the environment and validates the result.
func Load() (Config, error) {
	// Best effort: in Docker the values come from the env-file or compose, so
	// an absent .env is the normal case and must not fail startup. Load never
	// overwrites variables that are already set.
	if err := godotenv.Load(); err != nil {
		slog.Debug("no .env file loaded, using process environment", "err", err)
	}

	var cfg Config
	if err := env.Parse(&cfg); err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}

	trackers, err := loadTrackers(cfg.TrackerNames)
	if err != nil {
		return Config{}, err
	}
	cfg.Trackers = trackers

	// TRACKER_WORKERS used to size the pool back when a pool served one
	// tracker. Honour it while WORKERS is unset so an existing deployment keeps
	// its concurrency after the upgrade.
	if os.Getenv("WORKERS") == "" {
		if legacy := os.Getenv("TRACKER_WORKERS"); legacy != "" {
			workers, err := strconv.Atoi(strings.TrimSpace(legacy))
			if err != nil {
				return Config{}, fmt.Errorf("config: TRACKER_WORKERS %q is not a number", legacy)
			}
			cfg.Workers = workers
		}
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// trackerEnvPrefix is where every tracker variable starts. A named tracker adds
// its slug: TRACKERS=toloka reads TRACKER_TOLOKA_LOGIN.
const trackerEnvPrefix = "TRACKER_"

// loadTrackers resolves the TRACKERS list into full configurations. An empty
// list means the legacy layout, where a single tracker is configured with
// unprefixed TRACKER_* variables.
func loadTrackers(names []string) ([]Tracker, error) {
	slugs := make([]string, 0, len(names))
	for _, name := range names {
		if slug := strings.ToLower(strings.TrimSpace(name)); slug != "" {
			slugs = append(slugs, slug)
		}
	}

	if len(slugs) == 0 {
		tracker, err := loadTracker("", trackerEnvPrefix)
		if err != nil {
			return nil, err
		}
		return []Tracker{tracker}, nil
	}

	seen := make(map[string]struct{}, len(slugs))
	trackers := make([]Tracker, 0, len(slugs))
	for _, slug := range slugs {
		if _, duplicate := seen[slug]; duplicate {
			return nil, fmt.Errorf("config: TRACKERS lists %q more than once", slug)
		}
		seen[slug] = struct{}{}

		tracker, err := loadTracker(slug, trackerEnvPrefix+envSegment(slug)+"_")
		if err != nil {
			return nil, err
		}
		trackers = append(trackers, tracker)
	}
	return trackers, nil
}

// loadTracker binds one tracker's variables and merges its preset. slug is
// empty in the legacy layout, where the name comes from TRACKER_NAME instead.
func loadTracker(slug, prefix string) (Tracker, error) {
	var tracker Tracker
	if err := env.ParseWithOptions(&tracker, env.Options{Prefix: prefix}); err != nil {
		return Tracker{}, fmt.Errorf("config: %w", err)
	}
	tracker.EnvPrefix = prefix

	if slug != "" {
		// The slug is the identity: it picks the variables, the preset and the
		// tracker token in saved filenames, so NAME cannot disagree with it.
		tracker.Name = slug
	} else if strings.TrimSpace(tracker.Name) == "" {
		tracker.Name = DefaultPresetName
	}

	requested := strings.ToLower(strings.TrimSpace(tracker.Preset))
	preset, ok := LookupPreset(requested)
	switch {
	case ok:
		tracker.Preset = requested
	case requested != "":
		return Tracker{}, fmt.Errorf("config: %sPRESET %q is unknown (known presets: %s)",
			prefix, tracker.Preset, strings.Join(PresetNames(), ", "))
	default:
		// No explicit preset: try the tracker's own name before the fallback,
		// so a tracker named after its preset needs no PRESET variable.
		if preset, ok = LookupPreset(strings.ToLower(tracker.Name)); ok {
			tracker.Preset = strings.ToLower(tracker.Name)
		} else {
			preset, _ = LookupPreset(DefaultPresetName)
			tracker.Preset = DefaultPresetName
		}
	}

	if strings.TrimSpace(tracker.BaseURL) == "" {
		tracker.BaseURL = preset.BaseURL
	}

	options := preset.Options
	if raw := strings.TrimSpace(tracker.ExtraOptions); raw != "" {
		// Unmarshalling over the populated struct keeps the preset's value for
		// any key the operator did not specify.
		if err := json.Unmarshal([]byte(raw), &options); err != nil {
			return Tracker{}, fmt.Errorf("config: parse %sEXTRA_OPTIONS: %w", prefix, err)
		}
	}
	tracker.Options = options

	if strings.TrimSpace(tracker.UserAgent) == "" {
		tracker.UserAgent = DefaultUserAgent
	}

	return tracker, nil
}

// envSegment turns a tracker slug into the middle of its variable names:
// "my-tracker" becomes TRACKER_MY_TRACKER_LOGIN.
func envSegment(slug string) string {
	var out strings.Builder
	out.Grow(len(slug))
	for _, r := range strings.ToUpper(slug) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			out.WriteRune(r)
			continue
		}
		out.WriteRune('_')
	}
	return out.String()
}

// Validate fails fast with a message naming the offending variable.
func (c Config) Validate() error {
	var problems []string

	if c.HTTPPort < 1 || c.HTTPPort > 65535 {
		problems = append(problems, fmt.Sprintf("HTTP_PORT must be 1-65535, got %d", c.HTTPPort))
	}
	if strings.TrimSpace(c.TorrentFilesDir) == "" {
		problems = append(problems, "TORRENT_FILES_DIR must not be empty")
	}
	if strings.TrimSpace(c.DBPath) == "" {
		problems = append(problems, "DB_PATH must not be empty")
	}
	if c.BatchMaxLines < 1 {
		problems = append(problems, fmt.Sprintf("BATCH_MAX_LINES must be >= 1, got %d", c.BatchMaxLines))
	}
	switch strings.ToLower(c.LogLevel) {
	case "debug", "info", "warn", "error":
	default:
		problems = append(problems, fmt.Sprintf("LOG_LEVEL must be debug|info|warn|error, got %q", c.LogLevel))
	}
	if _, err := time.LoadLocation(c.TZ); err != nil {
		problems = append(problems, fmt.Sprintf("TZ %q is not a known timezone", c.TZ))
	}

	// Basic auth is optional, but half of a credential pair is always a mistake.
	if (c.AuthUser == "") != (c.AuthPassword == "") {
		problems = append(problems, "AUTH_USER and AUTH_PASSWORD must be set together or not at all")
	}

	if c.Workers < 1 {
		problems = append(problems, fmt.Sprintf("WORKERS must be >= 1, got %d", c.Workers))
	}
	if len(c.Trackers) == 0 {
		problems = append(problems, "at least one tracker must be configured (see TRACKERS)")
	}

	names := make(map[string]struct{}, len(c.Trackers))
	for _, tracker := range c.Trackers {
		prefix := tracker.EnvPrefix

		// A duplicated name would make two trackers indistinguishable in saved
		// filenames and in the result column.
		if _, duplicate := names[tracker.Name]; duplicate {
			problems = append(problems, fmt.Sprintf("two trackers are both named %q", tracker.Name))
		}
		names[tracker.Name] = struct{}{}

		if strings.TrimSpace(tracker.Name) == "" {
			problems = append(problems, prefix+"NAME must not be empty (it is part of saved filenames)")
		}
		if strings.TrimSpace(tracker.Login) == "" {
			problems = append(problems, prefix+"LOGIN must not be empty")
		}
		if strings.TrimSpace(tracker.Password) == "" {
			problems = append(problems, prefix+"PASSWORD must not be empty")
		}

		base, err := url.Parse(strings.TrimRight(tracker.BaseURL, "/"))
		switch {
		case strings.TrimSpace(tracker.BaseURL) == "":
			problems = append(problems, fmt.Sprintf(
				"%sBASE_URL must be set (preset %q supplies no default)", prefix, tracker.Preset))
		case err != nil:
			problems = append(problems, fmt.Sprintf("%sBASE_URL is not a valid URL: %v", prefix, err))
		case base.Scheme != "http" && base.Scheme != "https":
			problems = append(problems, prefix+"BASE_URL must use http or https")
		case base.Host == "":
			problems = append(problems, prefix+"BASE_URL must include a host")
		}

		if tracker.RPS <= 0 {
			problems = append(problems, fmt.Sprintf("%sRPS must be > 0, got %v", prefix, tracker.RPS))
		}
		if tracker.TimeoutSeconds < 1 {
			problems = append(problems, fmt.Sprintf("%sTIMEOUT_SECONDS must be >= 1, got %d", prefix, tracker.TimeoutSeconds))
		}
		if tracker.MaxSizeBytes < 0 {
			problems = append(problems, prefix+"MAX_SIZE_BYTES must be >= 0 (0 means unlimited)")
		}
	}

	if c.Retry.MaxAttempts < 1 {
		problems = append(problems, fmt.Sprintf("RETRY_MAX_ATTEMPTS must be >= 1, got %d", c.Retry.MaxAttempts))
	}
	if c.Retry.BaseSeconds < 1 {
		problems = append(problems, fmt.Sprintf("RETRY_BASE_SECONDS must be >= 1, got %d", c.Retry.BaseSeconds))
	}
	if c.Retry.MaxBackoffSeconds < c.Retry.BaseSeconds {
		problems = append(problems, "RETRY_MAX_BACKOFF_SECONDS must be >= RETRY_BASE_SECONDS")
	}

	// The trakt settings are only worth checking once the sync is switched on;
	// an unused block of empty variables is not a misconfiguration.
	if c.Trakt.Enabled {
		if strings.TrimSpace(c.Trakt.ClientID) == "" {
			problems = append(problems, "TRAKT_CLIENT_ID must be set when TRAKT_ENABLED is true")
		}
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

		base, err := url.Parse(strings.TrimRight(c.Trakt.BaseURL, "/"))
		switch {
		case strings.TrimSpace(c.Trakt.BaseURL) == "":
			problems = append(problems, "TRAKT_BASE_URL must not be empty")
		case err != nil:
			problems = append(problems, fmt.Sprintf("TRAKT_BASE_URL is not a valid URL: %v", err))
		case base.Scheme != "http" && base.Scheme != "https":
			problems = append(problems, "TRAKT_BASE_URL must use http or https")
		case base.Host == "":
			problems = append(problems, "TRAKT_BASE_URL must include a host")
		}

		if c.Trakt.IntervalMinutes < 1 {
			problems = append(problems, fmt.Sprintf("TRAKT_INTERVAL_MINUTES must be >= 1, got %d", c.Trakt.IntervalMinutes))
		}
		if c.Trakt.TimeoutSeconds < 1 {
			problems = append(problems, fmt.Sprintf("TRAKT_TIMEOUT_SECONDS must be >= 1, got %d", c.Trakt.TimeoutSeconds))
		}
		// 1000 is the largest page trakt serves; asking for more is silently
		// capped there, which would make MaxPages mean less than it says.
		if c.Trakt.PageLimit < 1 || c.Trakt.PageLimit > 1000 {
			problems = append(problems, fmt.Sprintf("TRAKT_PAGE_LIMIT must be 1-1000, got %d", c.Trakt.PageLimit))
		}
		if c.Trakt.MaxPages < 1 {
			problems = append(problems, fmt.Sprintf("TRAKT_MAX_PAGES must be >= 1, got %d", c.Trakt.MaxPages))
		}

		if uuid := strings.TrimSpace(c.Trakt.HealthcheckUUID); uuid != "" {
			// A pasted ping URL instead of the bare id would produce a working
			// but wrong request, so it is worth naming the mistake here.
			if !reUUID.MatchString(uuid) {
				problems = append(problems, fmt.Sprintf(
					"TRAKT_HEALTHCHECK_UUID must be the check UUID alone, got %q", uuid))
			}

			base, err := url.Parse(strings.TrimRight(c.Trakt.HealthcheckBaseURL, "/"))
			switch {
			case strings.TrimSpace(c.Trakt.HealthcheckBaseURL) == "":
				problems = append(problems, "TRAKT_HEALTHCHECK_BASE_URL must not be empty")
			case err != nil:
				problems = append(problems, fmt.Sprintf("TRAKT_HEALTHCHECK_BASE_URL is not a valid URL: %v", err))
			case base.Scheme != "http" && base.Scheme != "https":
				problems = append(problems, "TRAKT_HEALTHCHECK_BASE_URL must use http or https")
			case base.Host == "":
				problems = append(problems, "TRAKT_HEALTHCHECK_BASE_URL must include a host")
			}
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf("invalid configuration:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return nil
}

// MaxTrackerTimeout is the longest per-request timeout of any tracker. One
// attempt searches them in parallel, so the slowest tracker bounds the attempt.
func (c Config) MaxTrackerTimeout() time.Duration {
	longest := time.Duration(0)
	for _, tracker := range c.Trackers {
		if timeout := tracker.Timeout(); timeout > longest {
			longest = timeout
		}
	}
	return longest
}

// TrackerNamesList returns the configured tracker names in order, for the UI
// header and log lines.
func (c Config) TrackerNamesList() []string {
	names := make([]string, 0, len(c.Trackers))
	for _, tracker := range c.Trackers {
		names = append(names, tracker.Name)
	}
	return names
}

// AuthEnabled reports whether the UI and API require HTTP basic auth.
func (c Config) AuthEnabled() bool {
	return c.AuthUser != "" && c.AuthPassword != ""
}

// SlogLevel maps LOG_LEVEL onto slog.
func (c Config) SlogLevel() slog.Level {
	switch strings.ToLower(c.LogLevel) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// LogValue redacts every secret, so logging the whole config is always safe.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Int("http_port", c.HTTPPort),
		slog.String("torrent_files_dir", c.TorrentFilesDir),
		slog.String("db_path", c.DBPath),
		slog.String("tz", c.TZ),
		slog.String("log_level", c.LogLevel),
		slog.Int("batch_max_lines", c.BatchMaxLines),
		slog.Bool("auth_enabled", c.AuthEnabled()),
		slog.Bool("duplicate_check_enabled", c.DuplicateCheckEnabled),
		slog.Int("workers", c.Workers),
		slog.Attr{Key: "trackers", Value: trackerLogValue(c.Trackers)},
		slog.Group("retry",
			slog.Int("max_attempts", c.Retry.MaxAttempts),
			slog.Int("base_seconds", c.Retry.BaseSeconds),
			slog.Int("max_backoff_seconds", c.Retry.MaxBackoffSeconds),
		),
		slog.Group("trakt",
			slog.Bool("enabled", c.Trakt.Enabled),
			slog.String("client_id", redact(c.Trakt.ClientID)),
			slog.String("client_secret", redact(c.Trakt.ClientSecret)),
			slog.String("base_url", c.Trakt.BaseURL),
			slog.Int("interval_minutes", c.Trakt.IntervalMinutes),
			slog.Int("timeout_seconds", c.Trakt.TimeoutSeconds),
			slog.Int("page_limit", c.Trakt.PageLimit),
			slog.Int("max_pages", c.Trakt.MaxPages),
			slog.Bool("query_with_year", c.Trakt.QueryWithYear),
			slog.Bool("healthcheck_enabled", strings.TrimSpace(c.Trakt.HealthcheckUUID) != ""),
			slog.String("healthcheck_uuid", redact(c.Trakt.HealthcheckUUID)),
			slog.String("healthcheck_base_url", c.Trakt.HealthcheckBaseURL),
		),
		slog.Group("jellyfin",
			slog.String("host", c.Jellyfin.Host),
			slog.String("api_key", redact(c.Jellyfin.APIKey)),
			slog.String("user_id", redact(c.Jellyfin.UserID)),
			slog.Int("timeout_seconds", c.Jellyfin.TimeoutSeconds),
		),
	)
}

// trackerLogValue renders the trackers as a group keyed by name, secrets
// redacted. A group rather than a slice: a slice of slog.Value marshals to
// empty objects, because its fields are unexported.
func trackerLogValue(trackers []Tracker) slog.Value {
	groups := make([]slog.Attr, 0, len(trackers))
	for _, tracker := range trackers {
		groups = append(groups, slog.Attr{Key: tracker.Name, Value: slog.GroupValue(
			slog.String("preset", tracker.Preset),
			slog.String("base_url", tracker.BaseURL),
			slog.String("login", redact(tracker.Login)),
			slog.String("password", "[REDACTED]"),
			slog.Int("priority", tracker.Priority),
			slog.Float64("rps", tracker.RPS),
			slog.Int("timeout_seconds", tracker.TimeoutSeconds),
			slog.Int64("max_size_bytes", tracker.MaxSizeBytes),
		)})
	}
	return slog.GroupValue(groups...)
}

// redact keeps just enough of a value to recognise it in logs.
func redact(value string) string {
	if value == "" {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= 2 {
		return "**"
	}
	return string(runes[0]) + strings.Repeat("*", len(runes)-2) + string(runes[len(runes)-1])
}
