# Architecture

This document describes how the code implements `docs/spec.md`. The spec is
normative. If this document and the code disagree, the code is right and this
document is a bug. If the code and the spec disagree, the code is a bug and the
gap belongs in `docs/spec.md` §14.

## Package layout

```
gateway.go            Public API: Server, Handler, Tx, Response, Logger,
                      options, Serve, Shutdown
smtp.go               Protocol: parser, command handlers, connection state,
                      three-goroutine model, takeover, postcat hook
internal/postcat/     Postcat file write, parse, format
cmd/test-server/      Reference server for development and e2e tests
cmd/postcat/          CLI: inspect one postcat file
cmd/postcat-replay/   CLI: replay files through an SMTP server
cmd/verify-postcat/   CLI: batch-validate a postcat directory
examples/s3/          Example handler: compress and store to S3
examples/postcat-only/Example handler: accept everything, write postcat
bench-go-smtp/        Benchmark against emersion/go-smtp
e2e/                  Shell suites against real SMTP clients
```

The two example modules and `bench-go-smtp` are separate Go modules. The root
`go build ./...` does not build them. `make examples` does.

## Per-connection goroutine model

Each accepted connection runs three goroutines inside `handleConn` (`smtp.go`).

```
  net.Conn
     |
     +-- Reader goroutine (readCommands)
     |     - reads lines with readLine (CRLF or bare LF)
     |     - parses the verb and arguments
     |     - sends smtpCmd to events (depth 32)
     |     - pauses on resumeCh during DATA, BDAT, STARTTLS
     |
     +-- Worker loop (the rest of handleConn)
     |     - receives from events
     |     - dispatches to handleHelo, handleEhlo, handleMail, handleRcpt,
     |       handleData, handleBdat, handleStartTLS
     |     - runs handler callbacks, serially per connection
     |     - writes every reply through conn.write
     |
     +-- Watchdog
           - waits on connCtx.Done()
           - sleeps 50 ms
           - closes netConn
```

The reader is the only goroutine that reads from the socket. The worker is the
only goroutine that writes. The watchdog never touches the protocol.

The `events` channel is what makes PIPELINING (RFC 2920) work. The reader can
enqueue several commands while the worker processes an earlier one. The worker
drains them in order. The channel depth is 32. When it is full, the reader
blocks, which applies backpressure to the client.

The watchdog exists for shutdown. If the worker is blocked in `readDotUnstuffed`
or a TLS handshake, cancelling the context alone would not wake it. Closing the
socket does. The 50 ms delay lets the worker notice the context first and send
421.

## Read and write paths

Reads: `readLine` uses `bufio.Reader.ReadString('\n')`. It strips CRLF or LF.
The line limit check happens after the read (gap X4).

Writes: `connState.write` holds a mutex, sets a write deadline when
`WriteTimeout > 0`, writes one string, flushes, and clears the deadline. Only
the worker calls it, so the mutex is belt and braces. A flush error resets the
writer so stale bytes cannot prefix the next reply.

Three handlers write their reply themselves and return a nil response:

- `handleEhlo` writes the multi-line 250 response.
- `handleStartTLS` writes 220 before the handshake.
- `handleData` writes 354 before the body.

The worker writes the returned `*Response` for every other path.

## Connection takeover

For DATA, BDAT, and STARTTLS the reader pauses after enqueueing the command and
waits on `resumeCh` (capacity 1). The worker takes direct control of the
connection and signals `resumeCh` on every return path. This prevents the two
goroutines from racing for bytes on one socket. Clause C3.

### DATA

```
Reader                              Worker
  |                                    |
  +-- send DATA to events -----------> |
  |   (blocks on resumeCh)             |
  |                                    +-- phase and recipient checks
  |                                    +-- write 354
  |                                    +-- readDotUnstuffed(conn.r)
  |                                    +-- 8BITMIME check
  |                                    +-- Handler.Data(ctx, tx, body)
  |                                    +-- postcat.Write on 250
  |                                    +-- write 250/4xx/5xx
  |                                    +-- reset tx, phase
  <-- resumeCh ------------------------+
  |                                    |
  +-- read next command                +-- next event
```

