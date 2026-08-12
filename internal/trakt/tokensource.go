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

	// mu serialises refreshes. Trakt invalidates a refresh token the moment it
	// is spent, so two refreshes in flight would leave one of them holding a
	// pair that no longer works.
	mu sync.Mutex
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

	return s.Refresh(ctx)
}

// Refresh exchanges the plugin's refresh token for a new access token and
// stores the result back. Call it when trakt rejects a token Token believed was
// still good.
func (s *TokenSource) Refresh(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Re-read rather than trust what the caller saw: the plugin refreshes the
	// same grant, and if it got there first its token is the live one while the
	// refresh token we were about to spend is already dead.
	cfg, user, err := s.load(ctx)
	if err != nil {
		return "", err
	}
	if expiry := user.Expiration(); user.AccessToken() != "" && !expiry.IsZero() &&
		time.Now().Add(refreshBuffer).Before(expiry) {
		s.logger.Info("the trakt access token was already refreshed by the plugin; using it",
			"expires_at", expiry)
		return user.AccessToken(), nil
	}

	refreshToken := user.RefreshToken()
	if refreshToken == "" {
		return "", errors.New("trakt: the jellyfin trakt plugin holds no refresh token; re-authorize the plugin")
	}

	issued, err := s.oauth.Refresh(ctx, refreshToken)
	if err != nil {
		return "", err
	}

	// user points into cfg, so this edits the document that is about to be sent.
	user.SetTokens(issued.Token, issued.RefreshToken, issued.Expiry(time.Now()))

	if err := s.jellyfin.SaveTraktConfig(ctx, cfg); err != nil {
		// The token works; storing it did not. Using it for this sync beats
		// failing — but trakt has already retired the refresh token that
		// jellyfin still holds, so the next refresh will fail until the plugin
		// is re-authorized. That is worth saying out loud.
		s.logger.Error("refreshed the trakt access token but could not store it in jellyfin; "+
			"the refresh token jellyfin holds is now spent and the plugin will need re-authorizing",
			"err", err)
		return issued.Token, nil
	}

	s.logger.Info("refreshed the trakt access token", "expires_at", issued.Expiry(time.Now()))
	return issued.Token, nil
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
