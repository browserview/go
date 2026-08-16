package browserview

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const sessionBody = `{
	"id": "3f9c62d81b4a", "status": "running", "health": "healthy",
	"start_url": "https://example.com", "width": 1280, "height": 800,
	"created_at": "2026-07-29T00:00:00Z", "owner": "usr_123",
	"mem_limit_bytes": 1342177280,
	"viewer_url": "/sessions/3f9c62d81b4a/viewer?token=ctl",
	"watch_url": "/sessions/3f9c62d81b4a/viewer?token=view",
	"cdp_url": "/sessions/3f9c62d81b4a/cdp",
	"cdp_token": "cdp-token", "token_ttl_seconds": 3600
}`

// noSleep replaces the client's retry sleep with one that records requested
// durations and returns immediately, keeping tests fast.
func noSleep(c *Client, record *[]time.Duration) {
	c.sleep = func(ctx context.Context, d time.Duration) error {
		*record = append(*record, d)
		return ctx.Err()
	}
}

func TestCreateSession(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer bv_live_abc" {
			t.Errorf("Authorization = %q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["start_url"] != "https://example.com" {
			t.Errorf("start_url = %v", body["start_url"])
		}
		if _, present := body["wait"]; present {
			t.Error("wait should be omitted when nil")
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(sessionBody))
	}))
	defer server.Close()

	client := NewWithBaseURL("bv_live_abc", server.URL)
	session, err := client.CreateSession(context.Background(), CreateSessionOptions{
		StartURL: "https://example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if session.ID != "3f9c62d81b4a" {
		t.Errorf("ID = %q", session.ID)
	}
	if session.MemLimitBytes != 1342177280 {
		t.Errorf("MemLimitBytes = %d", session.MemLimitBytes)
	}
	if want := server.URL + "/sessions/3f9c62d81b4a/viewer?token=ctl"; session.ViewerURL != want {
		t.Errorf("ViewerURL = %q, want %q", session.ViewerURL, want)
	}
	if want := server.URL + "/sessions/3f9c62d81b4a/cdp"; session.CDPURL != want {
		t.Errorf("CDPURL = %q, want %q", session.CDPURL, want)
	}
}

func TestGetSessionDecodesHealthFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{
			"id": "3f9c62d81b4a", "status": "running", "health": "healthy",
			"start_url": "about:blank", "width": 1280, "height": 800,
			"created_at": "2026-07-29T00:00:00Z", "owner": "",
			"mem_limit_bytes": 1342177280, "restarts": 2, "degraded": true,
			"cdp_url": "/sessions/3f9c62d81b4a/cdp", "cdp_token": "t",
			"token_ttl_seconds": 3600
		}`))
	}))
	defer server.Close()

	client := NewWithBaseURL("k", server.URL)
	session, err := client.GetSession(context.Background(), "3f9c62d81b4a")
	if err != nil {
		t.Fatal(err)
	}
	if session.Restarts == nil || *session.Restarts != 2 {
		t.Errorf("Restarts = %v", session.Restarts)
	}
	if !session.Degraded {
		t.Error("Degraded = false, want true")
	}
	if session.MemLimitBytes != 1342177280 {
		t.Errorf("MemLimitBytes = %d", session.MemLimitBytes)
	}
}

func TestGetSessionNullRestarts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id": "abc", "status": "running", "restarts": null, "degraded": false}`))
	}))
	defer server.Close()

	client := NewWithBaseURL("k", server.URL)
	session, err := client.GetSession(context.Background(), "abc")
	if err != nil {
		t.Fatal(err)
	}
	if session.Restarts != nil {
		t.Errorf("Restarts = %v, want nil", session.Restarts)
	}
}

func TestDestroySessionAndErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"detail": "session not found"}`))
		}
	}))
	defer server.Close()

	client := NewWithBaseURL("bv_live_abc", server.URL)
	if err := client.DestroySession(context.Background(), "3f9c62d81b4a"); err != nil {
		t.Fatal(err)
	}

	_, err := client.GetSession(context.Background(), "nope")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %v", err)
	}
	if apiErr.StatusCode != 404 || apiErr.Message != "session not found" {
		t.Errorf("unexpected APIError: %+v", apiErr)
	}
	if apiErr.RetryAfter != 7*time.Second {
		t.Errorf("RetryAfter = %v, want 7s (parsed on all statuses)", apiErr.RetryAfter)
	}
}

func TestMintToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sessions/abc/tokens" {
			t.Errorf("path = %q", r.URL.Path)
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["scope"] != "view" || body["ttl_seconds"] != float64(600) {
			t.Errorf("body = %v", body)
		}
		w.Write([]byte(`{"token": "t", "scope": "view", "ttl_seconds": 600}`))
	}))
	defer server.Close()

	client := NewWithBaseURL("bv_live_abc", server.URL)
	token, err := client.MintToken(context.Background(), "abc", ScopeView, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if token.Token != "t" || token.TTLSeconds != 600 {
		t.Errorf("token = %+v", token)
	}
}

func TestRetryOn429WithRetryAfterThenSuccess(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"detail": "session limit reached"}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(sessionBody))
	}))
	defer server.Close()

	client := NewWithBaseURL("k", server.URL)
	var slept []time.Duration
	noSleep(client, &slept)

	session, err := client.CreateSession(context.Background(), CreateSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if session.ID != "3f9c62d81b4a" {
		t.Errorf("ID = %q", session.ID)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("calls = %d, want 2", got)
	}
	if len(slept) != 1 || slept[0] != 2*time.Second {
		t.Errorf("slept = %v, want [2s] (Retry-After honored)", slept)
	}
}

func TestRetryOn503(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"detail": "auth temporarily unavailable"}`))
			return
		}
		w.Write([]byte(`[]`))
	}))
	defer server.Close()

	client := NewWithBaseURL("k", server.URL)
	var slept []time.Duration
	noSleep(client, &slept)

	if _, err := client.ListSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("calls = %d, want 3", got)
	}
	// No Retry-After header: exponential backoff 1s then 2s.
	if len(slept) != 2 || slept[0] != time.Second || slept[1] != 2*time.Second {
		t.Errorf("slept = %v, want [1s 2s]", slept)
	}
}

func TestRetryExhaustionReturnsAPIError(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"detail": "rate limited"}`))
	}))
	defer server.Close()

	client := NewWithBaseURL("k", server.URL)
	var slept []time.Duration
	noSleep(client, &slept)

	_, err := client.ListSessions(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %v", err)
	}
	if apiErr.StatusCode != 429 || apiErr.Message != "rate limited" || apiErr.RetryAfter != 30*time.Second {
		t.Errorf("unexpected APIError: %+v", apiErr)
	}
	if got := calls.Load(); got != 4 { // initial + DefaultMaxRetries
		t.Errorf("calls = %d, want 4", got)
	}
}

func TestMaxRetriesZeroDisablesRetries(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	client, err := NewWithOptions("k", WithBaseURL(server.URL), WithMaxRetries(0))
	if err != nil {
		t.Fatal(err)
	}
	var slept []time.Duration
	noSleep(client, &slept)

	_, err = client.ListSessions(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 503 {
		t.Fatalf("want 503 APIError, got %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("calls = %d, want 1", got)
	}
}

func TestRetryAfterHTTPDate(t *testing.T) {
	future := time.Now().UTC().Add(10 * time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", future.Format(http.TimeFormat))
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"detail": "rate limited"}`))
	}))
	defer server.Close()

	client, err := NewWithOptions("k", WithBaseURL(server.URL), WithMaxRetries(0))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.ListSessions(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %v", err)
	}
	// Allow slack for time elapsed between response and parse.
	if apiErr.RetryAfter < 5*time.Second || apiErr.RetryAfter > 10*time.Second {
		t.Errorf("RetryAfter = %v, want ~10s from HTTP-date", apiErr.RetryAfter)
	}
}

