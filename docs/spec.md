# Specification

This document defines correct behaviour for `smtp-gateway`. It is normative.
Code that disagrees with a clause is a bug in the code. A clause that disagrees
with the code is a bug in this document.

The keywords MUST, MUST NOT, SHOULD, SHOULD NOT, and MAY are used as defined in
RFC 2119. Uppercase keywords are deliberate. Do not soften them.

Clause IDs are stable. Never renumber an existing clause. Add a new one at the
end of its group. When a clause is removed, mark it `withdrawn` and say which
clause replaced it.

## 1. Terms

| Term | Meaning |
|---|---|
| Server | The `Server` value. It owns configuration and the connection set |
| Handler | The user implementation of the four callbacks |
| Session | One TCP connection, from banner to close |
| Transaction | One mail delivery: MAIL, RCPT, DATA or BDAT |
| Tx | The per-transaction state passed to the Handler |
| Takeover | The worker reading from the socket while the reader is paused |
| Postcat file | The on-disk record written when `PostcatDir` is set |
| Gap | A known difference between a clause and the current code, listed in §14 |

## 2. Status

Each clause has a status.

| Status | Meaning |
|---|---|
| `ok` | The code matches, and a test covers it |
| `ok*` | The code matches, but no test targets it directly |
| `gap` | The code differs. See the `X` entry in §14 |
| `contract` | A rule for callers. The server does not enforce it at run time |
| `partial` | Implemented for a subset of the stated cases |
| `withdrawn` | No longer in force. Say what replaced it |

A change to a clause follows the protocol in §16.

## 3. Server lifecycle (S)

| ID | Requirement | Status |
|---|---|---|
| S1 | `Serve` MUST return an error and accept nothing when `Handler` is nil. | ok* |
| S2 | `Serve` MUST create `PostcatDir` with mode 0750 when the directory does not exist. | ok* |
| S3 | `Serve` MUST default zero-value fields: `Hostname` from `os.Hostname()` (fallback `localhost`), `MaxMessageSize` to 25 MiB, `ReadTimeout` to 5 minutes, `IdleTimeout` to 5 minutes. | ok |
| S4 | `NewServer` MUST apply the same defaults eagerly and then apply options in order. The last option for a field wins. | ok |
| S5 | `MaxMessageSize == 0` MUST mean the default. It MUST NOT be possible to disable the size limit. | gap (X18) |
| S6 | `MaxConnections == 0` MUST mean unlimited. A positive value MUST cap concurrent connections. | ok |
| S7 | Callers MUST NOT read or mutate `Server` fields after `Serve` starts. `Serve` is not safe for concurrent configuration. | contract |
| S8 | `Serve` MUST return `nil` when `Accept` fails after `Shutdown`, and the listener error otherwise. | ok* |
| S9 | `Shutdown` MUST stop new work, cancel connection contexts, force-close stuck connections after 50 ms, and wait for handlers to return or for `ctx` to expire. | ok |
| S10 | `Shutdown` MUST NOT close the listener. The caller MUST close it to unblock `Serve`. | contract |
| S11 | A `Server` MUST serve one listener once. Reusing it for a second `Serve` is not supported. | contract |
| S12 | `NewServer` MUST resolve the hostname before returning. With a struct literal, `Serve` resolves it. Reading `Hostname` before `Serve` in the literal path yields `""`. | ok* |
| S13 | `Shutdown` MUST be idempotent. | ok* |
| S14 | A negative value for `MaxMessageSize`, `MaxConnections`, `MaxRecipients`, `ReadTimeout`, `WriteTimeout`, or `IdleTimeout` MUST be rejected with an error from `Serve`. | gap (X18) |
| S15 | `RequireTLS` MUST be false by default. `Serve` MUST return an error when `RequireTLS` is true and `TLSConfig` is nil. | ok |

## 4. Connection model (C)

These clauses describe invariants of the implementation. They are checkable by
reading the code and by the concurrency tests.

| ID | Requirement | Status |
|---|---|---|
| C1 | Each connection MUST run one reader goroutine, one worker loop, and one watchdog goroutine. | ok |
| C2 | The reader MUST be the only goroutine that reads from the socket. The worker MUST be the only goroutine that writes replies. Replies MUST be written in command order. | ok |
| C3 | The reader MUST pause on `resumeCh` before the worker takes over the socket for DATA, BDAT, or STARTTLS. The worker MUST signal `resumeCh` on every return path. | ok |
| C4 | The watchdog MUST close the socket no earlier than 50 ms after the connection context is cancelled. The delay lets the worker send 421 first. | ok |
| C5 | On shutdown, the worker SHOULD send `421 4.3.0 Service shutting down` before it returns, when the socket is still writable. | ok |
| C6 | A panic in a handler or in the reader MUST NOT crash the process. The server MUST log it and close the connection. | ok* |
| C7 | `Tx.TLS` MUST describe the TLS session whenever the connection is protected by TLS, including implicit TLS through a `tls.Listener`. The state MUST survive RSET and DATA. | ok |
| C8 | Response writes MUST be serialised and flushed before the next command is processed. | ok |

