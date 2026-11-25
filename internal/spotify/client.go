// Package spotify implements the Spotify Web API adapter.
package spotify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/zxbass/music-syncer/internal/domain"
	"github.com/zxbass/music-syncer/internal/httpx"
)

const (
	defaultAPIBase      = "https://api.spotify.com/v1"
	defaultAccountsBase = "https://accounts.spotify.com"
)

// Token is an OAuth access token plus the refresh token needed to renew it.
type Token struct {
	AccessToken  string    `json:"access_token"`
	TokenType    string    `json:"token_type"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	Expiry       time.Time `json:"expiry,omitempty"`
}

// Expired reports whether the access token is expired or about to expire.
func (t Token) Expired() bool {
	if t.AccessToken == "" {
		return true
	}
	if t.Expiry.IsZero() {
		return false
	}
	return time.Now().Add(60 * time.Second).After(t.Expiry)
}

// Client is a low-level Spotify Web API client. It refreshes the access token
// automatically and calls OnToken whenever the token changes.
type Client struct {
	http         *httpx.Client
	apiBase      string
	accountsBase string
	clientID     string
	onToken      func(Token)
	log          *slog.Logger

	mu    sync.Mutex
	token Token
}

// Option customizes a Client.
type Option func(*Client)

// WithHTTPClient replaces the HTTP transport.
func WithHTTPClient(h *httpx.Client) Option {
	return func(c *Client) { c.http = h }
}

// WithLogger sets a logger.
func WithLogger(l *slog.Logger) Option {
	return func(c *Client) {
		if l != nil {
			c.log = l
		}
	}
}

// WithTokenUpdate registers a callback invoked after a token refresh.
func WithTokenUpdate(fn func(Token)) Option { return func(c *Client) { c.onToken = fn } }

// WithBaseURLs overrides the API endpoints. It is used by tests.
func WithBaseURLs(api, accounts string) Option {
	return func(c *Client) {
		c.apiBase = strings.TrimRight(api, "/")
		c.accountsBase = strings.TrimRight(accounts, "/")
	}
}

// New creates a Spotify client.
func New(clientID string, token Token, opts ...Option) *Client {
	c := &Client{
		http:         httpx.New(100*time.Millisecond, nil),
		apiBase:      defaultAPIBase,
		accountsBase: defaultAccountsBase,
		clientID:     clientID,
		token:        token,
		log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	for _, opt := range opts {
		opt(c)
	}
	if c.log == nil {
		c.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	c.http.Logger = c.log
	return c
}

// Token returns a copy of the current token.
func (c *Client) Token() Token {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token
}

// SetToken replaces the current token.
func (c *Client) SetToken(token Token) {
	c.mu.Lock()
	c.token = token
	c.mu.Unlock()
}

func (c *Client) ensureToken(ctx context.Context, force bool) error {
	c.mu.Lock()
	needsRefresh := force || c.token.Expired()
	if !needsRefresh {
		c.mu.Unlock()
		return nil
	}
	if c.token.RefreshToken == "" {
		token := c.token
		c.mu.Unlock()
		if token.AccessToken != "" && !token.Expired() {
			return nil
		}
		return &domain.APIError{Provider: domain.Spotify, StatusCode: 401,
			Message: "access token expired and no refresh token; run `music-syncer auth spotify`"}
	}
	c.mu.Unlock()

	// Serialize refreshes: another goroutine may have refreshed while we
	// waited for the lock.
	c.mu.Lock()
	defer c.mu.Unlock()
	if !force && !c.token.Expired() {
		return nil
	}
	token, err := c.refreshLocked(ctx)
	if err != nil {
		return err
	}
	c.token = token
	if c.onToken != nil {
		c.onToken(token)
	}
	return nil
}

func (c *Client) refreshLocked(ctx context.Context) (Token, error) {
	if c.token.RefreshToken == "" {
		return Token{}, &domain.APIError{Provider: domain.Spotify, StatusCode: 401,
			Message: "no refresh token; run `music-syncer auth spotify`"}
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {c.token.RefreshToken},
		"client_id":     {c.clientID},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.accountsBase+"/api/token",
		strings.NewReader(form.Encode()))
	if err != nil {
		return Token{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.HTTP.Do(req)
	if err != nil {
		return Token{}, fmt.Errorf("spotify token refresh: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Token{}, fmt.Errorf("spotify token refresh: read response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return Token{}, &domain.APIError{Provider: domain.Spotify, StatusCode: 401,
			Message: "token refresh failed: " + tokenError(resp.StatusCode, data).Error()}
	}
	var payload struct {
		AccessToken  string `json:"access_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return Token{}, fmt.Errorf("spotify token refresh: %w", err)
	}
	refresh := payload.RefreshToken
	if refresh == "" {
		refresh = c.token.RefreshToken
	}
	return Token{
		AccessToken:  payload.AccessToken,
		TokenType:    payload.TokenType,
		RefreshToken: refresh,
		Expiry:       time.Now().Add(time.Duration(payload.ExpiresIn) * time.Second),
	}, nil
}

