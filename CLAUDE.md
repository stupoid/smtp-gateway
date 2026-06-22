# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commit convention

Use Conventional Commits: `<type>(<scope>): <imperative summary>`. Atomic commits — one logical change per commit. Verify before committing: run tests, lint, and `git diff --check`.

## Commands

```bash
# Build all packages
go build ./...

# Run all tests
go test -count=1 ./...

# Run a single test
go test -count=1 -run TestSMTPStartTLS ./...

# Run tests with coverage
go test -count=1 -coverprofile=coverage.out ./...
go tool cover -func=coverage.out | sort -t: -k3 -n

# Lint (golangci-lint v2 config at .golangci.yml)
golangci-lint run ./...

# Vet
go vet ./...
```

## Architecture

### Per-connection model: two goroutines

Each accepted connection spawns two goroutines inside `handleConn` (`smtp.go`):

1. **Reader goroutine** (`readCommands`) — reads lines from `conn.r` (`*bufio.Reader`), parses SMTP verbs, sends `smtpCmd{verb, args}` onto a buffered `events` channel (depth 32). This enables RFC 2920 PIPELINING: the reader can read ahead while the worker processes.

2. **Worker loop** — receives from `events` and dispatches to `handleHelo`, `handleEhlo`, `handleMail`, `handleRcpt`, `handleData`, `handleStartTLS`, etc. Responses are written via `conn.write()` which uses a mutex-protected `*bufio.Writer`.

### Connection takeover: DATA and STARTTLS

For DATA and STARTTLS, the reader goroutine **pauses** after sending the command to `events`. It blocks on `resumeCh` until the worker signals completion:

- **DATA**: The worker reads the dot-stuffed body directly from `conn.r` (bypassing the reader), dot-unstuffs it, calls the handler, then signals `resumeCh`.
- **STARTTLS**: The worker performs the TLS handshake on `conn.netConn`, replaces `conn.r`/`conn.w` with TLS-wrapped readers/writers, then signals `resumeCh`. The reader resumes on the new TLS connection.

This pause is critical — without it, the reader goroutine and the TLS/data-read would race for bytes on the same `net.Conn`.

### Handler contract

The `Handler` interface (`gateway.go`) has four callbacks: `Hello`, `MailFrom`, `RcptTo`, `Data`. Key invariants:

- Callbacks for a **single connection are serial** (called from the worker loop sequentially).
- The **same Handler instance** handles all connections concurrently — make it goroutine-safe if it holds mutable state.
- `RcptTo` is called once per recipient. `tx.Rcpts` accumulates all presented; `tx.Accepted` tracks the accepted subset.
- `Data` is only called if ≥1 recipient was accepted. Return non-250 to bounce.
- Return `nil` from any callback and the server substitutes a 503 "Bad sequence" response.
- After RSET or successful DATA, `tx` is replaced with a fresh `Tx` via `newTx()`.

### Transaction state (`Tx`)

`Tx` (`gateway.go`) accumulates across phases and is reset on RSET or successful DATA. Fields:
- `Helo` — client's HELO/EHLO domain
- `MailFrom` — envelope sender (empty string = null sender `<>`)
- `Params` — MAIL FROM parameters (SIZE, BODY, etc.)
- `Rcpts` — all presented recipients (accepted + rejected)
- `Accepted` / `Rejected` — split by handler responses
- `TLS` — non-nil after STARTTLS upgrade

### SMTP protocol helpers (`smtp.go`)

- `parseSMTPCommand(line)` — splits "VERB args\r\n" into verb and args
- `parseMailFrom(args)` — extracts reverse-path and key=value parameters from MAIL FROM
- `parseRcptTo(args)` — extracts forward-path from RCPT TO (best-effort fallback for bare addresses)
- `readDotUnstuffed(r, maxSize, conn, readTimeout)` — reads dot-stuffed body, returns unstuffed bytes
- `readLine(r, readTimeout, conn)` — reads a line handling both `\r\n` and bare `\n`

### Logging

The `Logger` interface (`gateway.go`) has three methods: `Debug`, `Info`, `Error`. The `args ...any` variadic follows `slog` conventions (key=value pairs). Use `smtpgateway.Slog(slog.Default())` for the built-in slog adapter, `nil` to discard all logs. Connection open/close events are Debug-level; SMTP command/body events are Info-level; errors are Error-level.

### Postcat format (`internal/postcat/`)

Flat files with envelope records (`S`, `R`, `T` lines) followed by a blank line and raw RFC 5322 body. Written by `postcat.Write()` when `Server.PostcatDir` is set. Parsed by `postcat.Parse()`. The `cmd/postcat` and `cmd/postcat-replay` tools provide CLI access. `cmd/verify-postcat` does batch verification.

## Test patterns

Tests live in two files:
- **`smtp_test.go`** — unit tests for parsers, helpers, concurrency, postcat round-trips. Uses `net.Pipe()` for `readDotUnstuffed` tests.
- **`smtp_rfc_test.go`** — integration tests against real TCP servers. Provides helpers: `dialServer(t)` starts an accept-all server on a random port, `sendAndExpect(t, conn, scanner, cmd, prefix)` sends a command and checks the response prefix, `readMultiline(t, scanner)` drains multi-line EHLO responses.

The `testCert(t)` helper in `smtp_rfc_test.go` generates self-signed TLS certificates programmatically (no external cert files needed).

The `acceptAllHandler` type (in `smtp_test.go`) is a trivial Handler that accepts everything — reused across tests.

When adding protocol-level tests, prefer adding to `smtp_rfc_test.go` using the TCP helpers. When adding parser/helper unit tests, add to `smtp_test.go`.