## 5. Transaction state (T)

| ID | Requirement | Status |
|---|---|---|
| T1 | The server MUST create a fresh `Tx` for each connection. | ok |
| T2 | HELO and EHLO MUST set `tx.Helo`. Any 250 message returned by `Handler.Hello` MUST be replaced with the server hostname. | ok |
| T3 | MAIL MUST set `tx.MailFrom` and `tx.Params`, and MUST clear `Rcpts`, `Accepted`, and `Rejected`. | ok |
| T4 | RCPT MUST append the presented recipient to `tx.Rcpts`, then record it in `Accepted` or `Rejected` by handler response. | ok |
| T5 | `Handler.Data` MUST run only when `len(tx.Accepted) > 0`. | ok |
| T6 | RSET MUST replace `Tx`, clear the BDAT buffer, and clear the transaction phase. It MUST keep the HELO/EHLO state. | ok |
| T7 | After DATA, or after `BDAT ... LAST`, the server MUST require a new MAIL before the next RCPT, and MUST NOT require a new HELO. | ok |
| T8 | Callbacks for one connection MUST be serial. Callbacks for different connections MAY run concurrently on the same Handler. | ok |
| T9 | Handlers MUST treat `Tx` as read-only. The server does not defend against mutation. | contract |
| T10 | A successful STARTTLS MUST discard the HELO/EHLO state. The client MUST send a new EHLO before MAIL. | ok |
| T11 | A rejected `BDAT ... LAST` MUST close the transaction the same way a rejected DATA does. | gap (X6) |

## 6. Commands (P)

| ID | Requirement | Status |
|---|---|---|
| P1 | HELO and EHLO MUST reject an empty argument with 501, an argument longer than 255 bytes with 501, and an argument containing a control character with 501. Rejection MUST leave the state unchanged. | ok |
| P2 | A later HELO or EHLO in the same session MUST be accepted when the argument is valid, and MUST reset the transaction as if RSET had run (RFC 5321 §4.1.4). A rejected greeting MUST leave the state unchanged. | ok |
| P3 | MAIL MUST require `FROM:<path>` syntax. The path MAY be empty (`<>`) for the null sender. Parameters MUST be parsed as case-insensitive keys with optional values. | ok |
| P4 | MAIL out of sequence MUST return 503. Malformed syntax MUST return 501. A `SIZE` parameter above the limit MUST return 552 before the handler is called. | ok |
| P5 | RCPT MUST require MAIL first, else 503. An address longer than 320 bytes or containing a control character MUST return 501. Exceeding `MaxRecipients` MUST return 452. | ok |
| P6 | The server MUST pass `RCPT TO:<Postmaster>` to the handler unchanged. Accepting the reserved postmaster mailbox is the handler's duty (RFC 5321 §4.5.1). | contract |
| P7 | DATA before RCPT MUST return 503. DATA with no accepted recipient MUST return 554. Otherwise the server MUST send 354, read the dot-stuffed body, and call `Handler.Data`. | ok |
| P8 | DATA MUST end the transaction on every return path, including rejection. | ok |
| P9 | BDAT MUST parse `<size> [LAST]`, read exactly `size` bytes, return 250 for each chunk, and call `Handler.Data` only on LAST. | ok |
| P10 | During a BDAT sequence, only BDAT, RSET, NOOP, and QUIT MAY be accepted. Any other command MUST return 503 and MUST discard the partial body. DATA after BDAT in one transaction MUST return 503. | ok |
| P11 | RSET MUST return 250 and clear the transaction. | ok |
| P12 | NOOP MUST return 250 and change nothing. | ok |
| P13 | VRFY and EXPN MUST return 252. | ok |
| P14 | QUIT MUST return 221 and close the session. | ok |
| P15 | An unknown verb MUST return 500. | ok |
| P16 | HELP SHOULD return helpful text (RFC 5321 §4.1.1.8). | gap (X16) |
| P17 | An over-long command line MUST be rejected with a reply, not by a bare close, and the server MUST NOT buffer it without bound. | gap (X4) |
| P18 | A bare LF MUST terminate a line. A dot-stuffed line that ends with bare LF MUST NOT be treated as the end of data. | ok |

