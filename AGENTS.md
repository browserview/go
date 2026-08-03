# AGENTS.md — github.com/browserview/go

Go client for the browserview.io API. Standard library only; package name is `browserview`.

## Commands

- Test: `go test ./...` (offline via `httptest.NewServer`; the package `sleep` var is stubbed in tests)
- Vet: `go vet ./...`
- No publish step: pushing a `v*` tag to github.com/browserview/go makes the version available via the Go module proxy.

## Layout

- `browserview.go` — entire SDK: `Client`, options, typed structs (`Session`, `Token`, `Replay`), `APIError`.
- `browserview_test.go` — all tests.

## Conventions

- Go 1.21+ (uses builtin `min`). No dependencies — keep `go.sum` nonexistent.
- Struct JSON tags mirror the API's snake_case. `omitempty` on create options: only fields the caller set are sent (hence `Wait *bool` + the `Bool()` helper).
- 429/503 retried for every method; transport errors retried only for GET/DELETE. `Retry-After` supports integer seconds and HTTP-dates, capped at 30s per wait; sleeps respect context cancellation.
- `Version` constant feeds the `User-Agent: browserview-go/<Version>` header — bump it with releases.
- The API contract source of truth is the browserview.io orchestrator; docs: https://browserview.io/docs/api and https://browserview.io/llms-full.txt

## Release

1. Bump `Version` in `browserview.go` and the version line in `README.md`.
2. `go test ./... && go vet ./...`
3. Commit, tag `v<version>`, push with tags. Verify with `GOPROXY=https://proxy.golang.org go list -m github.com/browserview/go@v<version>`.
