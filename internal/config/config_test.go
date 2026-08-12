package config

import (
	"strings"
	"testing"
	"time"
)

// baseEnv sets the variables every configuration needs, so each test only has
// to declare the tracker layout it is actually about.
func baseEnv(t *testing.T) {
	t.Helper()
	t.Setenv("TORRENT_FILES_DIR", t.TempDir())
	t.Setenv("DB_PATH", t.TempDir()+"/app.db")
}

func TestLoadMultipleTrackers(t *testing.T) {
	baseEnv(t)
	t.Setenv("TRACKERS", "toloka, mazepa")
	t.Setenv("TRACKER_TOLOKA_LOGIN", "tester")
	t.Setenv("TRACKER_TOLOKA_PASSWORD", "secret")
	t.Setenv("TRACKER_TOLOKA_PRIORITY", "1")
	t.Setenv("TRACKER_MAZEPA_LOGIN", "tester")
	t.Setenv("TRACKER_MAZEPA_PASSWORD", "secret")
	t.Setenv("TRACKER_MAZEPA_PRIORITY", "2")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if len(cfg.Trackers) != 2 {
		t.Fatalf("loaded %d trackers, want 2", len(cfg.Trackers))
	}

	// Configuration order is preserved: it is the order the UI lists and the
	// tie-break order of equally ranked candidates.
	toloka, mazepa := cfg.Trackers[0], cfg.Trackers[1]
	if toloka.Name != "toloka" || mazepa.Name != "mazepa" {
		t.Fatalf("names = %q, %q; want toloka, mazepa", toloka.Name, mazepa.Name)
	}

	// The slug names the preset, so neither tracker needs a PRESET variable.
	if toloka.Preset != "toloka" || mazepa.Preset != "mazepa" {
		t.Errorf("presets = %q, %q; want toloka, mazepa", toloka.Preset, mazepa.Preset)
	}
	if toloka.BaseURL != "https://toloka.to" {
		t.Errorf("toloka base url = %q, want the preset default", toloka.BaseURL)
	}
	if mazepa.BaseURL != "https://mazepa.to" {
		t.Errorf("mazepa base url = %q, want the preset default", mazepa.BaseURL)
	}

	// The engines differ where it matters: the size column and the row selector.
	if toloka.Options.SizeCellIndex != 6 {
		t.Errorf("toloka size cell = %d, want 6", toloka.Options.SizeCellIndex)
	}
	if mazepa.Options.SizeCellIndex != 5 {
		t.Errorf("mazepa size cell = %d, want 5", mazepa.Options.SizeCellIndex)
	}
	if toloka.Options.LoginUsernameField != "username" || mazepa.Options.LoginUsernameField != "login_username" {
		t.Errorf("login fields = %q, %q", toloka.Options.LoginUsernameField, mazepa.Options.LoginUsernameField)
	}
	if toloka.Priority != 1 || mazepa.Priority != 2 {
		t.Errorf("priorities = %d, %d; want 1, 2", toloka.Priority, mazepa.Priority)
	}
}

// The legacy layout — one tracker on unprefixed TRACKER_* variables — has to
// keep working, because it is what every existing .env file looks like.
func TestLoadLegacySingleTracker(t *testing.T) {
	baseEnv(t)
	t.Setenv("TRACKER_NAME", "mazepa")
	t.Setenv("TRACKER_BASE_URL", "https://mazepa.to")
	t.Setenv("TRACKER_LOGIN", "tester")
	t.Setenv("TRACKER_PASSWORD", "secret")
	t.Setenv("TRACKER_WORKERS", "7")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if len(cfg.Trackers) != 1 {
		t.Fatalf("loaded %d trackers, want 1", len(cfg.Trackers))
	}
	if cfg.Trackers[0].Name != "mazepa" || cfg.Trackers[0].Preset != "mazepa" {
		t.Errorf("tracker = %q/%q, want mazepa/mazepa", cfg.Trackers[0].Name, cfg.Trackers[0].Preset)
	}
	// TRACKER_WORKERS used to size the pool; it still does while WORKERS is
	// unset, so an existing deployment keeps its concurrency.
	if cfg.Workers != 7 {
		t.Errorf("workers = %d, want the legacy TRACKER_WORKERS value 7", cfg.Workers)
	}
}