## 7. Extensions (E)

| ID | Requirement | Status |
|---|---|---|
| E1 | EHLO MUST advertise exactly these capabilities: PIPELINING, 8BITMIME, ENHANCEDSTATUSCODES, SMTPUTF8, CHUNKING, and `SIZE <bytes>` when the limit is positive. It MUST add STARTTLS when `TLSConfig` is set and the session is not yet protected by TLS. | ok |
| E2 | The server MUST NOT advertise STARTTLS in an EHLO received after a successful TLS handshake, including implicit TLS (RFC 3207 §4.2). | ok |
| E3 | PIPELINING: the reader MUST accept commands ahead of processing, up to the channel depth of 32. Replies MUST stay in order. DATA, BDAT, and STARTTLS MUST stop read-ahead until the worker finishes. | ok |
| E4 | STARTTLS MUST require EHLO first (503), MUST return 502 when not configured, and MUST return 503 when TLS is already active. On success the server MUST send 220 and perform the handshake within 30 seconds. | ok |
| E5 | A publicly referenced server MUST NOT require TLS for local delivery (RFC 3207 §3.1). The default configuration MUST accept plaintext MAIL. `RequireTLS` MAY require TLS for private deployments. | ok |
| E6 | 8BITMIME: the server MUST advertise it and MUST reject an 8-bit DATA body that arrives without `BODY=8BITMIME` or `SMTPUTF8`, using 550. | ok |
| E7 | SIZE: the server MUST advertise the limit, reject a larger MAIL `SIZE` parameter, and enforce the limit for DATA and BDAT. | ok |
| E8 | SMTPUTF8: the server MUST accept UTF-8 in envelope paths and MUST allow 8-bit bodies when the MAIL command carried `SMTPUTF8`. | partial |
| E9 | CHUNKING: the server MUST support BDAT as in P9 and P10. CHUNKING does not imply BINARYMIME; the two are separate extensions (RFC 3030 §3). | ok |
| E10 | ENHANCEDSTATUSCODES: replies SHOULD carry a 2.x.x, 4.x.x, or 5.x.x code. The 354 reply to DATA currently has none. | partial |

## 8. Handler contract (H)

| ID | Requirement | Status |
|---|---|---|
| H1 | A nil response from any callback MUST become `503 5.5.1 Bad sequence of commands`. | ok |
| H2 | `Hello` MUST run after a valid HELO or EHLO, before any other callback for that session. A non-250 response MUST reject the greeting and MUST leave the existing transaction state unchanged. | ok |
| H3 | `MailFrom` MUST run after a valid MAIL command, before RCPT. A non-250 response MUST reject the sender. | ok |
| H4 | `RcptTo` MUST run once per RCPT command. The presented recipient MUST be the last element of `tx.Rcpts`. A 250 response MUST add it to `tx.Accepted`; any other code MUST add a `Rejection`. | ok |
| H5 | `Data` MUST receive the dot-unstuffed RFC 5322 bytes. The server does not promise the slice's lifetime after the callback returns; a handler that retains it MUST copy it. | ok* |
| H6 | A non-250 response from `Data` MUST bounce the message and MUST NOT write a postcat file. | ok |
| H7 | A handler MUST be safe for concurrent use. One instance serves all connections. | contract |
| H8 | A handler SHOULD honour the context. The server cancels it on connection close or shutdown. | contract |

## 9. Limits and timeouts (L)

| ID | Requirement | Status |
|---|---|---|
| L1 | `ReadTimeout` is a per-read deadline, not a whole-session budget. The default is 5 minutes. Zero is replaced by the default. | ok |
| L2 | `WriteTimeout` is a per-response deadline. Zero means no deadline. | ok |
| L3 | `IdleTimeout` MUST close a connection after the configured inactivity. The effective per-read deadline MUST be the smaller of `IdleTimeout` and `ReadTimeout`. | ok |
| L4 | `MaxMessageSize` limits DATA and the total BDAT body. It applies to the unstuffed size. | ok |
| L5 | A single BDAT chunk MUST NOT exceed 64 MiB, independent of `MaxMessageSize`. | ok* |
| L6 | The TLS handshake deadline is 30 seconds and is not configurable. | ok* |
| L7 | The reader waits at most `ReadTimeout + 1 minute` for the worker to resume it. The value is not configurable. | ok* |
| L8 | There is no overall deadline for a DATA body. A client that sends one line per `ReadTimeout` can keep a message open indefinitely. | known limit |

## 10. Logging (G)