// Get performs an authenticated GET request.
func (c *Client) Get(ctx context.Context, path string, query url.Values, out any) error {
	return c.do(ctx, http.MethodGet, path, query, nil, out)
}

// Post performs an authenticated POST request with a JSON body.
func (c *Client) Post(ctx context.Context, path string, query url.Values, body, out any) error {
	return c.do(ctx, http.MethodPost, path, query, body, out)
}

// Put performs an authenticated PUT request with a JSON body.
func (c *Client) Put(ctx context.Context, path string, query url.Values, body any) error {
	return c.do(ctx, http.MethodPut, path, query, body, nil)
}

// Delete performs an authenticated DELETE request with a JSON body.
func (c *Client) Delete(ctx context.Context, path string, query url.Values, body any) error {
	return c.do(ctx, http.MethodDelete, path, query, body, nil)
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("spotify: encode request: %w", err)
		}
	}

	for attempt := 0; attempt < 2; attempt++ {
		if err := c.ensureToken(ctx, attempt > 0); err != nil {
			return err
		}

		u := c.apiBase + path
		if len(query) > 0 {
			u += "?" + query.Encode()
		}
		var reader io.Reader
		if payload != nil {
			reader = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, u, reader)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "music-syncer/0.1 (+https://github.com/zxbass/music-syncer)")
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		c.mu.Lock()
		access := c.token.AccessToken
		c.mu.Unlock()
		req.Header.Set("Authorization", "Bearer "+access)

		resp, err := c.http.Do(req)
		if err != nil {
			return fmt.Errorf("spotify %s %s: %w", method, path, err)
		}

		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
			_ = resp.Body.Close()
			c.log.Debug("spotify access token rejected, refreshing")
			continue
		}

		if resp.StatusCode/100 != 2 {
			defer resp.Body.Close()
			data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			return apiError(resp.StatusCode, data)
		}

		if out == nil || resp.StatusCode == http.StatusNoContent {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
			return resp.Body.Close()
		}
		dec := json.NewDecoder(io.LimitReader(resp.Body, 20<<20))
		if err := dec.Decode(out); err != nil {
			resp.Body.Close()
			return fmt.Errorf("spotify %s %s: decode response: %w", method, path, err)
		}
		return resp.Body.Close()
	}
	return &domain.APIError{Provider: domain.Spotify, StatusCode: 401,
		Message: "unauthorized after token refresh; run `music-syncer auth spotify`"}
}

func apiError(status int, data []byte) error {
	message := httpx.Snippet(data, 800)
	var payload struct {
		Error struct {
			Status  int    `json:"status"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &payload) == nil && payload.Error.Message != "" {
		message = payload.Error.Message
	}
	return &domain.APIError{Provider: domain.Spotify, StatusCode: status, Message: message}
}

func tokenError(status int, data []byte) error {
	message := strings.TrimSpace(string(data))
	var payload struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if json.Unmarshal(data, &payload) == nil {
		switch {
		case payload.ErrorDescription != "":
			message = payload.Error + ": " + payload.ErrorDescription
		case payload.Error != "":
			message = payload.Error
		}
	}
	return fmt.Errorf("spotify token endpoint: HTTP %d: %s", status, message)
}
