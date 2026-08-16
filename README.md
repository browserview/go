# BrowserView Go SDK

Go client for the [browserview.io](https://browserview.io) API. browserview.io runs disposable cloud Chromium sessions that humans can watch and control in a live viewer while agents drive the same browser over the Chrome DevTools Protocol.

Standard library only. Requires Go 1.21+. SDK version 0.3.1.

## Install

```sh
go get github.com/browserview/go
```

## Configuration

- Base URL: defaults to `https://sessions.browserview.io`. Override with the `BROWSERVIEW_BASE_URL` environment variable or `WithBaseURL`; an explicit option wins over the environment variable.
- API key: pass it to `New`, or set `BROWSERVIEW_API_KEY` and use `NewFromEnv`.
- API keys are minted in the [browserview.io console](https://browserview.io) and look like `bv_live_` + 40 hex chars; a key sees only its own sessions. The SDK does not validate the key format.
- The SDK authenticates with `Authorization: Bearer <key>`; the API also accepts `x-api-key: <key>`.

```go
client := browserview.New(apiKey)

// From the environment (BROWSERVIEW_API_KEY required, BROWSERVIEW_BASE_URL optional):
client, err := browserview.NewFromEnv()

// With options:
client, err := browserview.NewWithOptions(apiKey,
	browserview.WithBaseURL("https://sessions.browserview.io"),
	browserview.WithTimeout(2*time.Minute),  // default 90s
	browserview.WithMaxRetries(5),           // default 3; 0 disables retries
)
```

## Quickstart

```go
package main

import (
	"context"
	"fmt"
	"log"

	browserview "github.com/browserview/go"
)

func main() {
	ctx := context.Background()
	client, err := browserview.NewFromEnv()
	if err != nil {
		log.Fatal(err)
	}

	session, err := client.CreateSession(ctx, browserview.CreateSessionOptions{
		StartURL: "https://example.com",
	})
	if err != nil {
		log.Fatal(err)
	}
	defer client.DestroySession(ctx, session.ID)

	fmt.Println(session.ViewerURL) // live viewer, control access
	fmt.Println(session.WatchURL)  // live viewer, view-only
}
```

Server defaults for `CreateSessionOptions`: `StartURL` `"about:blank"`, 1280x800 (width 320-3840, height 240-2160), and `Wait` true — the call blocks until Chromium's CDP endpoint answers, typically ~5 seconds. Use `Wait: browserview.Bool(false)` to return immediately. The SDK only sends fields you set.

Other operations:

```go
sessions, err := client.ListSessions(ctx)         // no URLs/tokens in list responses
session, err = client.GetSession(ctx, session.ID) // fresh URLs/tokens + Restarts/Degraded
err = client.DestroySession(ctx, session.ID)      // 204, no body

token, err := client.MintToken(ctx, session.ID, browserview.ScopeView, time.Hour)
// scopes: ScopeView, ScopeControl, ScopeCDP; ttl bounds: 1s .. 7 days (604800s)
```

Session fields of note:

- `MemLimitBytes` — the container memory limit.
- `Restarts` (`*int`, GetSession only) — how many times the browser was restarted in place; `nil` when the in-container status server is unreachable.
- `Degraded` (GetSession only) — true when `Restarts > 0`.
- `ViewerURL`, `WatchURL`, `CDPURL` — returned relative by the API; the SDK resolves them to absolute URLs against the client's base URL.

## Drive the session over CDP

`session.CDPURL` is a standard Chrome DevTools Protocol endpoint; authenticate with the `x-session-token` header (or `?token=` query parameter) set to `session.CDPToken`. Any CDP client works — for example, from Playwright (Node):

```ts
import { chromium } from "playwright";

const browser = await chromium.connectOverCDP(session.CDPURL, {
  headers: { "x-session-token": session.CDPToken },
});
```

## Per-session configuration

All create-time knobs live on `CreateSessionOptions`; zero values are omitted:

```go
session, err := client.CreateSession(ctx, browserview.CreateSessionOptions{
	StartURL:  "about:blank",
	Stealth:   true, // anti-automation-detection tweaks
	UserAgent: "MyAgent/1.0",
	Locale:    "en-GB",
	Timezone:  "America/New_York",
	Geolocation: &browserview.Geolocation{Lat: 40.7, Lon: -74.0},
	Proxy: &browserview.ProxyConfig{
		Server: "http://proxy:8080", Username: "u", Password: "p",
	},
	ContextID:          "my-login", // cookies persist across runs under this id
	Downloads:          true,       // persisted /downloads mount + file APIs
	Metadata:           map[string]string{"job": "crawl-1"},
	MaxLifetimeSeconds: 600,
})
```

Overrides apply once the session is healthy — the very first `StartURL` navigation can go out with stock headers, so navigate over CDP from `about:blank` when the first request matters. The applied config (secrets redacted) is echoed on `session.Config`; labels on `session.Metadata`.

```go
sessions, err := client.ListSessionsByMetadata(ctx, map[string]string{"job": "crawl-1"})

image, err := client.Screenshot(ctx, session.ID, browserview.ScreenshotOptions{Format: "png"})

files, err := client.ListDownloads(ctx, session.ID)               // works after the session ends
data, err := client.DownloadFile(ctx, session.ID, files[0].Name)
result, err := client.UploadFile(ctx, session.ID, "input.csv", csvBytes)

token, err := client.SolveCaptcha(ctx, session.ID, browserview.SolveCaptchaOptions{
	Type: browserview.CaptchaTurnstile, Sitekey: "0x...", URL: "https://target.example",
}) // 501 APIError unless the host has a solver
```

## Session replay

Create the session with `Record: true` and BrowserView captures everything server-side — a video of the display plus structured streams of actions, console output, network requests, and errors:

```go
session, err := client.CreateSession(ctx, browserview.CreateSessionOptions{
	StartURL: "https://example.com",
	Record:   true,
})
// ... drive the session ...
_ = client.DestroySession(ctx, session.ID)

// Ready seconds after the session ends. Without a context deadline the wait
// is bounded at 2 minutes (DefaultReplayWait); a caller-supplied deadline is
// respected as-is.
replay, err := client.WaitForReplay(ctx, session.ID, 0) // 0 = 5s poll interval
fmt.Println(replay.Video.URL)              // seekable WebM
fmt.Println(replay.Pages)                  // main-frame navigation timeline
fmt.Println(replay.Events["console"].URL)  // JSONL: {"ts": ..., "level": ...}
```

`GetReplay` fetches the manifest without polling: it returns `Status: "recording"` while the session is alive and a 404 `APIError` while the recording finalizes. If the live session was created without `Record: true`, `WaitForReplay` fails immediately instead of polling. Every event line carries an absolute epoch-ms `ts`; align it with the video via `(ts - replay.Video.StartTimeMs) / 1000` seconds. Artifact URLs expire at `URLsExpireAtMs` — call `GetReplay` again for fresh ones. `replay.Video.Segments` maps each raw recording segment (several after a mid-session agent restart) into the merged video.

## Errors and retries

Non-2xx responses return an `*APIError` with `StatusCode`, `Message` (the API's `detail` field), and `RetryAfter` parsed from the `Retry-After` header (integer seconds or HTTP-date) on any status:

```go
_, err := client.GetSession(ctx, "nope")
var apiErr *browserview.APIError
if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
	// unknown or unowned session (tenants get 404, never 403)
}
```

Status codes you will see:

- `401` — invalid or missing key (repeated failures escalate to per-IP 429).
- `404` — unknown session, or one your key cannot see.
- `429` + `Retry-After: 30` — session/capacity limits on create (session limits, memory, ports).
- `429` + `Retry-After: 60` — request/create rate limits (per IP or per owner) and repeated auth failures.
- `502` — session create/backend failure.
- `503` + `Retry-After: 10` — auth temporarily unavailable; retryable.
- `503` + `Retry-After: 60` — recording backend unavailable (create with `Record: true`).

The client retries automatically: `429` and `503` responses are retried for every method (a `429`/`503` on `POST /sessions` means the session was **not** created, so the retry is safe), and transport errors are retried for idempotent GET/DELETE. Waits honor `Retry-After` (capped at 30s per wait), otherwise back off 1s/2s/4s. Default 3 retries; configure with `WithMaxRetries` (0 disables). Sleeps respect context cancellation. The default per-request timeout is 90s (`WithTimeout` to change), sized for create-with-wait (which can block up to ~60s server-side).

## Health checks

`GET /healthz` and `GET /readyz` are unauthenticated and return `{"status":"ok"}` — useful for probes; no SDK wrapper is provided.
