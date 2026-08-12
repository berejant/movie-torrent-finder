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