func TestLoadExtraOptionsOverridePresetPerTracker(t *testing.T) {
	baseEnv(t)
	t.Setenv("TRACKERS", "one,two")
	t.Setenv("TRACKER_ONE_PRESET", "torrentpier")
	t.Setenv("TRACKER_ONE_BASE_URL", "https://one.example")
	t.Setenv("TRACKER_ONE_LOGIN", "tester")
	t.Setenv("TRACKER_ONE_PASSWORD", "secret")
	t.Setenv("TRACKER_ONE_EXTRA_OPTIONS", `{"size_cell_index":9,"login_extra_fields":{"autologin":"1"}}`)
	t.Setenv("TRACKER_TWO_PRESET", "torrentpier")
	t.Setenv("TRACKER_TWO_BASE_URL", "https://two.example")
	t.Setenv("TRACKER_TWO_LOGIN", "tester")
	t.Setenv("TRACKER_TWO_PASSWORD", "secret")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	one, two := cfg.Trackers[0], cfg.Trackers[1]
	if one.Options.SizeCellIndex != 9 {
		t.Errorf("overridden size cell = %d, want 9", one.Options.SizeCellIndex)
	}
	// Keys the operator did not mention keep the preset's value.
	if one.Options.ResultRowSelector != DefaultTrackerOptions().ResultRowSelector {
		t.Errorf("row selector = %q, want the preset value", one.Options.ResultRowSelector)
	}
	// Both trackers share a preset, so an override that wrote through the
	// shared profile would show up on the other one.
	if two.Options.SizeCellIndex != 5 {
		t.Errorf("second tracker size cell = %d, want the untouched preset value 5", two.Options.SizeCellIndex)
	}
	if len(two.Options.LoginExtraFields) != 0 {
		t.Errorf("second tracker inherited login fields %v from the first", two.Options.LoginExtraFields)
	}
}

func TestLoadRejectsBadTrackerLists(t *testing.T) {
	t.Run("unknown preset", func(t *testing.T) {
		baseEnv(t)
		t.Setenv("TRACKERS", "custom")
		t.Setenv("TRACKER_CUSTOM_PRESET", "nosuchengine")
		t.Setenv("TRACKER_CUSTOM_BASE_URL", "https://custom.example")
		t.Setenv("TRACKER_CUSTOM_LOGIN", "tester")
		t.Setenv("TRACKER_CUSTOM_PASSWORD", "secret")

		_, err := Load()
		if err == nil || !strings.Contains(err.Error(), "TRACKER_CUSTOM_PRESET") {
			t.Fatalf("error = %v, want it to name TRACKER_CUSTOM_PRESET", err)
		}
	})

	t.Run("duplicate slug", func(t *testing.T) {
		baseEnv(t)
		t.Setenv("TRACKERS", "toloka,toloka")
		t.Setenv("TRACKER_TOLOKA_LOGIN", "tester")
		t.Setenv("TRACKER_TOLOKA_PASSWORD", "secret")

		_, err := Load()
		if err == nil || !strings.Contains(err.Error(), "more than once") {
			t.Fatalf("error = %v, want a duplicate-slug rejection", err)
		}
	})

	t.Run("missing credentials name the prefixed variable", func(t *testing.T) {
		baseEnv(t)
		t.Setenv("TRACKERS", "toloka")
		t.Setenv("TRACKER_TOLOKA_LOGIN", "")
		t.Setenv("TRACKER_TOLOKA_PASSWORD", "")

		_, err := Load()
		if err == nil || !strings.Contains(err.Error(), "TRACKER_TOLOKA_LOGIN") {
			t.Fatalf("error = %v, want it to name TRACKER_TOLOKA_LOGIN", err)
		}
	})

	// A preset that describes an engine rather than a site supplies no base
	// URL, so the operator has to.
	t.Run("engine preset without base url", func(t *testing.T) {
		baseEnv(t)
		t.Setenv("TRACKERS", "custom")
		t.Setenv("TRACKER_CUSTOM_LOGIN", "tester")
		t.Setenv("TRACKER_CUSTOM_PASSWORD", "secret")

		_, err := Load()
		if err == nil || !strings.Contains(err.Error(), "TRACKER_CUSTOM_BASE_URL") {
			t.Fatalf("error = %v, want it to name TRACKER_CUSTOM_BASE_URL", err)
		}
	})
}

func TestLoadTraktDefaults(t *testing.T) {
	baseEnv(t)
	t.Setenv("TRACKERS", "toloka")
	t.Setenv("TRACKER_TOLOKA_LOGIN", "tester")
	t.Setenv("TRACKER_TOLOKA_PASSWORD", "secret")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	// Off unless asked for: an existing deployment gains nothing to configure.
	if cfg.Trakt.Enabled {
		t.Error("Trakt.Enabled = true, want false by default")
	}
	if got := cfg.Trakt.Interval(); got != 15*time.Minute {
		t.Errorf("Trakt.Interval() = %v, want 15m", got)
	}
	if cfg.Trakt.BaseURL != "https://api.trakt.tv" {
		t.Errorf("Trakt.BaseURL = %q, want https://api.trakt.tv", cfg.Trakt.BaseURL)
	}
	if !cfg.Trakt.QueryWithYear {
		t.Error("Trakt.QueryWithYear = false, want true by default")
	}
}

