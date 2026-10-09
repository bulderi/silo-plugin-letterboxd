# Contributing to the Letterboxd Watch Provider Plugin

The [Silo contribution guide](https://github.com/Silo-Server/.github/blob/main/CONTRIBUTING.md)
covers project-wide coordination, focused changes, evidence, AI disclosure, and
pull request expectations. Those requirements apply here; this guide adds the
plugin-specific workflow.

## Before you start

Open an [issue](https://github.com/bulderi/silo-plugin-letterboxd/issues)
before changing authentication, snapshot completeness, idempotency, supported
state, configuration, or the advertised capability. This repository owns the
Letterboxd adapter; plugin contracts belong in
[`silo-plugin-sdk`](https://github.com/Silo-Server/silo-plugin-sdk), while host
watch-sync orchestration belongs in
[`silo-server`](https://github.com/Silo-Server/silo-server).

Letterboxd has no public API for this, so the plugin reads and writes the
website. A change that alters what the plugin sends to Letterboxd, or how it
signs in, needs a live check with `cmd/lbcheck` as well as the tests.

## Development setup

Use the Go version declared in `go.mod`. A local `go.work` may point at a sibling
SDK checkout while developing both repositories, but committed code and CI must
resolve the tagged SDK dependency with `GOWORK=off`. Never commit real
deployment URLs, passwords, cookies, CSRF tokens, captured Letterboxd pages, or a
local filesystem `replace` directive. Test fixtures use invented films and
members.

## Validate your change

```sh
GOWORK=off go test -race ./...
GOWORK=off go vet ./...
GOWORK=off go build ./...
GOWORK=off go run . manifest >/dev/null
gofmt -l .
golangci-lint run ./...
```

The manifest command must exit successfully, and `gofmt -l .` and
`golangci-lint` must report nothing. Add focused coverage for sign-in, session
renewal, snapshot completeness, identity mapping, event idempotency, and
upstream error handling when those behaviors change. A read that could return a
partial watchlist needs a test showing it returns a fault instead, because Silo
treats a film missing from a complete snapshot as removed.

## Open the pull request

Use a Conventional Commit title, explain any sync, privacy, or retry risk, and
paste the actual validation results. Read the
[AI-assisted contribution policy](https://github.com/Silo-Server/silo-server/blob/main/docs/ai-contributions.md)
and include its disclosure block.