func TestRetryAfterDateCappedAt30s(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "600")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`[]`))
	}))
	defer server.Close()

	client := NewWithBaseURL("k", server.URL)
	var slept []time.Duration
	noSleep(client, &slept)

	if _, err := client.ListSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(slept) != 1 || slept[0] != 30*time.Second {
		t.Errorf("slept = %v, want [30s] (single wait capped)", slept)
	}
}

func TestContextCancellationAbortsRetrySleep(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	client := NewWithBaseURL("k", server.URL) // real sleepCtx
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := client.ListSessions(ctx)
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want context.DeadlineExceeded, got %v", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("retry sleep not aborted by context: took %v", elapsed)
	}
}

func TestListSessionsResolvesURLs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The server does not currently return URLs on list; this guards the
		// future-proof resolution path.
		w.Write([]byte(`[
			{"id": "a1", "status": "running", "mem_limit_bytes": 1342177280,
			 "viewer_url": "/sessions/a1/viewer?token=x"},
			{"id": "b2", "status": "running"}
		]`))
	}))
	defer server.Close()

	client := NewWithBaseURL("k", server.URL)
	sessions, err := client.ListSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 {
		t.Fatalf("len = %d", len(sessions))
	}
	if want := server.URL + "/sessions/a1/viewer?token=x"; sessions[0].ViewerURL != want {
		t.Errorf("ViewerURL = %q, want %q", sessions[0].ViewerURL, want)
	}
	if sessions[0].MemLimitBytes != 1342177280 {
		t.Errorf("MemLimitBytes = %d", sessions[0].MemLimitBytes)
	}
	if sessions[1].ViewerURL != "" {
		t.Errorf("ViewerURL = %q, want empty", sessions[1].ViewerURL)
	}
}

