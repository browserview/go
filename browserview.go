// Package browserview is the official Go client for the browserview.io API.
//
// browserview.io provides disposable cloud Chromium sessions that humans can
// watch and control in a live viewer while agents drive the same browser via
// the Chrome DevTools Protocol.
//
//	client := browserview.New(os.Getenv("BROWSERVIEW_API_KEY"))
//	session, err := client.CreateSession(ctx, browserview.CreateSessionOptions{
//		StartURL: "https://example.com",
//	})
package browserview

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Version is the version of this SDK.
const Version = "0.4.0"

// DefaultBaseURL is the production API origin. It can be overridden with the
// BROWSERVIEW_BASE_URL environment variable or the WithBaseURL option;
// explicit options take precedence over the environment variable.
const DefaultBaseURL = "https://sessions.browserview.io"

// Environment variables honored by New, NewWithOptions, and NewFromEnv.
const (
	// EnvAPIKey names the environment variable read by NewFromEnv for the
	// API key.
	EnvAPIKey = "BROWSERVIEW_API_KEY"
	// EnvBaseURL names the environment variable that overrides
	// DefaultBaseURL.
	EnvBaseURL = "BROWSERVIEW_BASE_URL"
)

// Token scopes accepted by MintToken.
const (
	ScopeView    = "view"
	ScopeControl = "control"
	ScopeCDP     = "cdp"
)

// Client defaults, overridable with WithTimeout and WithMaxRetries.
const (
	// DefaultTimeout is the default per-request timeout. It is generous
	// because CreateSession blocks (~5s, sometimes up to ~60s server-side)
	// until the browser is ready unless Wait is set to false.
	DefaultTimeout = 90 * time.Second
	// DefaultMaxRetries is the default number of retries after the initial
	// attempt.
	DefaultMaxRetries = 3
	// maxRetryWait caps a single Retry-After-directed sleep.
	maxRetryWait = 30 * time.Second
	// DefaultReplayWait bounds WaitForReplay when the caller's context has
	// no deadline of its own.
	DefaultReplayWait = 2 * time.Minute
)

// Session is a browser session.
//
// The URL and token fields are only present on responses from CreateSession
// and GetSession; ListSessions omits them. URL fields are always absolute.
type Session struct {
	// ID is the unique session id.
	ID string `json:"id"`
	// Status is the lifecycle status, e.g. "running".
	Status string `json:"status"`
	// Health reports the health of the underlying browser, e.g. "healthy".
	Health string `json:"health"`
	// StartURL is the URL the browser was opened at.
	StartURL string `json:"start_url"`
	// Width is the viewport width in pixels.
	Width int `json:"width"`
	// Height is the viewport height in pixels.
	Height int `json:"height"`
	// CreatedAt is the ISO 8601 creation timestamp.
	CreatedAt string `json:"created_at"`
	// Owner is the id of the user the session belongs to.
	Owner string `json:"owner"`
	// MemLimitBytes is the container memory limit for this session.
	MemLimitBytes int64 `json:"mem_limit_bytes,omitempty"`
	// Restarts is the number of times the in-container supervisor has
	// restarted the browser. Only present on GetSession responses; nil when
	// omitted or when the in-container status server is unreachable.
	Restarts *int `json:"restarts,omitempty"`
	// Degraded is true when the browser has restarted at least once
	// (Restarts > 0). Only meaningful on GetSession responses.
	Degraded bool `json:"degraded,omitempty"`
	// ViewerURL is the absolute URL of the live viewer with control access.
	ViewerURL string `json:"viewer_url,omitempty"`
	// WatchURL is the absolute URL of the live viewer with view-only access.
	WatchURL string `json:"watch_url,omitempty"`
	// CDPURL is the absolute Chrome DevTools Protocol endpoint for this session.
	CDPURL string `json:"cdp_url,omitempty"`
	// CDPToken authorizes CDP connections; pass it as the "x-session-token"
	// header or the "token" query parameter.
	CDPToken string `json:"cdp_token,omitempty"`
	// TokenTTLSeconds is the lifetime of the embedded tokens, in seconds.
	TokenTTLSeconds int `json:"token_ttl_seconds,omitempty"`
	// Config is the applied per-session configuration (proxy/user_agent/
	// locale/timezone/geolocation/stealth/downloads/context_id/...), echoed
	// back by the server with proxy credentials redacted. Empty when none
	// were set.
	Config map[string]any `json:"config,omitempty"`
	// Metadata holds the free-form searchable labels attached at create time.
	Metadata map[string]string `json:"metadata,omitempty"`
}