func TestLoadTraktEnabled(t *testing.T) {
	baseEnv(t)
	t.Setenv("TRACKERS", "toloka")
	t.Setenv("TRACKER_TOLOKA_LOGIN", "tester")
	t.Setenv("TRACKER_TOLOKA_PASSWORD", "secret")
	t.Setenv("TRAKT_ENABLED", "true")
	t.Setenv("TRAKT_CLIENT_ID", "client-id")
	t.Setenv("TRAKT_CLIENT_SECRET", "client-secret")
	t.Setenv("TRAKT_INTERVAL_MINUTES", "5")
	t.Setenv("JELLYFIN_HOST", "http://jellyfin:8096")
	t.Setenv("JELLYFIN_API_KEY", "api-key")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.Trakt.ClientID != "client-id" {
		t.Errorf("Trakt.ClientID = %q, want client-id", cfg.Trakt.ClientID)
	}
	if got := cfg.Trakt.Interval(); got != 5*time.Minute {
		t.Errorf("Trakt.Interval() = %v, want 5m", got)
	}
}

// The trakt variables are only checked once the sync is switched on.
func TestLoadRejectsIncompleteTrakt(t *testing.T) {
	tests := map[string]string{
		"TRAKT_CLIENT_ID":     "TRAKT_CLIENT_SECRET",
		"TRAKT_CLIENT_SECRET": "TRAKT_CLIENT_ID",
	}

	for set, want := range tests {
		t.Run("only "+set, func(t *testing.T) {
			baseEnv(t)
			t.Setenv("TRACKERS", "toloka")
			t.Setenv("TRACKER_TOLOKA_LOGIN", "tester")
			t.Setenv("TRACKER_TOLOKA_PASSWORD", "secret")
			t.Setenv("TRAKT_ENABLED", "true")
			t.Setenv(set, "value")

			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("error = %v, want it to name %s", err, want)
			}
		})
	}
}

// The healthcheck id is optional; a pasted ping URL instead of the bare UUID
// would otherwise fail silently against the wrong endpoint.
func TestLoadRejectsABadHealthcheckUUID(t *testing.T) {
	baseEnv(t)
	t.Setenv("TRACKERS", "toloka")
	t.Setenv("TRACKER_TOLOKA_LOGIN", "tester")
	t.Setenv("TRACKER_TOLOKA_PASSWORD", "secret")
	t.Setenv("TRAKT_ENABLED", "true")
	t.Setenv("TRAKT_CLIENT_ID", "client-id")
	t.Setenv("TRAKT_CLIENT_SECRET", "client-secret")
	t.Setenv("JELLYFIN_HOST", "http://jellyfin:8096")
	t.Setenv("JELLYFIN_API_KEY", "api-key")

	t.Run("full ping url", func(t *testing.T) {
		t.Setenv("TRAKT_HEALTHCHECK_UUID", "https://hc-ping.com/c38a1b6c-0607-4e4c-8bbf-fc2d50e1f0e1")

		_, err := Load()
		if err == nil || !strings.Contains(err.Error(), "TRAKT_HEALTHCHECK_UUID") {
			t.Fatalf("error = %v, want it to name TRAKT_HEALTHCHECK_UUID", err)
		}
	})

	t.Run("valid uuid", func(t *testing.T) {
		t.Setenv("TRAKT_HEALTHCHECK_UUID", "c38a1b6c-0607-4e4c-8bbf-fc2d50e1f0e1")

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() error: %v", err)
		}
		if cfg.Trakt.HealthcheckBaseURL != "https://hc-ping.com" {
			t.Errorf("HealthcheckBaseURL = %q, want the hosted endpoint", cfg.Trakt.HealthcheckBaseURL)
		}
	})

	// Unset is the default and must stay valid: signalling is opt-in.
	t.Run("unset", func(t *testing.T) {
		t.Setenv("TRAKT_HEALTHCHECK_UUID", "")

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() error: %v", err)
		}
		if cfg.Trakt.HealthcheckUUID != "" {
			t.Errorf("HealthcheckUUID = %q, want empty", cfg.Trakt.HealthcheckUUID)
		}
	})
}

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
			t.Setenv("JELLYFIN_API_KEY", "api-key")
			t.Setenv("JELLYFIN_HOST", host)

			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), "JELLYFIN_HOST") {
				t.Fatalf("error = %v, want it to name JELLYFIN_HOST", err)
			}
		})
	}
}

func TestLoadRejectsABadJellyfinTimeout(t *testing.T) {
	for name, seconds := range map[string]string{"zero": "0", "negative": "-1"} {
		t.Run(name, func(t *testing.T) {
			baseEnv(t)
			t.Setenv("TRACKERS", "toloka")
			t.Setenv("TRACKER_TOLOKA_LOGIN", "tester")
			t.Setenv("TRACKER_TOLOKA_PASSWORD", "secret")
			t.Setenv("TRAKT_ENABLED", "true")
			t.Setenv("TRAKT_CLIENT_ID", "client-id")
			t.Setenv("TRAKT_CLIENT_SECRET", "client-secret")
			t.Setenv("JELLYFIN_HOST", "http://jellyfin:8096")
			t.Setenv("JELLYFIN_API_KEY", "api-key")
			t.Setenv("JELLYFIN_TIMEOUT_SECONDS", seconds)

			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), "JELLYFIN_TIMEOUT_SECONDS") {
				t.Fatalf("error = %v, want it to name JELLYFIN_TIMEOUT_SECONDS", err)
			}
		})
	}
}