func TestNewHonorsBaseURLEnv(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[]`))
	}))
	defer server.Close()

	t.Setenv(EnvBaseURL, server.URL)
	client := New("k")
	if _, err := client.ListSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestNewFromEnv(t *testing.T) {
	t.Setenv(EnvAPIKey, "")
	if _, err := NewFromEnv(); err == nil {
		t.Error("want error when BROWSERVIEW_API_KEY is unset")
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer env-key" {
			t.Errorf("Authorization = %q", got)
		}
		w.Write([]byte(`[]`))
	}))
	defer server.Close()

	t.Setenv(EnvAPIKey, "env-key")
	t.Setenv(EnvBaseURL, server.URL)
	client, err := NewFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestParseRetryAfter(t *testing.T) {
	if d, ok := parseRetryAfter("30"); !ok || d != 30*time.Second {
		t.Errorf("parseRetryAfter(30) = %v, %v", d, ok)
	}
	if d, ok := parseRetryAfter("-5"); !ok || d != 0 {
		t.Errorf("parseRetryAfter(-5) = %v, %v", d, ok)
	}
	if _, ok := parseRetryAfter(""); ok {
		t.Error("parseRetryAfter empty should be false")
	}
	if _, ok := parseRetryAfter("garbage"); ok {
		t.Error("parseRetryAfter garbage should be false")
	}
	past := time.Now().Add(-time.Minute).UTC().Format(http.TimeFormat)
	if d, ok := parseRetryAfter(past); !ok || d != 0 {
		t.Errorf("parseRetryAfter(past date) = %v, %v", d, ok)
	}
}

func TestGetReplayAbsolutizesRelativeURLs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sessions/3f9c62d81b4a/replay" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Write([]byte(`{
			"status": "ready", "session_id": "3f9c62d81b4a",
			"started_at_ms": 1000, "ended_at_ms": 2000, "end_reason": "api",
			"video": {"url": "/sessions/3f9c62d81b4a/replay/files/video.webm?token=r",
			          "format": "webm", "codec": "vp8",
			          "start_time_ms": 1000, "duration_ms": 1000, "size_bytes": 42,
			          "segments": [{"start_ms": 1000, "offset_ms": 0, "duration_ms": 400},
			                       {"start_ms": 1600, "offset_ms": 400, "duration_ms": 600}]},
			"page_count": 1,
			"pages": [{"page_id": "p_000", "url": "https://example.com",
			           "start_time_ms": 1000, "end_time_ms": 2000}],
			"events": {"console": {"url": "https://bucket.s3.invalid/c.jsonl.gz?sig=1",
			                       "count": 2, "size_bytes": 10}},
			"urls_expire_at_ms": 99999
		}`))
	}))
	defer server.Close()

	client := NewWithBaseURL("k", server.URL)
	replay, err := client.GetReplay(context.Background(), "3f9c62d81b4a")
	if err != nil {
		t.Fatal(err)
	}
	if replay.Status != "ready" {
		t.Errorf("Status = %q", replay.Status)
	}
	if want := server.URL + "/sessions/3f9c62d81b4a/replay/files/video.webm?token=r"; replay.Video.URL != want {
		t.Errorf("Video.URL = %q, want %q", replay.Video.URL, want)
	}
	// Presigned (already absolute) URLs must be untouched.
	if want := "https://bucket.s3.invalid/c.jsonl.gz?sig=1"; replay.Events["console"].URL != want {
		t.Errorf("console URL = %q, want %q", replay.Events["console"].URL, want)
	}
	if replay.Pages[0].PageID != "p_000" {
		t.Errorf("PageID = %q", replay.Pages[0].PageID)
	}
	if len(replay.Video.Segments) != 2 {
		t.Fatalf("len(Segments) = %d, want 2", len(replay.Video.Segments))
	}
	if got := replay.Video.Segments[1]; got.StartMs != 1600 || got.OffsetMs != 400 || got.DurationMs != 600 {
		t.Errorf("Segments[1] = %+v", got)
	}
}

func TestWaitForReplayPollsThroughRecordingAnd404(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch calls {
		case 1:
			w.Write([]byte(`{"status": "recording", "session_id": "x"}`))
		case 2:
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"detail": "replay not found"}`))
		default:
			w.Write([]byte(`{"status": "ready", "session_id": "x"}`))
		}
	}))
	defer server.Close()

	client := NewWithBaseURL("k", server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	replay, err := client.WaitForReplay(ctx, "x", time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if replay.Status != "ready" || calls != 3 {
		t.Errorf("Status = %q, calls = %d", replay.Status, calls)
	}
}

func TestWaitForReplayFastFailsWhenNotRecorded(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"detail": "session is not being recorded"}`))
	}))
	defer server.Close()

	client := NewWithBaseURL("k", server.URL)
	_, err := client.WaitForReplay(context.Background(), "x", time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "record: true") {
		t.Fatalf("err = %v, want record: true hint", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNotFound {
		t.Fatalf("want wrapped 404 APIError, got %v", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (no polling)", calls)
	}
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestWaitForReplayDefaultDeadlineWhenContextHasNone(t *testing.T) {
	// A context without a deadline must not poll forever: WaitForReplay wraps
	// it with DefaultReplayWait. Observe the deadline from inside the request.
	var deadline time.Time
	var hasDeadline bool
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		deadline, hasDeadline = r.Context().Deadline()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"status": "ready", "session_id": "x"}`)),
		}, nil
	})}
	client, err := NewWithOptions("k", WithBaseURL("http://fake.invalid"), WithHTTPClient(hc))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := client.WaitForReplay(context.Background(), "x", time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if !hasDeadline {
		t.Fatal("request context has no deadline; want DefaultReplayWait applied")
	}
	if remaining := time.Until(deadline); remaining <= 0 || remaining > DefaultReplayWait {
		t.Errorf("deadline %v from now, want within DefaultReplayWait", remaining)
	}

	// A caller-supplied deadline is respected, not replaced.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if _, err := client.WaitForReplay(ctx, "x", time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if remaining := time.Until(deadline); remaining <= DefaultReplayWait {
		t.Errorf("caller deadline overridden: %v remaining, want ~10m", remaining)
	}
}

func TestWaitForReplayPropagatesOtherErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"detail": "nope"}`))
	}))
	defer server.Close()

	client := NewWithBaseURL("k", server.URL)
	_, err := client.WaitForReplay(context.Background(), "x", time.Millisecond)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusForbidden {
		t.Fatalf("err = %v", err)
	}
}

func TestCreateSessionSendsP2Knobs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		for key, want := range map[string]any{
			"start_url":            "about:blank",
			"stealth":              true,
			"downloads":            true,
			"user_agent":           "UA/1.0",
			"locale":               "en-GB",
			"timezone":             "America/New_York",
			"context_id":           "login-1",
			"solve_captchas":       true,
			"max_lifetime_seconds": float64(600),
		} {
			if body[key] != want {
				t.Errorf("%s = %v, want %v", key, body[key], want)
			}
		}
		proxy, _ := body["proxy"].(map[string]any)
		if proxy["server"] != "http://p:8080" || proxy["username"] != "u" {
			t.Errorf("proxy = %v", body["proxy"])
		}
		if _, present := proxy["bypass"]; present {
			t.Error("empty proxy bypass should be omitted")
		}
		geo, _ := body["geolocation"].(map[string]any)
		if geo["lat"] != 40.7 || geo["lon"] != -74.0 {
			t.Errorf("geolocation = %v", body["geolocation"])
		}
		metadata, _ := body["metadata"].(map[string]any)
		if metadata["job"] != "x" {
			t.Errorf("metadata = %v", body["metadata"])
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(sessionBody))
	}))
	defer server.Close()

	client := NewWithBaseURL("bv_live_abc", server.URL)
	_, err := client.CreateSession(context.Background(), CreateSessionOptions{
		StartURL:           "about:blank",
		Stealth:            true,
		Downloads:          true,
		UserAgent:          "UA/1.0",
		Locale:             "en-GB",
		Timezone:           "America/New_York",
		Geolocation:        &Geolocation{Lat: 40.7, Lon: -74.0},
		Proxy:              &ProxyConfig{Server: "http://p:8080", Username: "u", Password: "s"},
		ContextID:          "login-1",
		SolveCaptchas:      true,
		Metadata:           map[string]string{"job": "x"},
		MaxLifetimeSeconds: 600,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
}

func TestSessionDecodesConfigAndMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id": "abc", "status": "running", "health": "healthy",
			"config": {"stealth": true}, "metadata": {"job": "x"}}`))
	}))
	defer server.Close()

	client := NewWithBaseURL("bv_live_abc", server.URL)
	session, err := client.GetSession(context.Background(), "abc")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if session.Config["stealth"] != true {
		t.Errorf("Config = %v", session.Config)
	}
	if session.Metadata["job"] != "x" {
		t.Errorf("Metadata = %v", session.Metadata)
	}
}

func TestListSessionsByMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("metadata.job"); got != "x" {
			t.Errorf("metadata.job = %q", got)
		}
		w.Write([]byte(`[` + sessionBody + `]`))
	}))
	defer server.Close()

	client := NewWithBaseURL("bv_live_abc", server.URL)
	sessions, err := client.ListSessionsByMetadata(context.Background(), map[string]string{"job": "x"})
	if err != nil {
		t.Fatalf("ListSessionsByMetadata: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("len(sessions) = %d", len(sessions))
	}
}

