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

// usersKey is the one top-level field this service reads and writes.
// Everything else in the document is held in TraktConfig.extra untouched.
const usersKey = "TraktUsers"

// TraktConfig is the trakt plugin's configuration document.
//
// The users are held as raw JSON rather than as a struct because a save is a
// read-modify-write of a document another application owns: decoding into a
// struct would drop every key this service does not know — Scrobble,
// LocationsExcluded, whatever a later plugin version adds — and write them back
// as absent, resetting the operator's settings. The same guarantee applies one
// level up: extra holds every top-level key besides TraktUsers, so a document
// setting this service has never heard of survives a save too.
type TraktConfig struct {
	Users []map[string]json.RawMessage

	extra map[string]json.RawMessage
}

// UnmarshalJSON splits the document into the users this service acts on and
// everything else, held untouched in extra.
func (c *TraktConfig) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	if usersRaw, ok := raw[usersKey]; ok {
		if err := json.Unmarshal(usersRaw, &c.Users); err != nil {
			return fmt.Errorf("jellyfin: decode %s: %w", usersKey, err)
		}
		delete(raw, usersKey)
	}
	c.extra = raw
	return nil
}

// MarshalJSON reassembles the document: extra byte-identical, TraktUsers
// re-encoded from Users, which SetTokens may have edited in place.
func (c *TraktConfig) MarshalJSON() ([]byte, error) {
	out := make(map[string]json.RawMessage, len(c.extra)+1)
	for key, value := range c.extra {
		out[key] = value
	}

	users, err := json.Marshal(c.Users)
	if err != nil {
		return nil, fmt.Errorf("jellyfin: encode %s: %w", usersKey, err)
	}
	out[usersKey] = users

	return json.Marshal(out)
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