// ProxyConfig routes a session's egress through a proxy. Credentials are
// answered over CDP and never reach the browser's command line; they are
// redacted in API responses.
type ProxyConfig struct {
	// Server is "scheme://host:port" — http, https, socks5, or socks4.
	Server   string `json:"server"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	// Bypass is a comma-separated bypass list, e.g. "*.internal,localhost".
	Bypass string `json:"bypass,omitempty"`
}

// Geolocation overrides a session's reported location.
type Geolocation struct {
	// Lat is the latitude, -90..90.
	Lat float64 `json:"lat"`
	// Lon is the longitude, -180..180.
	Lon float64 `json:"lon"`
	// Accuracy is in meters; zero omits the field.
	Accuracy float64 `json:"accuracy,omitempty"`
}

// DownloadFile is one file in a downloads-enabled session's /downloads
// directory.
type DownloadFile struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
	// ModifiedMs is the last-modified time, epoch milliseconds.
	ModifiedMs int64 `json:"modified_ms"`
}

// UploadResult is returned by UploadFile.
type UploadResult struct {
	Name string `json:"name"`
	// Path is the file's location inside the session container (pass it to
	// CDP DOM.setFileInputFiles).
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
}

// Captcha challenge types accepted by SolveCaptcha.
const (
	CaptchaRecaptchaV2 = "recaptcha_v2"
	CaptchaRecaptchaV3 = "recaptcha_v3"
	CaptchaHCaptcha    = "hcaptcha"
	CaptchaTurnstile   = "turnstile"
)

// Token is a freshly minted session token.
type Token struct {
	Token      string `json:"token"`
	Scope      string `json:"scope"`
	TTLSeconds int    `json:"ttl_seconds"`
}

// CreateSessionOptions configures CreateSession. The zero value uses the
// server defaults: start_url "about:blank", 1280x800, wait=true.
type CreateSessionOptions struct {
	// StartURL is the URL to open when the browser starts. The server
	// default is "about:blank".
	StartURL string `json:"start_url,omitempty"`
	// Width is the viewport width in pixels (320-3840, server default 1280).
	Width int `json:"width,omitempty"`
	// Height is the viewport height in pixels (240-2160, server default 800).
	Height int `json:"height,omitempty"`
	// Wait controls whether the request blocks (~5s) until the browser is
	// ready. Nil omits the field and uses the server default (true); use
	// browserview.Bool(false) to return immediately.
	Wait *bool `json:"wait,omitempty"`
	// Record captures a session replay (video plus action/console/network/
	// error streams), retrievable with GetReplay after the session ends.
	// Server default is false.
	Record bool `json:"record,omitempty"`
	// MaxLifetimeSeconds auto-destroys the session after this many seconds
	// (clamped to the plan/global caps). Zero uses the server default.
	MaxLifetimeSeconds int `json:"max_lifetime_seconds,omitempty"`
	// Proxy routes the session's egress through a proxy.
	Proxy *ProxyConfig `json:"proxy,omitempty"`
	// UserAgent overrides the browser's User-Agent string.
	UserAgent string `json:"user_agent,omitempty"`
	// Locale, e.g. "en-US", sets navigator.language and Accept-Language.
	Locale string `json:"locale,omitempty"`
	// Timezone is an IANA name, e.g. "America/New_York".
	Timezone string `json:"timezone,omitempty"`
	// Geolocation overrides the session's reported location.
	Geolocation *Geolocation `json:"geolocation,omitempty"`
	// Stealth enables standard-tier anti-automation-detection tweaks.
	Stealth bool `json:"stealth,omitempty"`
	// Downloads mounts a writable, persisted /downloads directory and enables
	// ListDownloads / DownloadFile / UploadFile for this session.
	Downloads bool `json:"downloads,omitempty"`
	// ContextID names an owner-scoped persistent context: cookies from a
	// previous session with the same id are restored before you start
	// driving, and archived back on end, so a login carries across runs.
	ContextID string `json:"context_id,omitempty"`
	// SolveCaptchas records intent for captcha solving (the SolveCaptcha
	// endpoint is separately gated on a server-configured provider).
	SolveCaptchas bool `json:"solve_captchas,omitempty"`
	// Metadata attaches {key: value} string labels, searchable with
	// ListSessionsByMetadata.
	Metadata map[string]string `json:"metadata,omitempty"`
}

// ScreenshotOptions configures Screenshot. The zero value uses the server
// defaults: PNG at quality 80.
type ScreenshotOptions struct {
	// Format is "png", "jpeg", or "webp". Empty uses the server default (png).
	Format string
	// Quality is 1-100, for lossy formats. Zero uses the server default (80).
	Quality int
}

// SolveCaptchaOptions configures SolveCaptcha.
type SolveCaptchaOptions struct {
	// Type is one of the Captcha* constants.
	Type string `json:"type"`
	// Sitekey is the site key from the challenge on the page.
	Sitekey string `json:"sitekey"`
	// URL is the page URL hosting the challenge.
	URL string `json:"url"`
	// Action is the reCAPTCHA v3 action, when applicable.
	Action string `json:"action,omitempty"`
}

// Bool returns a pointer to v, for use in CreateSessionOptions.Wait.
func Bool(v bool) *bool { return &v }

// ReplayPage is one main-frame navigation in a replay's page timeline.
// Timestamps are absolute epoch milliseconds.
type ReplayPage struct {
	PageID      string `json:"page_id"`
	URL         string `json:"url"`
	StartTimeMs int64  `json:"start_time_ms"`
	EndTimeMs   int64  `json:"end_time_ms"`
}

// ReplaySegment is one raw recording segment inside a replay's merged video.
// A session normally produces a single segment; an agent restart mid-session
// produces several, concatenated in order.
type ReplaySegment struct {
	// StartMs is when the segment started recording, absolute epoch ms.
	StartMs int64 `json:"start_ms"`
	// OffsetMs is where the segment begins within the merged video.
	OffsetMs int64 `json:"offset_ms"`
	// DurationMs is the segment's length; it covers
	// [OffsetMs, OffsetMs+DurationMs) in the merged video.
	DurationMs int64 `json:"duration_ms"`
}

// ReplayVideo describes a replay's recorded video: a single seekable WebM.
type ReplayVideo struct {
	// URL is where to fetch the video; it expires at Replay.URLsExpireAtMs.
	URL string `json:"url"`
	// Format is the container format ("webm").
	Format string `json:"format"`
	// Codec is the video codec ("vp8").
	Codec string `json:"codec"`
	// StartTimeMs anchors video time zero in absolute epoch milliseconds:
	// videoSeconds = (eventTs - StartTimeMs) / 1000.
	StartTimeMs int64 `json:"start_time_ms"`
	DurationMs  int64 `json:"duration_ms"`
	SizeBytes   int64 `json:"size_bytes"`
	// Segments maps each raw recording segment into the merged video.
	Segments []ReplaySegment `json:"segments"`
}

// ReplayEventStream is one of a replay's event streams (actions, console,
// network, errors): newline-delimited JSON where every line carries an
// epoch-ms "ts" field.
type ReplayEventStream struct {
	URL string `json:"url"`
	// Count is the number of events, when known.
	Count     *int  `json:"count"`
	SizeBytes int64 `json:"size_bytes"`
}

// Replay is a session replay manifest returned by GetReplay.
//
// Status is "recording" while the session is still alive (other fields
// empty) and "ready" once artifacts are available. Artifact URLs expire at
// URLsExpireAtMs; call GetReplay again for fresh ones.
type Replay struct {
	Status      string `json:"status"`
	SessionID   string `json:"session_id"`
	StartURL    string `json:"start_url,omitempty"`
	Width       int    `json:"width,omitempty"`
	Height      int    `json:"height,omitempty"`
	StartedAtMs int64  `json:"started_at_ms,omitempty"`
	EndedAtMs   int64  `json:"ended_at_ms,omitempty"`
	// EndReason is why the session ended: "api", "idle", "lifetime", ...
	EndReason string `json:"end_reason,omitempty"`
	// Video is nil when no video was captured.
	Video     *ReplayVideo                 `json:"video,omitempty"`
	PageCount int                          `json:"page_count,omitempty"`
	Pages     []ReplayPage                 `json:"pages,omitempty"`
	Events    map[string]ReplayEventStream `json:"events,omitempty"`
	// URLsExpireAtMs is when the artifact URLs stop working, epoch ms.
	URLsExpireAtMs int64 `json:"urls_expire_at_ms,omitempty"`
}

// APIError is returned for any non-2xx API response.
type APIError struct {
	// StatusCode is the HTTP status code.
	StatusCode int
	// Message is the "detail" field of the error body, if present.
	Message string
	// RetryAfter is how long to wait before retrying, parsed from the
	// Retry-After header (integer seconds or HTTP-date) on any status;
	// zero when the header is absent.
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	return fmt.Sprintf("browserview: %s (status %d)", e.Message, e.StatusCode)
}

// Client talks to the browserview.io API. Create one with New, NewFromEnv,
// NewWithOptions, or NewWithBaseURL; it is safe for concurrent use.
type Client struct {
	apiKey     string
	baseURL    *url.URL
	httpClient *http.Client
	maxRetries int

	// sleep pauses between retries; overridable in tests.
	sleep func(ctx context.Context, d time.Duration) error
}

// Option configures a Client created with NewWithOptions.
type Option func(*clientConfig) error

type clientConfig struct {
	baseURL    string
	timeout    time.Duration
	maxRetries int
	httpClient *http.Client
}

// WithBaseURL targets a custom API origin instead of DefaultBaseURL (or the
// BROWSERVIEW_BASE_URL environment variable, which it overrides).
func WithBaseURL(baseURL string) Option {
	return func(cfg *clientConfig) error {
		if baseURL == "" {
			return errors.New("browserview: base URL must not be empty")
		}
		cfg.baseURL = baseURL
		return nil
	}
}

// WithTimeout sets the per-request timeout (default DefaultTimeout, 90s).
// Zero or negative disables the timeout.
func WithTimeout(d time.Duration) Option {
	return func(cfg *clientConfig) error {
		if d < 0 {
			d = 0
		}
		cfg.timeout = d
		return nil
	}
}

// WithMaxRetries sets how many times a request is retried after the initial
// attempt (default DefaultMaxRetries, 3). 0 disables retries.
func WithMaxRetries(n int) Option {
	return func(cfg *clientConfig) error {
		if n < 0 {
			return fmt.Errorf("browserview: max retries must be >= 0, got %d", n)
		}
		cfg.maxRetries = n
		return nil
	}
}

// WithHTTPClient supplies a custom *http.Client. Its Timeout is left
// untouched unless WithTimeout is also given.
func WithHTTPClient(hc *http.Client) Option {
	return func(cfg *clientConfig) error {
		if hc == nil {
			return errors.New("browserview: http client must not be nil")
		}
		cfg.httpClient = hc
		return nil
	}
}

// New returns a Client for the production API (or the origin named by the
// BROWSERVIEW_BASE_URL environment variable, when set).
func New(apiKey string) *Client {
	base := DefaultBaseURL
	if env := os.Getenv(EnvBaseURL); env != "" {
		base = env
	}
	return NewWithBaseURL(apiKey, base)
}

// NewFromEnv returns a Client configured from the environment: the API key
// from BROWSERVIEW_API_KEY (required) and the base URL from
// BROWSERVIEW_BASE_URL (optional, default DefaultBaseURL). Additional
// options may be supplied and take precedence over the environment.
func NewFromEnv(opts ...Option) (*Client, error) {
	apiKey := os.Getenv(EnvAPIKey)
	if apiKey == "" {
		return nil, fmt.Errorf("browserview: %s is not set", EnvAPIKey)
	}
	return NewWithOptions(apiKey, opts...)
}

// NewWithOptions returns a Client configured with functional options. With
// no options it behaves like New: DefaultBaseURL (or BROWSERVIEW_BASE_URL),
// a 90s timeout, and 3 retries.
func NewWithOptions(apiKey string, opts ...Option) (*Client, error) {
	cfg := clientConfig{
		baseURL:    DefaultBaseURL,
		timeout:    DefaultTimeout,
		maxRetries: DefaultMaxRetries,
	}
	if env := os.Getenv(EnvBaseURL); env != "" {
		cfg.baseURL = env
	}
	explicitTimeout := false
	for _, opt := range opts {
		before := cfg.timeout
		if err := opt(&cfg); err != nil {
			return nil, err
		}
		if cfg.timeout != before {
			explicitTimeout = true
		}
	}

	u, err := url.Parse(cfg.baseURL)
	if err != nil {
		return nil, fmt.Errorf("browserview: invalid base URL %q: %w", cfg.baseURL, err)
	}

	hc := cfg.httpClient
	if hc == nil {
		hc = &http.Client{Timeout: cfg.timeout}
	} else if explicitTimeout {
		clone := *hc
		clone.Timeout = cfg.timeout
		hc = &clone
	}

	return &Client{
		apiKey:     apiKey,
		baseURL:    u,
		httpClient: hc,
		maxRetries: cfg.maxRetries,
		sleep:      sleepCtx,
	}, nil
}

// NewWithBaseURL returns a Client that targets a custom API origin.
// It panics if baseURL is not a valid URL; prefer NewWithOptions with
// WithBaseURL for an error-returning variant.
func NewWithBaseURL(apiKey, baseURL string) *Client {
	u, err := url.Parse(baseURL)
	if err != nil {
		panic(fmt.Sprintf("browserview: invalid base URL %q: %v", baseURL, err))
	}
	return &Client{
		apiKey:     apiKey,
		baseURL:    u,
		httpClient: &http.Client{Timeout: DefaultTimeout},
		maxRetries: DefaultMaxRetries,
		sleep:      sleepCtx,
	}
}

// CreateSession creates a new browser session. Unless opts.Wait is set to
// false, the call blocks until the browser is ready, typically ~5 seconds.
//
// The server responds 429 (with Retry-After) when the shard or owner is at
// its session limit; the client retries those automatically (the session is
// not created on a 429/503 response, so the retry is safe).
func (c *Client) CreateSession(ctx context.Context, opts CreateSessionOptions) (*Session, error) {
	var session Session
	if err := c.do(ctx, http.MethodPost, "/sessions", opts, &session); err != nil {
		return nil, err
	}
	c.resolveURLs(&session)
	return &session, nil
}

// ListSessions lists your sessions. URL and token fields are omitted; use
// GetSession for fresh ones.
func (c *Client) ListSessions(ctx context.Context) ([]Session, error) {
	var sessions []Session
	if err := c.do(ctx, http.MethodGet, "/sessions", nil, &sessions); err != nil {
		return nil, err
	}
	for i := range sessions {
		c.resolveURLs(&sessions[i])
	}
	return sessions, nil
}

// ListSessionsByMetadata lists sessions whose metadata labels contain every
// given {key: value} pair. URL and token fields are omitted; use GetSession
// for fresh ones.
func (c *Client) ListSessionsByMetadata(ctx context.Context, metadata map[string]string) ([]Session, error) {
	path := "/sessions"
	if len(metadata) > 0 {
		params := url.Values{}
		for key, value := range metadata {
			params.Set("metadata."+key, value)
		}
		path += "?" + params.Encode()
	}
	var sessions []Session
	if err := c.do(ctx, http.MethodGet, path, nil, &sessions); err != nil {
		return nil, err
	}
	for i := range sessions {
		c.resolveURLs(&sessions[i])
	}
	return sessions, nil
}

// Screenshot captures a current-viewport image of a live session and returns
// the raw image bytes.
func (c *Client) Screenshot(ctx context.Context, id string, opts ScreenshotOptions) ([]byte, error) {
	params := url.Values{}
	if opts.Format != "" {
		params.Set("format", opts.Format)
	}
	if opts.Quality != 0 {
		params.Set("quality", strconv.Itoa(opts.Quality))
	}
	path := "/sessions/" + url.PathEscape(id) + "/screenshot"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	return c.doBytes(ctx, http.MethodGet, path, "", nil, nil, true)
}

// ListDownloads lists a downloads-enabled session's files (works during and
// after the session).
func (c *Client) ListDownloads(ctx context.Context, id string) ([]DownloadFile, error) {
	var out struct {
		Files []DownloadFile `json:"files"`
	}
	if err := c.do(ctx, http.MethodGet, "/sessions/"+url.PathEscape(id)+"/downloads", nil, &out); err != nil {
		return nil, err
	}
	return out.Files, nil
}

// DownloadFile fetches one downloaded file's contents (following the
// presigned-S3 redirect the server issues after the session ends).
func (c *Client) DownloadFile(ctx context.Context, id, name string) ([]byte, error) {
	path := "/sessions/" + url.PathEscape(id) + "/downloads/" + url.PathEscape(name)
	return c.doBytes(ctx, http.MethodGet, path, "", nil, nil, true)
}

// UploadFile places a file into a live session's /downloads mount for
// input[type=file] flows — attach it via CDP DOM.setFileInputFiles with the
// returned container Path. The session must have been created with
// Downloads: true.
func (c *Client) UploadFile(ctx context.Context, id, filename string, content []byte) (*UploadResult, error) {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		return nil, fmt.Errorf("browserview: build upload: %w", err)
	}
	if _, err := part.Write(content); err != nil {
		return nil, fmt.Errorf("browserview: build upload: %w", err)
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("browserview: build upload: %w", err)
	}
	var result UploadResult
	path := "/sessions/" + url.PathEscape(id) + "/files"
	if _, err := c.doBytes(ctx, http.MethodPost, path, writer.FormDataContentType(), buf.Bytes(), &result, false); err != nil {
		return nil, err
	}
	return &result, nil
}

// SolveCaptcha relays a captcha challenge to the server-configured solver
// and returns the solution token for your agent to inject. The server
// responds 501 when solving is not enabled on the host.
func (c *Client) SolveCaptcha(ctx context.Context, id string, opts SolveCaptchaOptions) (string, error) {
	var out struct {
		Token string `json:"token"`
	}
	if err := c.do(ctx, http.MethodPost, "/sessions/"+url.PathEscape(id)+"/captcha/solve", opts, &out); err != nil {
		return "", err
	}
	if out.Token == "" {
		return "", errors.New("browserview: unexpected captcha response from the server (missing 'token' field)")
	}
	return out.Token, nil
}

// GetSession fetches a session by id, including fresh URLs and tokens plus
// the Restarts and Degraded health fields.
func (c *Client) GetSession(ctx context.Context, id string) (*Session, error) {
	var session Session
	if err := c.do(ctx, http.MethodGet, "/sessions/"+url.PathEscape(id), nil, &session); err != nil {
		return nil, err
	}
	c.resolveURLs(&session)
	return &session, nil
}

// DestroySession destroys a session and its browser.
func (c *Client) DestroySession(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/sessions/"+url.PathEscape(id), nil, nil)
}

// MintToken mints a new token for a session. Scope is one of ScopeView,
// ScopeControl, or ScopeCDP; ttl is rounded down to whole seconds and must
// be between 1 second and 7 days (604800s) inclusive.
func (c *Client) MintToken(ctx context.Context, id, scope string, ttl time.Duration) (*Token, error) {
	body := struct {
		Scope      string `json:"scope"`
		TTLSeconds int    `json:"ttl_seconds"`
	}{Scope: scope, TTLSeconds: int(ttl / time.Second)}

	var token Token
	if err := c.do(ctx, http.MethodPost, "/sessions/"+url.PathEscape(id)+"/tokens", body, &token); err != nil {
		return nil, err
	}
	return &token, nil
}

// GetReplay fetches the replay manifest for a recorded session.
//
// It works after the session is destroyed. While the session is still alive
// it returns a Replay with Status "recording"; while the recording is being
// finalized (typically <30s after the session ends) the server responds 404
// — use WaitForReplay to poll through that window.
func (c *Client) GetReplay(ctx context.Context, id string) (*Replay, error) {
	var replay Replay
	if err := c.do(ctx, http.MethodGet, "/sessions/"+url.PathEscape(id)+"/replay", nil, &replay); err != nil {
		return nil, err
	}
	c.resolveReplayURLs(&replay)
	return &replay, nil
}

// WaitForReplay polls until a session's replay is ready and returns its
// manifest, checking every interval until the context is done. It polls
// through the live ("recording") and finalizing (404) states; any other API
// error is returned immediately, as is a 404 reporting that the live session
// is not being recorded (one created without Record: true). Pass
// interval <= 0 for the 5s default.
//
// When ctx has no deadline the wait is bounded at DefaultReplayWait
// (2 minutes); a caller-supplied deadline is respected as-is:
//
//	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
//	defer cancel()
//	replay, err := client.WaitForReplay(ctx, session.ID, 0)
func (c *Client) WaitForReplay(ctx context.Context, id string, interval time.Duration) (*Replay, error) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultReplayWait)
		defer cancel()
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		replay, err := c.GetReplay(ctx, id)
		if err == nil && replay.Status == "ready" {
			return replay, nil
		}
		if err != nil {
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNotFound {
				return nil, err
			}
			if strings.Contains(apiErr.Message, "not being recorded") {
				return nil, fmt.Errorf("browserview: session %s was not created with record: true: %w", id, err)
			}
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("browserview: replay for session %s not ready: %w", id, ctx.Err())
		case <-ticker.C:
		}
	}
}

// resolveReplayURLs absolutizes relative artifact URLs (returned by
// local-storage shards); presigned S3 URLs are already absolute.
func (c *Client) resolveReplayURLs(r *Replay) {
	resolve := func(value string) string {
		if value == "" {
			return value
		}
		if ref, err := url.Parse(value); err == nil {
			return c.baseURL.ResolveReference(ref).String()
		}
		return value
	}
	if r.Video != nil {
		r.Video.URL = resolve(r.Video.URL)
	}
	for name, stream := range r.Events {
		stream.URL = resolve(stream.URL)
		r.Events[name] = stream
	}
}

// do performs one JSON API call with automatic retries. 429 and 503
// responses are retried for every method (session creation is not committed
// on those); transport errors are retried only for idempotent GET/DELETE.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var encoded []byte
	contentType := ""
	if in != nil {
		var err error
		encoded, err = json.Marshal(in)
		if err != nil {
			return fmt.Errorf("browserview: encode request: %w", err)
		}
		contentType = "application/json"
	}
	_, err := c.doBytes(ctx, method, path, contentType, encoded, out, false)
	return err
}

// doBytes is the byte-level core of do: it sends encoded (with contentType,
// when non-empty) and either decodes the response JSON into out or, when raw
// is true, returns the response body verbatim (screenshots, file downloads).
// Retry semantics match do.
func (c *Client) doBytes(ctx context.Context, method, path, contentType string, encoded []byte, out any, raw bool) ([]byte, error) {
	idempotent := method == http.MethodGet || method == http.MethodDelete

	// JoinPath escapes a "?" inside its argument, so split any query string
	// off and re-attach it verbatim.
	rawQuery := ""
	if idx := strings.IndexByte(path, '?'); idx >= 0 {
		path, rawQuery = path[:idx], path[idx+1:]
	}
	target := c.baseURL.JoinPath(path)
	target.RawQuery = rawQuery

	for attempt := 0; ; attempt++ {
		var body io.Reader
		if encoded != nil {
			body = bytes.NewReader(encoded)
		}
		req, err := http.NewRequestWithContext(ctx, method, target.String(), body)
		if err != nil {
			return nil, fmt.Errorf("browserview: build request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "browserview-go/"+Version)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			if ctx.Err() == nil && idempotent && attempt < c.maxRetries {
				if serr := c.sleep(ctx, backoff(attempt)); serr != nil {
					return nil, fmt.Errorf("browserview: retry aborted: %w", serr)
				}
				continue
			}
			return nil, fmt.Errorf("browserview: %s %s: %w", method, path, err)
		}

		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			apiErr := newAPIError(resp)
			resp.Body.Close()
			if retryableStatus(resp.StatusCode) && attempt < c.maxRetries {
				wait := backoff(attempt)
				if apiErr.RetryAfter > 0 {
					wait = min(apiErr.RetryAfter, maxRetryWait)
				}
				if serr := c.sleep(ctx, wait); serr != nil {
					return nil, fmt.Errorf("browserview: retry aborted: %w", serr)
				}
				continue
			}
			return nil, apiErr
		}

		if raw {
			data, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				return nil, fmt.Errorf("browserview: read response: %w", err)
			}
			return data, nil
		}
		if out == nil || resp.StatusCode == http.StatusNoContent {
			resp.Body.Close()
			return nil, nil
		}
		err = json.NewDecoder(resp.Body).Decode(out)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("browserview: decode response: %w", err)
		}
		return nil, nil
	}
}

func retryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code == http.StatusServiceUnavailable
}

// backoff returns the exponential retry delay: 1s, 2s, 4s, ... capped at
// maxRetryWait.
func backoff(attempt int) time.Duration {
	if attempt > 5 {
		attempt = 5
	}
	return min(time.Second<<attempt, maxRetryWait)
}

// sleepCtx sleeps for d or until ctx is done, whichever comes first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func newAPIError(resp *http.Response) *APIError {
	apiErr := &APIError{
		StatusCode: resp.StatusCode,
		Message:    fmt.Sprintf("HTTP %d", resp.StatusCode),
	}
	var payload struct {
		Detail json.RawMessage `json:"detail"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err == nil && len(payload.Detail) > 0 {
		var s string
		if json.Unmarshal(payload.Detail, &s) == nil {
			apiErr.Message = s
		} else {
			// detail may occasionally be structured; stringify defensively.
			apiErr.Message = string(payload.Detail)
		}
	}
	if d, ok := parseRetryAfter(resp.Header.Get("Retry-After")); ok {
		apiErr.RetryAfter = d
	}
	return apiErr
}

// parseRetryAfter parses a Retry-After header as either integer seconds or
// an HTTP-date. Negative values clamp to zero.
func parseRetryAfter(header string) (time.Duration, bool) {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0, false
	}
	if seconds, err := strconv.Atoi(header); err == nil {
		if seconds < 0 {
			seconds = 0
		}
		return time.Duration(seconds) * time.Second, true
	}
	if t, err := http.ParseTime(header); err == nil {
		d := time.Until(t)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}

// resolveURLs rewrites the relative viewer/watch/CDP paths returned by the
// API into absolute URLs against the client's base URL.
func (c *Client) resolveURLs(s *Session) {
	for _, field := range []*string{&s.ViewerURL, &s.WatchURL, &s.CDPURL} {
		if *field == "" {
			continue
		}
		if ref, err := url.Parse(*field); err == nil {
			*field = c.baseURL.ResolveReference(ref).String()
		}
	}
}
