package jellyfin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/berejant/movie-torrent-finder/internal/config"
)

// pluginID is the trakt plugin's GUID. A plugin's id is generated once, for the
// plugin and not for the install, so hard-coding it is correct.
const pluginID = "4fe3201ed6ae4f2e8917e12bda571281"

// configPath serves the plugin's configuration: GET reads it, POST replaces it.
const configPath = "/Plugins/" + pluginID + "/Configuration"

// maxResponseSize caps the configuration document, which is a few kilobytes at
// most. It bounds the damage from pointing JELLYFIN_HOST at the wrong service.
const maxResponseSize = 4 << 20

// ErrUnauthorized means Jellyfin rejected the API key. It is worth its own
// error: the fix is a Jellyfin setting, and nothing about the trakt token.
var ErrUnauthorized = errors.New("jellyfin: api key rejected")

// Client talks to Jellyfin's plugin-configuration API.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// NewClient builds a client. It does not contact Jellyfin. logger is accepted
// but currently unused; the parameter stays so a client that later needs to
// log does not force every caller to change its call site.
func NewClient(cfg config.Jellyfin, logger *slog.Logger) (*Client, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.Host), "/")
	if base == "" {
		return nil, errors.New("jellyfin: JELLYFIN_HOST is empty")
	}
	if _, err := url.Parse(base); err != nil {
		return nil, fmt.Errorf("jellyfin: parse host: %w", err)
	}

	return &Client{
		baseURL: base,
		apiKey:  strings.TrimSpace(cfg.APIKey),
		http:    &http.Client{Timeout: cfg.Timeout()},
	}, nil
}

// TraktConfig reads the trakt plugin's configuration.
func (c *Client) TraktConfig(ctx context.Context) (*TraktConfig, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+configPath, nil)
	if err != nil {
		return nil, fmt.Errorf("jellyfin: build request: %w", err)
	}
	req.Header.Set("Authorization", c.authorization())
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jellyfin: GET %s: %w", configPath, err)
	}
	defer drain(resp)

	if err := checkStatus(resp, http.MethodGet); err != nil {
		return nil, err
	}

	var cfg TraktConfig
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseSize)).Decode(&cfg); err != nil {
		return nil, fmt.Errorf("jellyfin: decode plugin configuration: %w", err)
	}
	return &cfg, nil
}

// SaveTraktConfig writes the configuration back. The whole document is sent:
// the endpoint replaces it rather than patching it.
func (c *Client) SaveTraktConfig(ctx context.Context, cfg *TraktConfig) error {
	body, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("jellyfin: encode plugin configuration: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+configPath, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("jellyfin: build request: %w", err)
	}
	req.Header.Set("Authorization", c.authorization())
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("jellyfin: POST %s: %w", configPath, err)
	}
	defer drain(resp)

	return checkStatus(resp, http.MethodPost)
}

// authorization is Jellyfin's API-key scheme. The key is quoted.
func (c *Client) authorization() string {
	return fmt.Sprintf("MediaBrowser Token=%q", c.apiKey)
}

// checkStatus maps a response to an error. Any 2xx is a success: the read
// answers 200 and the save 204, and a later version is free to pick another.
func checkStatus(resp *http.Response, method string) error {
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w (%s %s, status %d)", ErrUnauthorized, method, configPath, resp.StatusCode)
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return fmt.Errorf("jellyfin: %s %s: unexpected status %d", method, configPath, resp.StatusCode)
	}
	return nil
}

// drain empties and closes the body so the connection can be reused.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseSize))
	_ = resp.Body.Close()
}
