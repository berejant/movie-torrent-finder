package trakt

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// fakeOAuth serves trakt's token endpoint and keeps the request it was sent.
type fakeOAuth struct {
	*httptest.Server

	request map[string]string
	calls   int
	status  int // 0 means 200 with a fresh token
	body    string
}

func newFakeOAuth(t *testing.T) *fakeOAuth {
	t.Helper()

	fake := &fakeOAuth{
		body: `{"access_token":"fresh-access","refresh_token":"fresh-refresh",` +
			`"expires_in":7776000,"token_type":"bearer","scope":"public"}`,
	}
	fake.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != oauthTokenPath {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}

		fake.calls++
		if err := json.NewDecoder(r.Body).Decode(&fake.request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if fake.status != 0 {
			w.WriteHeader(fake.status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, fake.body)
	}))
	t.Cleanup(fake.Close)

	return fake
}

func TestOAuthRefreshSendsThePluginsRequest(t *testing.T) {
	fake := newFakeOAuth(t)

	cfg := testConfig(fake.URL)
	cfg.ClientSecret = "client-secret"

	issued, err := NewOAuth(cfg).Refresh(context.Background(), "old-refresh")
	if err != nil {
		t.Fatalf("Refresh() error: %v", err)
	}

	// Field for field what the Emby/Jellyfin plugin sends: the grant was issued
	// against these values, so a refresh has to name them too.
	for field, want := range map[string]string{
		"client_id":     testClientID,
		"client_secret": "client-secret",
		"redirect_uri":  "urn:ietf:wg:oauth:2.0:oob",
		"refresh_token": "old-refresh",
		"grant_type":    "refresh_token",
	} {
		if got := fake.request[field]; got != want {
			t.Errorf("request %s = %q, want %q", field, got, want)
		}
	}

	if issued.Token != "fresh-access" || issued.RefreshToken != "fresh-refresh" {
		t.Errorf("issued = %+v, want the fresh pair", issued)
	}
	if issued.ExpiresIn != 7776000 {
		t.Errorf("ExpiresIn = %d, want 7776000", issued.ExpiresIn)
	}
}

// Three quarters of the token's life, the same margin the plugin leaves: trakt
// does not say when a refresh token dies, so both sides renew early.
func TestAccessTokenExpiry(t *testing.T) {
	now := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	issued := AccessToken{ExpiresIn: 7776000} // 90 days

	want := now.Add(5832000 * time.Second) // 67.5 days
	if got := issued.Expiry(now); !got.Equal(want) {
		t.Errorf("Expiry() = %v, want %v", got, want)
	}
}

func TestOAuthRefreshRejection(t *testing.T) {
	tests := map[string]struct {
		status int
		body   string
		wantIs error
	}{
		"spent refresh token": {status: http.StatusUnauthorized, wantIs: ErrUnauthorized},
		"bad request":         {status: http.StatusBadRequest, wantIs: ErrUnauthorized},
		"trakt is down":       {status: http.StatusBadGateway},
		"empty token":         {body: `{"access_token":"","refresh_token":"x","expires_in":1}`},
		"not json":            {body: `<html>`},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			fake := newFakeOAuth(t)
			fake.status = tc.status
			if tc.body != "" {
				fake.body = tc.body
			}

			cfg := testConfig(fake.URL)
			cfg.ClientSecret = "client-secret"

			_, err := NewOAuth(cfg).Refresh(context.Background(), "old-refresh")
			if err == nil {
				t.Fatal("Refresh() succeeded, want an error")
			}
			if tc.wantIs != nil {
				if !errors.Is(err, tc.wantIs) {
					t.Fatalf("error = %v, want it to wrap %v", err, tc.wantIs)
				}
				return
			}

			// A non-4xx failure must never be classified as ErrUnauthorized: that
			// would tell an operator to re-authorize the plugin during, say, a
			// trakt outage rather than a bad grant.
			if errors.Is(err, ErrUnauthorized) {
				t.Fatalf("error = %v, want it not to wrap ErrUnauthorized", err)
			}
		})
	}
}