| ID | Requirement | Status |
|---|---|---|
| G1 | `Server.Logger` MUST accept any implementation of the three-method `Logger`. A nil `Logger` MUST discard all events. | ok |
| G2 | Levels MUST follow the event catalog in `docs/logging.md`: connection lifecycle at Debug, command and message events at Info, failures at Error. | ok |
| G3 | Event names and keys MUST match `docs/logging.md`. Change the code and the catalog in one commit. | ok |
| G4 | Logged client input MUST have control characters removed and MUST be length-bounded. | ok |
| G5 | The default Info stream SHOULD NOT contain full envelope addresses. It currently does (`smtp_recv`). Operators MUST treat the log stream as personal data. | gap (X17) |

## 11. Postcat files (F)

| ID | Requirement | Status |
|---|---|---|
| F1 | A postcat file MUST contain `S <sender>`, zero or more `R <recipient>`, `T <RFC3339>`, a blank line, and then the raw RFC 5322 bytes. Each record ends with LF. | ok |
| F2 | The null sender MUST be written as `<>`. | ok |
| F3 | CR and LF in envelope addresses MUST be removed before writing. | ok* |
| F4 | The server MUST write the file only after the handler returns 250, and MUST treat the write as best effort. A write failure MUST NOT change the SMTP reply. | ok |
| F5 | The write MUST use a temporary file in the target directory and an atomic rename. An error MUST NOT leave a partial file. | ok |
| F6 | `Parse` MUST accept LF and CRLF envelopes and MUST locate the first blank line after the envelope records. | gap (X7) |
| F7 | `FormatEnvelope` SHOULD be the only formatter of the record section. `Write` MUST use it. | gap (X11) |
| F8 | The file name MUST be unique. The current form is `<unix>-<nanosecond>-<random>.eml`. | ok |
| F9 | The format is Postfix-style, not the Postfix queue-file format, and not `postcat(1)` output. Documentation MUST NOT claim compatibility without an interoperability test. | gap (X10) |
| F10 | `Parse` MUST return an error when a `T` record is present but not RFC 3339. A file without a separator MUST parse as an empty message with no body. | ok |

## 12. Quality gates (Q)

| ID | Requirement | Status |
|---|---|---|
| Q1 | `make check` MUST pass on a clean checkout: gofmt, `go vet`, golangci-lint, and `go test -race ./...`. | ok |
| Q2 | CI MUST run a gofmt check, lint, race tests, and the eight e2e suites on Linux. | ok |
| Q3 | A test MUST NOT protect a known gap without an `X` reference in a comment. | contract |
| Q4 | Parsers SHOULD have fuzz targets: `parseMailFrom`, `parseRcptTo`, `parseBdatArgs`, and `readDotUnstuffed`. | gap (X13) |
| Q5 | The module MUST build and pass tests on the Go version in `go.mod` (1.21). CI currently tests only 1.26. | gap (X12) |
| Q6 | Releases SHOULD be tagged with semantic versions so `go get` resolves a stable version. No tag exists today. | gap (X14) |
| Q7 | Commits MUST be atomic and follow `AGENTS.md`. Documents MUST change in the commit that makes them true. | contract |

## 13. Non-goals

These are out of scope, not missing features.

- SMTP AUTH and message submission on port 587 (RFC 4954, RFC 6409).
- Outbound relaying, queueing, retry, and DSN generation.
- DKIM, SPF, DMARC, and content scanning.
- BINARYMIME.
- Remote configuration or a management API.
- Windows service integration.

## 14. Conformance gaps

A gap is a known difference between a clause and the code. It is a bug with a
date and an owner, not a licence to keep the behaviour.