func TestScreenshotReturnsRawBytes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sessions/abc/screenshot" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.URL.Query().Get("format") != "webp" || r.URL.Query().Get("quality") != "70" {
			t.Errorf("query = %q", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "image/webp")
		w.Write([]byte{1, 2, 3})
	}))
	defer server.Close()

	client := NewWithBaseURL("bv_live_abc", server.URL)
	image, err := client.Screenshot(context.Background(), "abc", ScreenshotOptions{Format: "webp", Quality: 70})
	if err != nil {
		t.Fatalf("Screenshot: %v", err)
	}
	if string(image) != string([]byte{1, 2, 3}) {
		t.Errorf("image = %v", image)
	}
}

func TestListAndDownloadFiles(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sessions/abc/downloads":
			w.Write([]byte(`{"session_id": "abc", "files": [{"name": "a.pdf", "size_bytes": 3, "modified_ms": 1}]}`))
		case "/sessions/abc/downloads/a.pdf":
			// Presigned-style redirect; the default http.Client follows it.
			http.Redirect(w, r, "/presigned/a.pdf", http.StatusFound)
		case "/presigned/a.pdf":
			w.Write([]byte("pdfbytes"))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := NewWithBaseURL("bv_live_abc", server.URL)
	files, err := client.ListDownloads(context.Background(), "abc")
	if err != nil {
		t.Fatalf("ListDownloads: %v", err)
	}
	if len(files) != 1 || files[0].Name != "a.pdf" {
		t.Fatalf("files = %v", files)
	}
	data, err := client.DownloadFile(context.Background(), "abc", "a.pdf")
	if err != nil {
		t.Fatalf("DownloadFile: %v", err)
	}
	if string(data) != "pdfbytes" {
		t.Errorf("data = %q", data)
	}
}

func TestUploadFileMultipart(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sessions/abc/files" {
			t.Errorf("path = %q", r.URL.Path)
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			t.Fatalf("FormFile: %v", err)
		}
		defer file.Close()
		if header.Filename != "in.csv" {
			t.Errorf("filename = %q", header.Filename)
		}
		data, _ := io.ReadAll(file)
		if string(data) != "col1,col2" {
			t.Errorf("content = %q", data)
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"name": "in.csv", "path": "/downloads/in.csv", "size_bytes": 9}`))
	}))
	defer server.Close()

	client := NewWithBaseURL("bv_live_abc", server.URL)
	result, err := client.UploadFile(context.Background(), "abc", "in.csv", []byte("col1,col2"))
	if err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
	if result.Path != "/downloads/in.csv" {
		t.Errorf("Path = %q", result.Path)
	}
}

func TestSolveCaptcha(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["type"] != CaptchaTurnstile || body["sitekey"] != "0xKEY" {
			t.Errorf("body = %v", body)
		}
		if _, present := body["action"]; present {
			t.Error("empty action should be omitted")
		}
		w.Write([]byte(`{"token": "solved", "type": "turnstile"}`))
	}))
	defer server.Close()

	client := NewWithBaseURL("bv_live_abc", server.URL)
	token, err := client.SolveCaptcha(context.Background(), "abc", SolveCaptchaOptions{
		Type: CaptchaTurnstile, Sitekey: "0xKEY", URL: "https://target.example",
	})
	if err != nil {
		t.Fatalf("SolveCaptcha: %v", err)
	}
	if token != "solved" {
		t.Errorf("token = %q", token)
	}
}

func TestSolveCaptchaDisabled501(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotImplemented)
		w.Write([]byte(`{"detail": "captcha solving is not enabled on this host"}`))
	}))
	defer server.Close()

	client := NewWithBaseURL("bv_live_abc", server.URL)
	_, err := client.SolveCaptcha(context.Background(), "abc", SolveCaptchaOptions{
		Type: CaptchaTurnstile, Sitekey: "k", URL: "https://x",
	})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNotImplemented {
		t.Fatalf("err = %v", err)
	}
}
