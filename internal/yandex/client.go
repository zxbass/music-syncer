// Package yandex implements the Yandex Music API adapter.
//
// Yandex Music does not publish an official public API. This client uses the
// same endpoints as the official mobile/web clients with a user OAuth token.
// Endpoints can change without notice.
package yandex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/zxbass/music-syncer/internal/domain"
	"github.com/zxbass/music-syncer/internal/httpx"
)

const defaultBaseURL = "https://api.music.yandex.net"

// Client is a low-level Yandex Music API client.
type Client struct {
	http     *httpx.Client
	baseURL  string
	token    string
	clientID string
	log      *slog.Logger
}

// Option customizes a Client.
type Option func(*Client)

// WithBaseURL overrides the API endpoint (used by tests).
func WithBaseURL(base string) Option {
	return func(c *Client) { c.baseURL = strings.TrimRight(base, "/") }
}

// WithHTTPClient replaces the HTTP transport.
func WithHTTPClient(h *httpx.Client) Option { return func(c *Client) { c.http = h } }

// WithLogger sets a logger.
func WithLogger(l *slog.Logger) Option {
	return func(c *Client) {
		if l != nil {
			c.log = l
		}
	}
}

// New creates a Yandex Music client.
func New(token string, opts ...Option) *Client {
	token = strings.TrimSpace(token)
	switch {
	case len(token) >= 6 && strings.EqualFold(token[:6], "oauth "):
		token = strings.TrimSpace(token[6:])
	case len(token) >= 7 && strings.EqualFold(token[:7], "bearer "):
		token = strings.TrimSpace(token[7:])
	}
	c := &Client{
		http:     httpx.New(120*time.Millisecond, nil),
		baseURL:  defaultBaseURL,
		token:    token,
		clientID: "YandexMusicAndroid/24023621",
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
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

type envelope struct {
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

func (c *Client) request(ctx context.Context, method, path string, query, form url.Values, out any) error {
	if c.token == "" {
		return &domain.APIError{Provider: domain.Yandex, StatusCode: 401,
			Message: "no Yandex Music token; run `music-syncer auth yandex`"}
	}
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "OAuth "+c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "music-syncer/0.1 (+https://github.com/zxbass/music-syncer)")
	req.Header.Set("X-Yandex-Music-Client", c.clientID)
	req.Header.Set("Accept-Language", "ru")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("yandex %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 50<<20))
	if err != nil {
		return fmt.Errorf("yandex %s %s: read response: %w", method, path, err)
	}
	if resp.StatusCode/100 != 2 {
		return yandexError(resp.StatusCode, data)
	}
	if out == nil {
		return nil
	}

	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("yandex %s %s: decode envelope: %w", method, path, err)
	}
	result := env.Result
	if len(result) == 0 || string(result) == "null" {
		// A few endpoints return the payload without an envelope.
		result = data
	}
	if err := json.Unmarshal(result, out); err != nil {
		return fmt.Errorf("yandex %s %s: decode result: %w", method, path, err)
	}
	return nil
}

func yandexError(status int, data []byte) error {
	message := httpx.Snippet(data, 800)
	var payload struct {
		Error struct {
			Name    string `json:"name"`
			Message string `json:"message"`
		} `json:"error"`
		Result struct {
			Name    string `json:"name"`
			Message string `json:"message"`
		} `json:"result"`
	}
	if json.Unmarshal(data, &payload) == nil {
		switch {
		case payload.Error.Message != "":
			message = payload.Error.Name + ": " + payload.Error.Message
		case payload.Result.Message != "":
			message = payload.Result.Name + ": " + payload.Result.Message
		}
	}
	if status == http.StatusUnauthorized {
		message = "token is invalid or expired; run `music-syncer auth yandex`"
	}
	return &domain.APIError{Provider: domain.Yandex, StatusCode: status, Message: message}
}

// FlexID accepts both JSON numbers and strings. Yandex is inconsistent: track
// and album IDs are usually numbers, but sometimes stringified.
type FlexID string

// UnmarshalJSON implements json.Unmarshaler.
func (f *FlexID) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || string(data) == "null" {
		*f = ""
		return nil
	}
	if data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		*f = FlexID(s)
		return nil
	}
	*f = FlexID(data)
	return nil
}