When DATA is rejected before the body read (wrong phase, no accepted
recipients), the server has consumed no body bytes. The reader resumes and
parses the body lines as commands. Most get 500. RFC 5321 requires the client to
RSET after a failed DATA, so this is acceptable. The alternative, draining the
body for a command that was never accepted, risks desynchronising a client that
did not send it.

### BDAT

```
Reader                              Worker
  |                                    |
  +-- send BDAT n [LAST] ------------> |
  |   (blocks on resumeCh)             |
  |                                    +-- parse size and LAST
  |                                    +-- reject? discard exactly n bytes
  |                                    +-- read exactly n bytes
  |                                    +-- append to conn.bodyBuf
  |                                    +-- not LAST: 250, resume
  |                                    +-- LAST: Handler.Data, postcat, reset
  <-- resumeCh ------------------------+
```

`discardChunk` uses `bufio.Reader.Discard` so a rejected `BDAT <huge>` cannot
force a proportional allocation. A single chunk is capped at 64 MiB.

While a BDAT sequence is open, the worker rejects every verb except BDAT, RSET,
NOOP, and QUIT. RSET and a bad-sequence rejection both call `resetBodyBuf`, so
orphaned chunk data cannot leak into the next transaction.

### STARTTLS

```
Reader                              Worker
  |                                    |
  +-- send STARTTLS to events -------> |
  |   (blocks on resumeCh)             |
  |                                    +-- checks: EHLO, TLSConfig, not active
  |                                    +-- write 220
  |                                    +-- tls.Server(conn.netConn).Handshake
  |                                    +-- replace conn.r and conn.w
  |                                    +-- conn.tlsState = state
  |                                    +-- gotHelo = false, phase = init
  <-- resumeCh ------------------------+
  |                                    |
  +-- read on the TLS connection       +-- next event
```

The handshake has a 30 second deadline, independent of `ReadTimeout`. The
listener is not replaced; only the buffered reader and writer wrap the TLS
connection. Bytes the client pipelined after STARTTLS are discarded with the
old plaintext reader, which is correct: RFC 3207 forbids pipelining the
handshake.

TLS state lives on `connState`, not on `Tx`. Every new `Tx` copies it, so
`Tx.TLS` survives RSET and DATA. For implicit TLS the state is captured after
the banner write, because `tls.Conn` performs the handshake lazily.

`handleMail` returns 530 only when `RequireTLS` is set and `conn.tlsState` is
nil. The default accepts plaintext mail (E5).

## Transaction state machine

```
[*] --> phaseInit : new connection
phaseInit --> phaseHelo : HELO / EHLO accepted
phaseHelo --> phaseMail : MAIL accepted
phaseMail --> phaseRcpt : RCPT (accepted or rejected)
phaseRcpt --> phaseHelo : DATA returns (success or bounce)
phaseRcpt --> phaseInit : BDAT LAST returns (success)
phaseMail --> phaseInit : RSET
phaseHelo --> phaseInit : RSET
```

`gotHelo` is tracked separately from `phase`. RSET clears `phase` but keeps
`gotHelo`, so the client does not need to greet again. DATA sets `phase` to
`phaseHelo` but keeps `gotHelo`. BDAT LAST sets `phase` to `phaseInit`; because
`gotHelo` is still true, the observable behaviour is the same as DATA: a new
MAIL is required, a new HELO is not (clause T7). The difference in enum value is
an implementation detail, not a contract.

`Tx` is created at connection start. A new `Tx` replaces it on RSET, after DATA,
after a successful BDAT LAST, on a later HELO/EHLO, and on a successful
STARTTLS. The reset paths keep the HELO state. STARTTLS and a new greeting set
it from scratch. `tx.Rcpts` accumulates every presented recipient.
`tx.Accepted` and `tx.Rejected` record the handler's decisions.

