package config

import (
	"maps"
	"slices"
)

// Preset is a ready-made tracker profile: the selectors, form fields and column
// layout of one tracker engine, plus the canonical base URL when the preset
// names a concrete site rather than a generic engine.
//
// Adding a tracker should not require touching the parser. Write a preset here,
// list the slug in TRACKERS, and supply credentials.
type Preset struct {
	// BaseURL is used when TRACKER_<SLUG>_BASE_URL is not set. Empty for a
	// preset that describes an engine rather than one site.
	BaseURL string
	Options TrackerOptions
}

// DefaultPresetName is the fallback when neither TRACKER_<SLUG>_PRESET nor the
// tracker slug itself names a known preset.
const DefaultPresetName = "torrentpier"

// presets is keyed by the name TRACKER_<SLUG>_PRESET takes, which defaults to
// the tracker slug — so TRACKERS=toloka,mazepa needs no PRESET variable at all.
var presets = map[string]Preset{
	"torrentpier": {Options: torrentPierOptions()},
	"mazepa":      {BaseURL: "https://mazepa.to", Options: torrentPierOptions()},
	"toloka":      {BaseURL: "https://toloka.to", Options: tolokaOptions()},
}

// LookupPreset returns a copy of the named preset. The copy matters: the
// returned options are then merged with EXTRA_OPTIONS, which must not write
// through into the shared preset that other trackers also use.
func LookupPreset(name string) (Preset, bool) {
	preset, ok := presets[name]
	if !ok {
		return Preset{}, false
	}
	preset.Options = preset.Options.clone()
	return preset, true
}

// PresetNames lists the known presets for error messages, in a stable order.
func PresetNames() []string {
	names := make([]string, 0, len(presets))
	for name := range presets {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// torrentPierOptions targets a stock TorrentPier install (mazepa.to).
//
// Result columns: 0 publish, 1 status, 2 forum, 3 topic, 4 author,
// 5 size/download, 6 seeders, 7 leechers, 8 replies, 9 added.
func torrentPierOptions() TrackerOptions {
	return TrackerOptions{
		TrackerPath:          "/tracker.php",
		LoginPath:            "/login.php",
		LoginUsernameField:   "login_username",
		LoginPasswordField:   "login_password",
		LoginSubmitField:     "login",
		LoginSubmitValue:     "Увійти",
		SearchQueryField:     "nm",
		LoggedInSelector:     "a[href*='logout']",
		LoggedOutSelector:    "#register_link",
		ResultRowSelector:    "#forum_table tbody tr",
		TopicLinkSelector:    "a[href*='topic-']",
		DownloadLinkSelector: "a[href*='dl.php?id=']",
		ForumLinkSelector:    "a[href*='forum-']",
		SizeCellIndex:        5,
		TopicBodySelector:    ".post_body, .postbody, td.message",
	}
}

// tolokaOptions targets toloka.to, which runs a phpBB2-derived engine with a
// different table than TorrentPier.
//
// Result columns: 0 icon, 1 forum, 2 topic, 3 author, 4 checked status,
// 5 download, 6 size, 7 release status, 8 completed, 9 seeders, 10 leechers,
// 11 replies, 12 added.
func tolokaOptions() TrackerOptions {
	return TrackerOptions{
		TrackerPath:        "/tracker.php",
		LoginPath:          "/login.php",
		LoginUsernameField: "username",
		LoginPasswordField: "password",
		LoginSubmitField:   "login",
		LoginSubmitValue:   "Вхід",
		// The login form ticks autologin by default; without it the tracker
		// hands out a session cookie that dies with the browser session.
		LoginExtraFields: map[string]string{"autologin": "1"},

		SearchQueryField: "nm",

		LoggedInSelector: "a[href*='logout']",
		// Deliberately not a login.php link: the logged-in header links
		// /login.php?logout=true, so only the register link separates the two
		// states.
		LoggedOutSelector: "a[href*='mode=register']",

		ResultRowSelector:    "table.forumline tr.prow1, table.forumline tr.prow2",
		TopicLinkSelector:    "td.topictitle a",
		DownloadLinkSelector: "a[href*='download.php?id=']",
		// The author cell links tracker.php?pid=, so the forum link has to be
		// matched on f= rather than on tracker.php alone.
		ForumLinkSelector: "a[href*='tracker.php?f=']",
		SizeCellIndex:     6,
		TopicBodySelector: ".postbody",
	}
}

// clone deep-copies the parts of the options that are reference types.
func (o TrackerOptions) clone() TrackerOptions {
	if o.LoginExtraFields != nil {
		fields := make(map[string]string, len(o.LoginExtraFields))
		maps.Copy(fields, o.LoginExtraFields)
		o.LoginExtraFields = fields
	}
	return o
}