| ID | Severity | Clause | Current behaviour | Evidence | Fix |
|---|---|---|---|---|---|
| X4 | High | P17 | `readLine` calls `bufio.Reader.ReadString` and checks the length afterwards, so a long line is buffered in full. An over-long line closes the socket with no reply. | `smtp.go:926-940`; probe: a 1 MiB line produced no response | Read with a bounded buffer, or use `ReadSlice` and count. Return 500 and close after the limit. Add a test at 512 bytes and at 64 KiB. |
| X6 | Medium | T11 | A non-250 handler response to `BDAT ... LAST` leaves the transaction open. A later BDAT can call `Data` again without MAIL or RCPT. DATA does reset. | `smtp.go:185-191`, `smtp.go:781-791` | Reset phase and `Tx` on any completed LAST, matching DATA. Add a test. |
| X7 | Medium | F6 | `Parse` looks for `\n\n` before `\r\n\r\n`. A CRLF file whose body contains an LF blank line is truncated at the wrong offset. | `postcat.go:162-170`; reproduced: body `line1\n\nline2` parsed as `line2` | Find the earliest separator of either form, and require it to follow the last record line. Add LF and CRLF tests. |
| X8 | Low | F5 | `Write` does not close the temp file on early write errors. The descriptor leaks until GC. | `postcat.go:75-95` | Close on every error path, or use a single deferred close. |
| X9 | Low | F5 | The file is fsynced but the directory is not. The rename survives a process crash but not a power loss. | `postcat.go:101-112` | Sync the directory before returning, or state the weaker guarantee in `docs/postcat.md`. |
| X10 | Low | F9 | Docs call the format "Postfix-compatible". `postcat(1)` prints Postfix queue files, which this format is not. The `T` record is RFC 3339, not an epoch. | `README.md`, `docs/postcat.md`, `internal/postcat/postcat.go` package doc; initial commit message | Say "Postfix-style". Add an evidence note. Do not claim compatibility. |
| X11 | Medium | F7 | `FormatEnvelope` is documented as the single authority but `Write` duplicates the record code. `FormatEnvelope` has 0% coverage. | `postcat.go:35-47` vs `postcat.go:75-84`; `go tool cover` | Make `Write` call `FormatEnvelope`, or extract a record writer. Add a test. |
| X12 | Medium | Q5 | `go.mod` declares Go 1.21. CI tests only 1.26, so a dependency or syntax that raises the floor is not caught. | `go.mod:3`, `.github/workflows/ci.yml` | Add a CI job with `go-version-file: go.mod`. |
| X13 | Medium | Q4 | No fuzz targets exist. All four CLI packages have 0% coverage. | `grep -r "func Fuzz" .` returns nothing; `go tool cover` | Add fuzz tests for the parsers and body reader. Add one test per CLI. |
| X14 | Low | Q6 | The repository has no tags. `go get` resolves a pseudo-version. | `git tag` is empty | Tag `v0.1.0` when the API is ready. |
| X15 | Low | S6 | Rejected connections at `MaxConnections` are closed with no 421 and logged at Error. A flood of connections floods the error log. | `gateway.go:351-358` | Send a 421 with a short deadline, then close. Log at Info or rate-limit. |
| X16 | Low | P16 | HELP returns 500. RFC 5321 §4.1.1.8 says servers SHOULD support it. | `smtp.go:205-207` | Return a short 214 multi-line reply. |
| X17 | Medium | G5 | `smtp_recv` logs the full command arguments at Info, including sender and recipient addresses. | `smtp.go:260-263` | Log the verb at Info and the arguments at Debug, or add a redaction option. Document the change. |
| X18 | Low | S5, S14 | A negative limit or duration silently disables the feature instead of failing. `MaxMessageSize: -1` removes the size limit and the SIZE advertisement; negative timeouts remove deadlines. | `gateway.go:328-336` only defaults `== 0`; `smtp.go:306-315`, `smtp.go:926-929` test `> 0` | Validate configuration in `Serve` and return an error. Add a table-driven test with negative values. |

Severity: High means protocol non-compliance or a denial-of-service risk.
Medium means data loss, interop risk, or a broken promise in the docs. Low means
a defect with a narrow trigger or a documentation defect.

## 15. Test map

| File | Owns |
|---|---|
| `smtp_test.go` | Parser unit tests, `readDotUnstuffed`, `readNBytes`, postcat round trip, handler rejection, concurrency smoke tests, small helpers |
| `smtp_rfc_test.go` | TCP-level protocol, STARTTLS, BDAT, SMTPUTF8, size limits, shutdown, connection cap, bare-LF smuggling |
| `smtp_bench_test.go` | Benchmarks only |
| `gateway_test.go` | `NewServer`, options, `SlogLogger` |
| `example_test.go` | Package example |
| `internal/postcat/postcat_test.go` | `Write`, `Parse`, null sender, empty body |
| `e2e/run-e2e*.sh` | Real clients: swaks, mutt, msmtp, curl; TLS, BDAT, pipelining, concurrency |

Coverage on 2026-10-27: 67.0% of statements in the root module. Recompute with:

```bash
go test -count=1 -coverprofile=coverage.out ./...
go tool cover -func=coverage.out | tail -1
```

## 16. Change protocol

1. Find the clause. If it does not exist, write it first and decide its status.
2. Write a test that fails for the right reason.
3. Change the code.
4. Update the clause status, the test map, and §14 when a gap is fixed or added.
5. Add an ADR entry if the change is a design decision.
6. Commit the spec, code, tests, and user docs together.

A gap may be added only with an `X` entry in the same commit. A test that
protects a gap must cite the `X` ID in a comment.