A later HELO/EHLO from any phase returns the machine to `phaseHelo` and clears
the envelope. A greeting the handler rejects leaves the state unchanged
(RFC 5321 §4.1.4).

## Handler contract

The full contract is clauses H1 to H8 and `docs/handler-guide.md`. The short
version:

- Four callbacks, serial per connection.
- One handler instance for all connections. Guard shared state.
- Nil becomes 503.
- `Data` only runs with at least one accepted recipient.
- The server owns `Tx`. Do not mutate it.
- `Data` receives a slice whose lifetime is not promised. Copy before retaining.

## Postcat persistence

When `Server.PostcatDir` is set and `Handler.Data` returns 250, `writePostcat`
calls `postcat.Write`:

1. `os.CreateTemp` in the target directory with a random suffix.
2. Write S/R/T records, a blank line, and the body.
3. Flush, fsync, close.
4. `os.Rename` to `<unix>-<nanosecond>-<random>.eml`.

The rename makes the file appear atomically: a reader sees the complete file or
nothing. The directory is not fsynced, so the file may not survive a power loss
(gap X9). The write is best effort: the handler already returned 250, so a
failure is logged and the client still gets 250.

`FormatEnvelope` is documented as the single formatter, but `Write` duplicates
the record loop today (gap X11).

## Shutdown

```
Shutdown(ctx)
  +-- shutdownOnce.Do(cancel)
  +-- wait for wg or ctx

connection context cancelled
  +-- worker select sees ctx.Done -> write 421 -> return
  +-- watchdog closes netConn after 50 ms
  +-- reader's blocked read returns an error
  +-- handleConn defer: connCancel, conn.Close, wg.Done

Serve()
  +-- Accept returns an error
       +-- ctx cancelled? wait for wg, return nil
       +-- otherwise return the listener error
```

The caller must close the listener. `Shutdown` does not close it (S10).

## Failure modes

| Trigger | Server action | What the client sees |
|---|---|---|
| Peer closes | Reader sees EOF, closes `events`, worker returns | Nothing; socket closed |
| Read timeout | Reader returns, closes `events` | Socket closed, no reply |
| Command line over 64 KiB | Reader returns | Socket closed, no reply (X4) |
| Handler panic | Recover, log `handler_panic`, close | Socket closed, no reply |
| Reader panic | Recover, log `reader_panic`, close | Socket closed, no reply |
| Write error | Worker returns | Socket closed |
| TLS handshake failure | Log, close | TLS alert or reset |
| `MaxConnections` full | Close immediately, log | Socket closed, no 421 (X15) |
| Shutdown | 421 when the socket is still open | 421, then close |

A silent close is valid SMTP behaviour for a broken session, but a client sees
it as a transient failure. The gaps above note the cases where a reply would be
better.

## Where behaviour lives

| Behaviour | Code |
|---|---|
| Defaults, options, listener loop, shutdown | `gateway.go` |
| Line reading, command parsing, body reading | `smtp.go` lower half |
| Command sequencing and state | `smtp.go` `handleConn` switch |
| HELO/EHLO reply text | `smtp.go` `handleHelo`, `handleEhlo` |
| MAIL/RCPT parsing | `smtp.go` `parseMailFrom`, `parseRcptTo` |
| DATA and 8BITMIME | `smtp.go` `handleData` |
| BDAT | `smtp.go` `handleBdat`, `readNBytes` |
| STARTTLS | `smtp.go` `handleStartTLS`, `connState.upgradeToTLS` |
| Logging events | Calls to `s.logDebug`, `s.logInfo`, `s.logError` |
| Postcat format | `internal/postcat/postcat.go` |

## Related documents

- `docs/spec.md`: normative clauses and gaps.
- `docs/decisions.md`: why the design is what it is.
- `docs/handler-guide.md`: task-oriented handler walkthrough.
- `docs/logging.md`: event catalog.
- `docs/postcat.md`: file format and CLI tools.
- `CONCEPTS.md`: vocabulary.
