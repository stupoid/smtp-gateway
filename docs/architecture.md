# Architecture

## Package layout

```
gateway.go        Public API — Server, Handler, Tx, Logger, Response
smtp.go           Protocol implementation — command parser, state machine,
                  two-goroutine per-connection model, connection takeover
internal/postcat/ On-disk persistence — write, parse, format
cmd/test-server/  Reference SMTP server for development and e2e tests
cmd/postcat/      CLI: inspect postcat files
cmd/postcat-replay/ CLI: replay postcat files through an SMTP server
cmd/verify-postcat/ CLI: batch-validate a postcat directory
examples/s3/      Example handler that compresses and stores to S3
```

## Per-connection goroutine model

Each accepted connection spawns two goroutines:

```
  net.Conn
     │
     ├── Reader goroutine (readCommands)
     │   - bufio.Reader.ReadString('\n') loop
     │   - parses SMTP verbs
     │   - sends smtpCmd{verb, args} to events channel (depth 32)
     │   - pauses on resumeCh during DATA/BDAT/STARTTLS
     │
     └── Worker loop (handleConn body)
         - receives from events
         - dispatches to handleHelo / handleEhlo / handleMail / ...
         - serial handler callbacks per connection
         - writes responses via mutex-protected bufio.Writer
```

The `events` channel (buffered, depth 32) enables RFC 2920 PIPELINING:
the reader can enqueue several commands while the worker is processing
an earlier one.  The worker drains them one at a time.

## Connection takeover

For DATA, BDAT, and STARTTLS, the reader goroutine **pauses** after
sending the command to `events`.  It blocks on `resumeCh` (capacity 1)
until the worker signals completion.  This prevents the two goroutines
from racing for bytes on the same `net.Conn`.

### DATA takeover

```
Reader                          Worker
  │                               │
  ├─ send DATA to events ────────►│
  │  (blocks on resumeCh)         │
  │                               ├─ receive DATA
  │                               ├─ readDotUnstuffed(conn.r, ...)
  │                               │    reads body directly from conn.r
  │                               │    dot-unstuffs it
  │                               ├─ call handler.Data(ctx, tx, body)
  │                               ├─ optional postcat.Write(...)
  │                               ├─ send 250 response
  │                               ├─ resumeCh <- struct{}{}
  ◄─ unblocks                     │
  │                               │
  └─ resumes normal reading       └─ loops back to events select
```

### BDAT takeover

```
Reader                          Worker
  │                               │
  ├─ send BDAT to events ────────►│
  │  (blocks on resumeCh)         │
  │                               ├─ readNBytes(conn.r, size, ...)
  │                               │    reads exactly size raw bytes
  │                               ├─ append to conn.bodyBuf
  │                               ├─ if !LAST:
  │                               │    send 250, resumeCh <- struct{}
  │                               │    (reader unblocks, sends next BDAT)
  │                               ├─ if LAST:
  │                               │    call handler.Data(...)
  │                               │    resumeCh <- struct{}
  │                               │    (reader resumes normal reading)
  ◄─ unblocks (per chunk or LAST) │
  │                               │
```

If any BDAT chunk is rejected (bad phase, no recipients, size exceeded),
the chunk bytes are drained via `io.CopyN(io.Discard, ...)` to keep the
protocol stream synchronised without allocating the full chunk.

### STARTTLS takeover

```
Reader                          Worker
  │                               │
  ├─ send STARTTLS to events ────►│
  │  (blocks on resumeCh)         │
  │                               ├─ send 220 response
  │                               ├─ tls.Server(conn.netConn, config)
  │                               ├─ replace conn.r, conn.w with TLS wrappers
  │                               ├─ reset gotHelo=false, phase=phaseInit
  │                               ├─ resumeCh <- struct{}{}
  ◄─ unblocks                     │
  │                               │
  └─ resumes reading on TLS conn  └─ loops, next command arrives encrypted
```

## Transaction state machine

```
                    ┌──────────┐
     new connection  │ phaseInit│◄──────── RSET or successful DATA ──┐
                    └────┬─────┘                                     │
                         │ HELO / EHLO                               │
                    ┌────▼─────┐                                     │
                    │ phaseHelo │                                     │
                    └────┬─────┘                                     │
                         │ MAIL FROM                                 │
                    ┌────▼──────┐                                    │
                    │ phaseMail  │                                    │
                    └────┬──────┘                                    │
                         │ RCPT TO                                   │
                    ┌────▼──────┐                                    │
                    │ phaseRcpt  │────────────────────────────────────┘
                    └────┬──────┘
                         │ DATA / BDAT LAST
                         ▼
                    handler.Data callback
```

A `Tx` is created at connection start and replaced after RSET or
successful DATA/BDAT LAST.  `tx.Rcpts` accumulates all presented
recipients; `tx.Accepted` contains those the handler accepted.  DATA is
only called if `len(tx.Accepted) > 0`.

## Handler contract

```go
type Handler interface {
    Hello(ctx, tx)  *Response  // called after HELO/EHLO
    MailFrom(ctx, tx) *Response // called after MAIL FROM
    RcptTo(ctx, tx) *Response   // called once per RCPT TO
    Data(ctx, tx, body) *Response // called after DATA or BDAT LAST
}
```

Key invariants:

- **One Handler per server.**  All connections share the same instance.
  Make it goroutine-safe if it holds mutable state.
- **Callbacks are serial per connection.**  Within a single connection,
  Hello → MailFrom → RcptTo* → Data are called sequentially from the
  worker loop.  You don't need per-connection locking.
- **Return nil → 503.**  If a callback returns nil, the server
  substitutes `503 Bad sequence`.  Use the `Resp*` constants (or
  `Resp*.Copy()`) for typed responses.
- **Data only fires with accepted recipients.**  If all RCPT TOs are
  rejected, Data is skipped and the client gets the rejection codes.
- **tx is read-only for callbacks.**  Don't mutate Tx fields — the
  server manages them.
- **Hello 250 message is replaced.**  Any 250 response from Hello has
  its message overwritten with the server hostname.

## Postcat persistence

When `Server.PostcatDir` is set and a handler returns 250 for Data, the
server calls `postcat.Write()`:

1. Open temp file via `os.CreateTemp` (random suffix, same directory)
2. Write envelope records (S/R/T lines) + separator + body
3. `Flush` → `Sync` → `Close`
4. `os.Rename` into place

The CreateTemp + Rename pattern makes writes atomic: readers see either
the complete file or nothing.  The random suffix prevents collisions
under concurrent writes.

## Shutdown sequence

```
Shutdown(ctx)
  └─ s.cancel()             // cancels s.ctx → all conn.ctx cancelled
       │
       ├─ Worker select loop: conn.ctx.Done() fires → send 421 → return
       ├─ Reader: conn.netConn closed after 50ms grace → ReadString fails
       │         → close events → return
       └─ handleConn: both goroutines exit → connCancel() → conn.Close()
                      → wg.Done()

Serve()
  └─ ln.Accept() returns error (listener closed by caller)
     └─ s.ctx.Done() is true → s.wg.Wait() → return nil
```
