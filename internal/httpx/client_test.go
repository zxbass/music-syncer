package httpx

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testClient() *Client {
	c := New(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.MaxRetries = 3
	return c
}

func TestRetry429ReplaysBody(t *testing.T) {
	var calls int32
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(data))
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := testClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
	if len(bodies) != 2 || bodies[0] != "payload" || bodies[1] != "payload" {
		t.Errorf("bodies = %v", bodies)
	}
}

func TestRetryWithNonReplayableBodyReturnsError(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL,
		io.NopCloser(strings.NewReader("payload")))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := testClient().Do(req)
	if err == nil {
		if resp != nil {
			resp.Body.Close()
		}
		t.Fatal("expected an error for a non-replayable body")
	}
	if resp != nil {
		resp.Body.Close()
		t.Fatalf("response = %v, want nil", resp)
	}
	if !strings.Contains(err.Error(), "replayable") {
		t.Errorf("error = %v", err)
	}
}

func TestPostNotRetriedOn500(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader("x"))
	resp, err := testClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestGetRetriedOn500(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	start := time.Now()
	resp, err := testClient().Do(mustRequest(t, http.MethodGet, srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
	if time.Since(start) < 400*time.Millisecond {
		t.Errorf("retry happened without backoff")
	}
}

func TestUnauthorizedNotRetried(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	resp, err := testClient().Do(mustRequest(t, http.MethodGet, srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
}

func TestRetryAfterParsing(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Retry-After", "2")
	if got := retryAfter(resp, time.Second); got != 2*time.Second {
		t.Errorf("seconds: got %v", got)
	}
	resp.Header.Set("Retry-After", "invalid")
	if got := retryAfter(resp, 3*time.Second); got != 3*time.Second {
		t.Errorf("fallback: got %v", got)
	}
	future := time.Now().Add(5 * time.Second).UTC().Format(http.TimeFormat)
	resp.Header.Set("Retry-After", future)
	if got := retryAfter(resp, time.Second); got < 3*time.Second || got > 6*time.Second {
		t.Errorf("date: got %v", got)
	}
}

func mustRequest(t *testing.T, method, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}
