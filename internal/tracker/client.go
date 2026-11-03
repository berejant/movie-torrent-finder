// Package tracker talks to a TorrentPier-based tracker: it authenticates,
// searches, ranks the results and saves the winning .torrent file.
//
// The client is safe for concurrent use by the worker pool. It holds no global
// lock: all workers share one cookie session and one rate limiter, and only
// session establishment is serialised (via singleflight), so five workers
// hitting an expired session trigger exactly one re-login.
package tracker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/sync/singleflight"
	"golang.org/x/time/rate"

	"github.com/berejant/movie-torrent-finder/internal/config"
	"github.com/berejant/movie-torrent-finder/internal/media"
)

// maxTorrentFileSize caps what we are willing to read from a download link.
const maxTorrentFileSize = 20 << 20

// Torrent is one ranked tracker result.
type Torrent struct {
	Title       string
	Forum       string
	TopicURL    string
	DownloadURL string
	SizeText    string

	media.Attributes
}

// Client is a tracker session.
type Client struct {
	cfg     config.Tracker
	options config.TrackerOptions
	baseURL *url.URL
	http    *http.Client
	limiter *rate.Limiter
	logins  singleflight.Group
	logger  *slog.Logger
}

// New builds a client. It does not contact the tracker.
func New(cfg config.Tracker, logger *slog.Logger) (*Client, error) {
	baseURL, err := url.Parse(strings.TrimRight(cfg.BaseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("tracker: parse base url: %w", err)
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("tracker: create cookie jar: %w", err)
	}

	return &Client{
		cfg:     cfg,
		options: cfg.Options,
		baseURL: baseURL,
		http: &http.Client{
			Jar:     jar,
			Timeout: cfg.Timeout(),
		},
		// Burst of 1: every request waits its turn, so five workers cannot
		// stampede the tracker.
		limiter: rate.NewLimiter(rate.Limit(cfg.RPS), 1),
		logger:  logger.With("tracker", cfg.Name),
	}, nil
}

// Name is the tracker slug used in saved filenames.
func (c *Client) Name() string { return c.cfg.Name }

// Priority is the tracker rank; lower wins when several trackers are configured.
func (c *Client) Priority() int { return c.cfg.Priority }

// Search authenticates if needed, queries the tracker and returns the results
// ordered best first.
func (c *Client) Search(ctx context.Context, query string) ([]Torrent, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("tracker: search query is empty")
	}

	if err := c.EnsureSession(ctx); err != nil {
		return nil, err
	}

	searchURL, err := c.searchURL(query)
	if err != nil {
		return nil, err
	}

	doc, err := c.fetchDocument(ctx, searchURL, c.resolve(c.options.TrackerPath))
	if err != nil {
		return nil, err
	}

	// The session can expire between the check and the search. Re-login once
	// and repeat before treating it as an error.
	if c.looksLoggedOut(doc) {
		c.logger.Info("session expired mid-search, re-authenticating")
		if err := c.login(ctx); err != nil {
			return nil, err
		}
		if doc, err = c.fetchDocument(ctx, searchURL, c.resolve(c.options.TrackerPath)); err != nil {
			return nil, err
		}
		if c.looksLoggedOut(doc) {
			return nil, ErrLoginFailed
		}
	}

	torrents := c.parseResults(doc, searchURL)
	if len(torrents) == 0 {
		return nil, ErrNoResults
	}

	sort.SliceStable(torrents, func(i, j int) bool {
		return media.Better(c.ranked(torrents[i]), c.ranked(torrents[j]))
	})

	return torrents, nil
}

// SelectBest returns the top-ranked candidate.
func (c *Client) SelectBest(torrents []Torrent) (Torrent, error) {
	if len(torrents) == 0 {
		return Torrent{}, ErrNoResults
	}
	return torrents[0], nil
}

