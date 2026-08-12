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

// pendingTokens is a pair that was refreshed but could not be stored in
// jellyfin. Trakt retired the refresh token jellyfin still holds the moment
// this pair was issued, so this pair — not the document's — is the live one,
// and every later refresh has to start from it.
type pendingTokens struct {
	access  string
	refresh string
	expiry  time.Time
}

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

	// mu serialises refreshes and guards pending. Trakt invalidates a refresh
	// token the moment it is spent, so two refreshes in flight would leave one
	// of them holding a pair that no longer works.
	mu sync.Mutex

	// pending holds a pair that was refreshed but not yet stored, so a Jellyfin
	// outage does not cost the grant: the next Refresh writes it into the
	// document it re-reads instead of asking trakt for a new one.
	pending *pendingTokens
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

	return s.Refresh(ctx, user.AccessToken())
}

// Refresh exchanges the plugin's refresh token for a new access token and
// stores the result back. Call it when trakt rejects a token Token believed was
// still good; rejected is that token, so a re-read still holding it is not
// mistaken for a renewal.
func (s *TokenSource) Refresh(ctx context.Context, rejected string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Re-read rather than trust what the caller saw: the plugin refreshes the
	// same grant, and if it got there first its token is the live one while the
	// refresh token we were about to spend is already dead.
	cfg, user, err := s.load(ctx)
	if err != nil {
		return "", err
	}

	if s.pending != nil {
		// The document's pair is known-dead: trakt retired it the moment the
		// pending pair was issued, so ours wins outright over whatever the
		// plugin wrote in the meantime. RefreshToken below now reads pending's
		// refresh token too, in case the pair needs renewing again.
		user.SetTokens(s.pending.access, s.pending.refresh, s.pending.expiry)
	}

	// The same token coming back means nothing has changed, so the shortcut
	// below would hand the caller the very token that was just refused.
	if expiry := user.Expiration(); user.AccessToken() != "" && user.AccessToken() != rejected &&
		!expiry.IsZero() && time.Now().Add(refreshBuffer).Before(expiry) {
		if s.pending == nil {
			s.logger.Info("the trakt access token was already refreshed by the plugin; using it",
				"expires_at", expiry)
			return user.AccessToken(), nil
		}

		// Trakt already issued this pair; it only ever needed storing.
		access := user.AccessToken()
		if err := s.store(ctx, cfg, access, user.RefreshToken(), expiry); err != nil {
			return access, nil
		}
		s.logger.Info("stored a previously-held trakt access token in jellyfin", "expires_at", expiry)
		return access, nil
	}

	refreshToken := user.RefreshToken()
	if refreshToken == "" {
		return "", errors.New("trakt: the jellyfin trakt plugin holds no refresh token; re-authorize the plugin")
	}

	issued, err := s.oauth.Refresh(ctx, refreshToken)
	if err != nil {
		return "", err
	}

	now := time.Now()
	expiry := issued.Expiry(now)
	// user points into cfg, so this edits the document that is about to be sent.
	user.SetTokens(issued.Token, issued.RefreshToken, expiry)

	if err := s.store(ctx, cfg, issued.Token, issued.RefreshToken, expiry); err != nil {
		return issued.Token, nil
	}
	s.logger.Info("refreshed the trakt access token", "expires_at", expiry)
	return issued.Token, nil
}

// store saves cfg, which already carries access/refresh/expiry via user. On
// failure the pair is held in s.pending rather than discarded: trakt has
// already retired whatever refresh token jellyfin held before this pair was
// written, so this pair, not the document's, is the one every later Refresh
// must start from.
func (s *TokenSource) store(ctx context.Context, cfg *jellyfin.TraktConfig, access, refresh string, expiry time.Time) error {
	if err := s.jellyfin.SaveTraktConfig(ctx, cfg); err != nil {
		s.pending = &pendingTokens{access: access, refresh: refresh, expiry: expiry}
		s.logger.Error("refreshed the trakt access token but could not store it in jellyfin; "+
			"holding it in memory and retrying on the next sync",
			"err", err)
		return err
	}

	s.pending = nil
	return nil
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