// String returns the identifier as a string.
func (f FlexID) String() string { return string(f) }

// Int returns the identifier as an integer, or 0.
func (f FlexID) Int() int {
	n, _ := strconv.Atoi(string(f))
	return n
}

type accountStatus struct {
	Account struct {
		UID         FlexID `json:"uid"`
		Login       string `json:"login"`
		DisplayName string `json:"display_name"`
		FullName    string `json:"full_name"`
		FirstName   string `json:"first_name"`
	} `json:"account"`
	Plus *struct {
		HasPlus bool `json:"hasPlus"`
	} `json:"plus"`
}

type playlist struct {
	UID                   FlexID     `json:"uid"`
	Kind                  FlexID     `json:"kind"`
	Title                 string     `json:"title"`
	Description           string     `json:"description"`
	TrackCount            int        `json:"trackCount"`
	Revision              int        `json:"revision"`
	Snapshot              int        `json:"snapshot"`
	Visibility            string     `json:"visibility"`
	GeneratedPlaylistType string     `json:"generatedPlaylistType"`
	Type                  string     `json:"type"`
	Owner                 user       `json:"owner"`
	Tracks                []trackRef `json:"tracks"`
}

type user struct {
	UID         FlexID `json:"uid"`
	Login       string `json:"login"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
}

type trackRef struct {
	ID      FlexID `json:"id"`
	AlbumID FlexID `json:"albumId"`
	Track   *track `json:"track"`
}

type track struct {
	ID            FlexID   `json:"id"`
	Title         string   `json:"title"`
	DurationMS    int      `json:"durationMs"`
	ISRC          string   `json:"isrc"`
	Explicit      bool     `json:"explicit"`
	Available     bool     `json:"available"`
	Version       string   `json:"version"`
	UGCArtistName string   `json:"ugcArtistName"`
	Artists       []artist `json:"artists"`
	Albums        []album  `json:"albums"`
}

type artist struct {
	ID   FlexID `json:"id"`
	Name string `json:"name"`
}

type album struct {
	ID    FlexID `json:"id"`
	Title string `json:"title"`
}

type searchResult[T any] struct {
	Results []T `json:"results"`
	Total   int `json:"total"`
	PerPage int `json:"perPage"`
	Page    int `json:"page"`
}

type searchResponse struct {
	Tracks  searchResult[track]  `json:"tracks"`
	Artists searchResult[artist] `json:"artists"`
}

type tracksList struct {
	Library struct {
		UID      FlexID     `json:"uid"`
		Revision int        `json:"revision"`
		Tracks   []trackRef `json:"tracks"`
	} `json:"library"`
}

type likedArtist struct {
	Type      string  `json:"type"`
	ID        FlexID  `json:"id"`
	Timestamp string  `json:"timestamp"`
	Artist    *artist `json:"artist"`
	// Some API versions return the artist object directly.
	Name string `json:"name"`
}

type changeOp struct {
	Op     string      `json:"op"`
	At     *int        `json:"at,omitempty"`
	From   *int        `json:"from,omitempty"`
	To     *int        `json:"to,omitempty"`
	Tracks []changeRef `json:"tracks,omitempty"`
}

func insertOp(at int, refs []changeRef) changeOp {
	return changeOp{Op: "insert", At: &at, Tracks: refs}
}

func deleteOp(from, to int) changeOp {
	return changeOp{Op: "delete", From: &from, To: &to}
}

type changeRef struct {
	ID      string `json:"id"`
	AlbumID string `json:"albumId"`
}