// FetchTopicDetails loads the topic page and parses its description and metadata.
func (c *Client) FetchTopicDetails(ctx context.Context, topicURL string) (TopicDetails, error) {
	if strings.TrimSpace(topicURL) == "" {
		return TopicDetails{}, nil
	}

	if err := c.EnsureSession(ctx); err != nil {
		return TopicDetails{}, err
	}

	searchReferer := c.resolve(c.options.TrackerPath)
	doc, err := c.fetchDocument(ctx, topicURL, searchReferer)
	if err != nil {
		if strings.Contains(err.Error(), "returned 404") {
			return TopicDetails{}, nil
		}
		return TopicDetails{}, err
	}

	if c.looksLoggedOut(doc) {
		c.logger.Info("session expired fetching topic, re-authenticating")
		if err := c.login(ctx); err != nil {
			return TopicDetails{}, err
		}
		if doc, err = c.fetchDocument(ctx, topicURL, searchReferer); err != nil {
			if strings.Contains(err.Error(), "returned 404") {
				return TopicDetails{}, nil
			}
			return TopicDetails{}, err
		}
		if c.looksLoggedOut(doc) {
			return TopicDetails{}, ErrLoginFailed
		}
	}

	return c.ParseTopicDetails(doc), nil
}

func (c *Client) ranked(t Torrent) media.Ranked {
	return media.Ranked{Attributes: t.Attributes, Priority: c.cfg.Priority}
}

// Download saves the selected .torrent into dir and returns its full path.
// The file is written to a temporary name first and renamed into place, so a
// crash never leaves a half-written .torrent for a download client to pick up.
func (c *Client) Download(ctx context.Context, torrent Torrent, dir, requestID string) (string, error) {
	if torrent.DownloadURL == "" {
		return "", fmt.Errorf("tracker: torrent has no download url")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("tracker: create download dir: %w", err)
	}

	resp, err := c.do(ctx, http.MethodGet, torrent.DownloadURL, nil, kindDownload, torrent.TopicURL)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", httpStatusError("download torrent", resp.StatusCode)
	}

	content, err := io.ReadAll(io.LimitReader(resp.Body, maxTorrentFileSize+1))
	if err != nil {
		return "", Transient(fmt.Errorf("tracker: read torrent body: %w", err))
	}
	if len(content) == 0 || len(content) > maxTorrentFileSize {
		return "", ErrInvalidTorrent
	}
	if !looksLikeTorrent(content) {
		return "", ErrInvalidTorrent
	}

	name := media.Filename(torrent.Title, c.cfg.Name, torrent.Quality, requestID)
	finalPath := filepath.Join(dir, name)

	temp, err := os.CreateTemp(dir, ".torrent-*.tmp")
	if err != nil {
		return "", fmt.Errorf("tracker: create temp file: %w", err)
	}
	tempPath := temp.Name()
	defer func() {
		_ = temp.Close()
		_ = os.Remove(tempPath) // no-op once the rename succeeded
	}()

	if _, err := temp.Write(content); err != nil {
		return "", fmt.Errorf("tracker: write temp file: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return "", fmt.Errorf("tracker: sync temp file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return "", fmt.Errorf("tracker: close temp file: %w", err)
	}
	if err := os.Chmod(tempPath, 0o644); err != nil {
		return "", fmt.Errorf("tracker: chmod temp file: %w", err)
	}
	if err := os.Rename(tempPath, finalPath); err != nil {
		return "", fmt.Errorf("tracker: finalize download: %w", err)
	}

	return finalPath, nil
}

// EnsureSession logs in when the current cookie session is not authenticated.
// Concurrent callers collapse into a single login.
func (c *Client) EnsureSession(ctx context.Context) error {
	_, err, _ := c.logins.Do("session", func() (any, error) {
		authenticated, err := c.isLoggedIn(ctx)
		if err != nil {
			return nil, err
		}
		if authenticated {
			return nil, nil
		}
		return nil, c.performLogin(ctx)
	})
	return err
}

