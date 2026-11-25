// Package httpx provides a small retrying, rate-limited HTTP helper shared by
// the Yandex Music and Spotify clients.
package httpx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Client wraps http.Client with a minimum interval between request starts and
// retries for transient failures.
//
// Only rate-limit responses and GET requests are retried automatically.
// Mutating POST requests are not retried on 5xx because a request that failed
// after being received may have already created a playlist or added tracks.
type Client struct {
	HTTP        *http.Client
	MinInterval time.Duration
	MaxRetries  int
	Logger      *slog.Logger

	mu   sync.Mutex
	next time.Time
}

// New creates a client. A zero minInterval disables pacing.
func New(minInterval time.Duration, logger *slog.Logger) *Client {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Client{
		HTTP: &http.Client{
			Timeout: 60 * time.Second,
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				MaxIdleConns:          20,
				MaxIdleConnsPerHost:   10,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
			},
		},
		MinInterval: minInterval,
		MaxRetries:  5,
		Logger:      logger,
	}
}

// Do sends a request, waiting for the rate limiter and retrying transient
// failures. The caller owns the response body.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	attempts := c.MaxRetries + 1
	var lastErr error

	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			// GetBody is set automatically for requests built with bytes.Reader,
			// strings.Reader or bytes.Buffer. If it is missing we cannot safely
			// replay the request.
			if req.Body != nil && req.GetBody == nil {
				return nil, lastErr
			}
			if req.Body != nil {
				body, err := req.GetBody()
				if err != nil {
					return nil, fmt.Errorf("rewind request body: %w", err)
				}
				req.Body = body
			}
		}

		if err := c.wait(req.Context()); err != nil {
			return nil, err
		}

		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = err
			if !isIdempotent(req.Method) || req.Context().Err() != nil {
				return nil, err
			}
			if attempt == attempts-1 {
				return nil, err
			}
			if err := sleep(req.Context(), backoff(attempt)); err != nil {
				return nil, err
			}
			c.Logger.Debug("retrying request after network error",
				"method", req.Method, "url", redactURL(req.URL.String()), "attempt", attempt+1, "error", err)
			continue
		}

		if resp.StatusCode == http.StatusTooManyRequests {
			delay := retryAfter(resp, backoff(attempt))
			drainAndClose(resp.Body)
			if attempt == attempts-1 {
				return nil, fmt.Errorf("rate limited after %d attempts", attempts)
			}
			c.Logger.Warn("rate limited, waiting", "provider_host", req.URL.Host, "delay", delay)
			if err := sleep(req.Context(), delay); err != nil {
				return nil, err
			}
			continue
		}

		if resp.StatusCode >= 500 && isIdempotent(req.Method) && attempt < attempts-1 {
			drainAndClose(resp.Body)
			delay := backoff(attempt)
			c.Logger.Debug("retrying request after server error",
				"method", req.Method, "url", redactURL(req.URL.String()), "status", resp.StatusCode, "delay", delay)
			if err := sleep(req.Context(), delay); err != nil {
				return nil, err
			}
			continue
		}

		return resp, nil
	}

	if lastErr != nil {
		return nil, lastErr
	}
	return nil, errors.New("request failed")
}

func (c *Client) wait(ctx context.Context) error {
	if c.MinInterval <= 0 {
		return ctx.Err()
	}
	c.mu.Lock()
	now := time.Now()
	if c.next.Before(now) {
		c.next = now
	}
	delay := c.next.Sub(now)
	c.next = c.next.Add(c.MinInterval)
	c.mu.Unlock()

	if delay <= 0 {
		return ctx.Err()
	}
	return sleep(ctx, delay)
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func backoff(attempt int) time.Duration {
	base := 500 * time.Millisecond << uint(attempt)
	if base > 10*time.Second {
		base = 10 * time.Second
	}
	jitter := time.Duration(rand.Int63n(int64(base / 2)))
	return base + jitter
}

func retryAfter(resp *http.Response, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if value == "" {
		return fallback
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		d := time.Duration(seconds) * time.Second
		if d > 60*time.Second {
			d = 60 * time.Second
		}
		return d
	}
	if t, err := http.ParseTime(value); err == nil {
		d := time.Until(t)
		if d < 0 {
			return fallback
		}
		if d > 60*time.Second {
			d = 60 * time.Second
		}
		return d
	}
	return fallback
}

func isIdempotent(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete:
		return true
	default:
		return false
	}
}

// Snippet returns a bounded, trimmed string for error messages.
func Snippet(data []byte, max int) string {
	s := strings.TrimSpace(string(data))
	if max > 0 && len(s) > max {
		return s[:max] + "…"
	}
	return s
}

func drainAndClose(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 64<<10))
	_ = body.Close()
}

func redactURL(raw string) string {
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		return raw[:i] + "?…"
	}
	return raw
}
