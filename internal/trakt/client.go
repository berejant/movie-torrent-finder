// Package trakt reads a trakt.tv watchlist and schedules its movies for
// download.
//
// The API side is deliberately small: one authenticated GET against
// /sync/watchlist/movies/listed_at/desc, paged. Everything that decides what to
// do with the result lives in the syncer.
package trakt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/berejant/movie-torrent-finder/internal/config"
)

// ErrUnauthorized means trakt rejected the access token or the client id. The
// syncer answers it by refreshing the token once; a second one means the grant
// is gone and the trakt plugin has to be re-authorized in jellyfin.
var ErrUnauthorized = errors.New("trakt: credentials rejected")

// apiVersion is the value of the required trakt-api-version header.
const apiVersion = "2"

// watchlistPath is the movies watchlist, newest addition first. Sorting in the
// path is what bounds the rescan: the sync stops at the first entry it has
// already seen instead of walking the whole list.
const watchlistPath = "/sync/watchlist/movies/listed_at/desc"

// maxResponseSize caps a watchlist page. A 100-item page is well under 100 KB.
const maxResponseSize = 32 << 20

// MovieIDs are the external identifiers trakt carries for a movie.
type MovieIDs struct {
	Trakt int64  `json:"trakt"`
	Slug  string `json:"slug"`
	IMDB  string `json:"imdb"`
	TMDB  int64  `json:"tmdb"`
}

// Movie is the movie an entry points at.
type Movie struct {
	Title string   `json:"title"`
	Year  int      `json:"year"`
	IDs   MovieIDs `json:"ids"`
}

// WatchlistItem is one entry of the movies watchlist.
type WatchlistItem struct {
	// ID is the watchlist entry id. It changes when the movie is removed and
	// added again; Movie.IDs.Trakt does not.
	ID       int64     `json:"id"`
	Rank     int       `json:"rank"`
	ListedAt time.Time `json:"listed_at"`
	Type     string    `json:"type"`
	Movie    Movie     `json:"movie"`
}

// Page is one page of the watchlist plus what the pagination headers said.
type Page struct {
	Items []WatchlistItem
	// PageCount is X-Pagination-Page-Count, or 0 when trakt did not send it.
	PageCount int
}

// Client talks to the trakt API.
type Client struct {
	baseURL  string
	clientID string
	http     *http.Client
	logger   *slog.Logger
}

// NewClient builds a client. It does not contact trakt.
func NewClient(cfg config.Trakt, logger *slog.Logger) (*Client, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if _, err := url.Parse(base); err != nil {
		return nil, fmt.Errorf("trakt: parse base url: %w", err)
	}

	return &Client{
		baseURL:  base,
		clientID: strings.TrimSpace(cfg.ClientID),
		http:     &http.Client{Timeout: cfg.Timeout()},
		logger:   logger.With("component", "trakt"),
	}, nil
}

// WatchlistMovies fetches one page of the movies watchlist, newest first.
// Pages are 1-based; limit is the page size.
func (c *Client) WatchlistMovies(ctx context.Context, accessToken string, page, limit int) (Page, error) {
	if page < 1 {
		page = 1
	}

	target := fmt.Sprintf("%s%s?page=%d&limit=%d", c.baseURL, watchlistPath, page, limit)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return Page{}, fmt.Errorf("trakt: build request: %w", err)
	}

	// The four headers trakt requires; without any of them the API answers 4xx
	// regardless of the token.
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("trakt-api-version", apiVersion)
	req.Header.Set("trakt-api-key", c.clientID)
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := c.http.Do(req)
	if err != nil {
		return Page{}, fmt.Errorf("trakt: GET %s: %w", watchlistPath, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseSize))
		_ = resp.Body.Close()
	}()

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return Page{}, fmt.Errorf("%w (status %d)", ErrUnauthorized, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return Page{}, fmt.Errorf("trakt: GET %s: unexpected status %d", watchlistPath, resp.StatusCode)
	}

	var items []WatchlistItem
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseSize)).Decode(&items); err != nil {
		return Page{}, fmt.Errorf("trakt: decode watchlist: %w", err)
	}

	pageCount := 0
	if raw := resp.Header.Get("X-Pagination-Page-Count"); raw != "" {
		if parsed, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil {
			pageCount = parsed
		}
	}

	return Page{Items: items, PageCount: pageCount}, nil
}
