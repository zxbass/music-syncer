// Package httpx provides a small retrying, rate-limited HTTP helper shared by
// the Yandex Music and Spotify clients.
package httpx

import (
	"context"
	"encoding/json"
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

var (
	// ErrRateLimited indicates that a provider kept returning HTTP 429 after
	// automatic retries. The work already completed is safe to keep; the
	// command can be resumed later.
	ErrRateLimited = errors.New("rate limited")
	// ErrQuotaExceeded indicates that a provider's development quota is
	// exhausted. Waiting for the usual retry delay does not help; the quota
	// resets on the provider's schedule.
	ErrQuotaExceeded = errors.New("provider quota exceeded")
)

// maxAutomaticRetryDelay is the longest 429 wait that is performed
// automatically. Longer limits are reported to the caller so an interactive
// run does not appear to hang for hours.
const maxAutomaticRetryDelay = 2 * time.Minute

// RateLimitError describes an HTTP 429 returned by a provider.
type RateLimitError struct {
	Host          string
	RetryAfter    time.Duration
	QuotaExceeded bool
}

// Error implements error.
func (e *RateLimitError) Error() string {
	host := e.Host
	if host == "" {
		host = "provider"
	}
	limit := "rate limited"
	if e.QuotaExceeded {
		limit = "development quota exhausted"
	}
	if e.RetryAfter > 0 {
		return fmt.Sprintf("%s: %s; retry after %s", host, limit, e.RetryAfter.Round(time.Second))
	}
	return fmt.Sprintf("%s: %s", host, limit)
}

// Unwrap makes errors.Is(err, ErrRateLimited/ErrQuotaExceeded) work through
// provider error wrappers.
func (e *RateLimitError) Unwrap() error {
	if e.QuotaExceeded {
		return ErrQuotaExceeded
	}
	return ErrRateLimited
}

// Client wraps http.Client with a minimum interval between request starts and
// retries for transient failures.
//
// Ordinary rate-limit responses and GET requests are retried automatically.
// Provider quota exhaustion (HTTP 429 with reason QUOTA_EXCEEDED) is returned
// immediately because waiting cannot help. Mutating POST requests are not
// retried on 5xx because a request that failed after being received may have
// already created a playlist or added tracks.
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
				return nil, errors.New("cannot retry request: body is not replayable")
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
			data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
			_ = resp.Body.Close()
			if quotaExceeded(data) {
				return nil, &RateLimitError{
					Host:          req.URL.Host,
					RetryAfter:    retryAfter(resp, 0),
					QuotaExceeded: true,
				}
			}
			delay := retryAfter(resp, backoff(attempt))
			// A very long Retry-After means the provider does not expect the
			// client to wait automatically. Return the limit to the caller so
			// progress can be checkpointed and the command resumed later.
			if delay > maxAutomaticRetryDelay {
				return nil, &RateLimitError{Host: req.URL.Host, RetryAfter: delay}
			}
			if attempt == attempts-1 {
				return nil, &RateLimitError{Host: req.URL.Host, RetryAfter: delay}
			}
			c.Logger.Warn("rate limited, waiting", "provider_host", req.URL.Host, "delay", delay)
			c.slowDown()
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
	c.mu.Lock()
	interval := c.MinInterval
	if interval <= 0 {
		c.mu.Unlock()
		return ctx.Err()
	}
	now := time.Now()
	if c.next.Before(now) {
		c.next = now
	}
	delay := c.next.Sub(now)
	c.next = c.next.Add(interval)
	c.mu.Unlock()

	if delay <= 0 {
		return ctx.Err()
	}
	return sleep(ctx, delay)
}

// slowDown increases the pacing interval after a rate limit so the next
// requests do not immediately consume the same budget again. It is a no-op
// when pacing is disabled (as in focused unit tests).
func (c *Client) slowDown() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.MinInterval <= 0 {
		return
	}
	next := c.MinInterval * 2
	if next < 500*time.Millisecond {
		next = 500 * time.Millisecond
	}
	if next > 5*time.Second {
		next = 5 * time.Second
	}
	if next != c.MinInterval {
		c.Logger.Debug("slowing request pacing", "min_interval", next)
		c.MinInterval = next
	}
}

func quotaExceeded(data []byte) bool {
	var payload struct {
		Error struct {
			Reason string `json:"reason"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(payload.Error.Reason), "QUOTA_EXCEEDED")
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
		return time.Duration(seconds) * time.Second
	}
	if t, err := http.ParseTime(value); err == nil {
		d := time.Until(t)
		if d < 0 {
			return fallback
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