// login forces a fresh authentication, collapsing concurrent callers.
func (c *Client) login(ctx context.Context) error {
	_, err, _ := c.logins.Do("login", func() (any, error) {
		return nil, c.performLogin(ctx)
	})
	return err
}

func (c *Client) performLogin(ctx context.Context) error {
	loginURL := c.resolve(c.options.LoginPath)

	form := url.Values{
		c.options.LoginUsernameField: {c.cfg.Login},
		c.options.LoginPasswordField: {c.cfg.Password},
	}
	if c.options.LoginSubmitField != "" {
		form.Set(c.options.LoginSubmitField, c.options.LoginSubmitValue)
	}
	// Checkboxes and hidden inputs the tracker's own form submits, such as
	// toloka's autologin. Set last so a preset cannot silently overwrite the
	// credentials.
	for field, value := range c.options.LoginExtraFields {
		if field == c.options.LoginUsernameField || field == c.options.LoginPasswordField {
			continue
		}
		form.Set(field, value)
	}

	resp, err := c.do(ctx, http.MethodPost, loginURL, strings.NewReader(form.Encode()), kindForm, loginURL)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= http.StatusBadRequest {
		return httpStatusError("submit login", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))

	authenticated, err := c.isLoggedIn(ctx)
	if err != nil {
		return err
	}
	if !authenticated {
		return ErrLoginFailed
	}

	c.logger.Info("tracker session established")
	return nil
}

func (c *Client) isLoggedIn(ctx context.Context) (bool, error) {
	doc, err := c.fetchDocument(ctx, c.resolve(c.options.TrackerPath), c.baseURL.String())
	if err != nil {
		return false, err
	}

	loggedIn := doc.Find(c.options.LoggedInSelector).Length() > 0
	loggedOut := doc.Find(c.options.LoggedOutSelector).Length() > 0
	return loggedIn && !loggedOut, nil
}

func (c *Client) looksLoggedOut(doc *goquery.Document) bool {
	return doc.Find(c.options.LoggedOutSelector).Length() > 0 &&
		doc.Find(c.options.LoggedInSelector).Length() == 0
}

func (c *Client) fetchDocument(ctx context.Context, target, referer string) (*goquery.Document, error) {
	resp, err := c.do(ctx, http.MethodGet, target, nil, kindDocument, referer)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, httpStatusError("fetch "+target, resp.StatusCode)
	}

	doc, err := goquery.NewDocumentFromReader(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, Transient(fmt.Errorf("tracker: parse html: %w", err))
	}
	// html, _ := doc.Html()
	// fmt.Printf("Fetched document from %s:\n%s\n", target, html) // Debugging output

	return doc, nil
}

func (c *Client) searchURL(query string) (string, error) {
	target, err := url.Parse(c.resolve(c.options.TrackerPath))
	if err != nil {
		return "", fmt.Errorf("tracker: build search url: %w", err)
	}

	params := target.Query()
	params.Set(c.options.SearchQueryField, query)
	target.RawQuery = params.Encode()

	return target.String(), nil
}

func (c *Client) resolve(path string) string {
	if parsed, err := url.Parse(path); err == nil && parsed.IsAbs() {
		return parsed.String()
	}

	resolved := *c.baseURL
	resolved.Path = strings.TrimRight(resolved.Path, "/") + "/" + strings.TrimLeft(path, "/")
	return resolved.String()
}

// looksLikeTorrent checks the bencode envelope rather than trusting the
// content type: trackers commonly return an HTML error page with HTTP 200.
func looksLikeTorrent(data []byte) bool {
	trimmed := bytes.TrimSpace(data)
	if !bytes.HasPrefix(trimmed, []byte("d")) {
		return false
	}
	return bytes.Contains(trimmed, []byte("announce")) || bytes.Contains(trimmed, []byte("4:info"))
}
