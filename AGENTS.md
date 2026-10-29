# AGENTS.md

The single guide for anyone, human or agent, working in this repository.
Claude Code users may enter through `CLAUDE.md`. That file only points here.
Do not add tool-specific instructions anywhere else.

Read `docs/spec.md` before you change behaviour. It is the definition of
correct. This file tells you how to work.

## What this repository is

`smtp-gateway` is a Go library. It runs an SMTP server on a `net.Listener` that
you provide, and it calls your `Handler` at each phase of a mail transaction.
It is built for inbound mail: port 25, MX-style traffic, no SMTP AUTH.
Accepted messages go to your handler. If `PostcatDir` is set, the server also
writes each accepted message to disk.

Status: pre-1.0, tagged v0.1.0. The public API can change. `README.md` is
for users. `docs/spec.md` is for conformance.

## Read in this order

1. This file.
2. `docs/spec.md`: the normative clauses and the list of known gaps. Read only
   the clause groups for the code you will touch.
3. `docs/architecture.md`: how the code implements the clauses.
4. `docs/decisions.md`: why the design is what it is.
5. `CONCEPTS.md`: the vocabulary. Use these terms in code, comments, and docs.

Do not read every document for every change. Read the clause group and the
relevant section of the architecture.

## Golden rules

1. **The spec is the source of truth.** A behaviour change starts with a spec
   clause, not with code.
2. **Change the spec, the code, the tests, and the user docs in one atomic
   commit.** A behaviour change and its clause, test, and guide are one reason.
   A pure docs or pure test commit is fine when it has one reason.
3. **A stale document is a bug.** If your change makes a sentence false, fix
   the sentence in the same commit. See "Where docs go" below.
4. **Tests can encode a known gap.** Check §14 before you trust a green test.
   Never add a test that protects a gap without a reference to its `X` ID.
5. **One commit, one reason.** Use Conventional Commits. Put the reason in the
   body. Add `Refs:` with the clause and decision IDs.
6. **No public API without a clause and a decision.** New exported names,
   options, and interfaces need a spec clause and an ADR entry.
7. **Do not weaken a test to make a change pass.** Fix the code, or change the
   spec and say why.
8. **Client input is untrusted.** HELO names, addresses, and message bodies are
   attacker-controlled. Keep control characters out of logs and files.

## Build and test

```bash
make check          # gofmt, go vet, golangci-lint, go test -race
make test           # go test -race -count=1 ./...
make lint           # golangci-lint run ./...
make fmt            # gofmt -w .
make e2e            # shell e2e suites; needs swaks, mutt, msmtp, curl, nc
make examples       # build the two example modules
```

Single test:

```bash
go test -count=1 -run TestSMTPStartTLS ./...
```

Requirements:

- Go 1.26 for the race suite. The module declares Go 1.21, and a CI job tests
  that declared floor.
- `golangci-lint` v2.12 or later. The config is schema v2.
- End-to-end tests need `swaks`, `mutt`, `msmtp`, `curl`, and `nc`.

If the sandbox cannot write the default Go caches, set them inside the
workspace:

```bash
export GOCACHE="$PWD/.gocache" GOPATH="$PWD/.gopath"
```

`make check` must pass before you commit. A green `go test` alone is not enough.
The test suite includes a docs-integrity check: a broken clause ID, status, gap
reference, or relative link fails the gate.

## Repository map

| Path | Contents |
|---|---|
| `gateway.go` | Public API: `Server`, `Handler`, `Tx`, `Response`, `Logger`, options, `Serve`, `Shutdown` |
| `smtp.go` | Protocol: parser, command handlers, per-connection goroutines, takeover, postcat hook |
| `internal/postcat/` | Postcat file write and parse |
| `cmd/test-server/` | Reference server used by the e2e scripts |
| `cmd/postcat/`, `cmd/postcat-replay/`, `cmd/verify-postcat/` | CLI tools over postcat files |
| `examples/s3/`, `examples/postcat-only/` | Example handlers, separate modules |
| `bench-go-smtp/` | Benchmark against `emersion/go-smtp`, separate module |
| `e2e/` | Shell end-to-end suites against real SMTP clients |
| `docs/` | Spec, architecture, decisions, guides |

## Architecture in one page

### Per connection: three goroutines

`handleConn` (`smtp.go`) starts:

1. **Reader** (`readCommands`): reads lines from `conn.r`, parses the verb, and
   sends `smtpCmd` to a buffered channel (`events`, depth 32). The buffer is
   what makes PIPELINING work.
2. **Worker** (the body of `handleConn`): receives commands, dispatches to
   `handleHelo`, `handleMail`, `handleData`, and so on, and writes every reply.
3. **Watchdog**: waits on the connection context, then closes the socket after
   a 50 ms grace period. This unblocks a body read or TLS handshake during
   shutdown.

The reader is the only goroutine that reads from the socket. The worker takes
over only after the reader pauses.

### Takeover

For `DATA`, `BDAT`, and `STARTTLS`, the reader sends the command and then blocks
on `resumeCh`. The worker reads the body, reads a chunk, or performs the TLS
handshake directly. It signals `resumeCh` on every return path. Without this
pause, the two goroutines race for bytes on the same connection.

### Transaction

`Tx` holds the envelope state. The server replaces it after `RSET`, after
`DATA`, after a successful `BDAT ... LAST`, after a later HELO/EHLO, and after
a successful `STARTTLS`. `tx.Rcpts` keeps all presented recipients.
`tx.Accepted` and `tx.Rejected` keep the split. `Data` runs only when at least
one recipient was accepted.

### Handler

Four callbacks: `Hello`, `MailFrom`, `RcptTo`, `Data`. They are serial per
connection and concurrent across connections. One handler instance serves all
connections. A nil return becomes `503`. The server owns `Tx`; handlers must
not mutate it.

## Invariants you must not break

Each one has a clause in `docs/spec.md`.

| Invariant | Clause |
|---|---|
| The reader is the only reader; the worker writes all replies in order | C2 |
| The reader pauses during DATA, BDAT, and STARTTLS | C3 |
| The watchdog closes the socket 50 ms after context cancel | C4 |
| `Tx` is replaced after RSET, a completed transaction, a greeting, and STARTTLS | T1, T6, T7, P2, T10 |
| `Data` runs only with at least one accepted recipient | T5 |
| Callbacks are serial per connection and concurrent across connections | T8 |
| A nil handler response becomes `503` | H1 |
| Postcat writes happen after the handler returns 250, and are best-effort | F4 |
| Client input is sanitised before it reaches logs or files | G4, F3 |

## Task recipes

### Change a protocol reply or a command behaviour

1. Find the clause in `docs/spec.md`. If none exists, write one first.
2. Add or change a TCP-level test in `smtp_rfc_test.go`. Use the existing
   helpers: `dialServer`, `sendAndExpect`, `readMultiline`.
3. Run the test. It must fail for the reason you expect.
4. Change `smtp.go`.
5. Update the clause status, the test map, and §14 if a gap moved.
6. Commit: `fix(smtp): ...` with `Refs: P4`.

### Add a Server option

1. Add a clause under S in the spec.
2. Add the field to `Server` and a `WithX` option in `gateway.go`.
3. Apply defaults in both `NewServer` and `Serve`. The two paths must agree.
4. Add a test in `gateway_test.go` for the default and the override.
5. Update the options table in `README.md` and `docs/handler-guide.md`.

### Change the Handler interface

This breaks users. Do not do it in a patch release. Add a decision entry, bump
the minor version, and update every implementation in the repo: `cmd/test-server`,
`examples/s3`, `examples/postcat-only`, and the test handlers.

### Change the postcat format

The format is a file contract. Add a decision entry. Change
`writeEnvelope`; `FormatEnvelope` and `Write` both use it. Then update `Parse`.
Add a round-trip test and a parse test for old files.

### Fix a known gap

1. Read the `X` entry in `docs/spec.md` §14. It names the clause, the evidence,
   and the current test that asserts the wrong behaviour.
2. Change the clause status only after the code and test agree.
3. Remove the `X` entry and add the test that proves the fix.

## Definition of done

- [ ] `make check` passes.
- [ ] A spec clause exists for the new or changed behaviour.
- [ ] A test fails without the change and passes with it.
- [ ] Every document that describes the old behaviour is updated.
- [ ] The docs-integrity test passes (included in `make check`).
- [ ] No new lint suppression or `//nolint` without a reason on the same line.
- [ ] No new public API without a decision entry.
- [ ] The commit message says why, and cites clause and decision IDs.

## Commits

Conventional Commits: `<type>(<scope>): <imperative summary>`.

Types: `feat`, `fix`, `docs`, `refactor`, `test`, `chore`, `perf`.

Rules:

- One logical change per commit. A bug fix and a rename are two commits.
- **Atomic means one reason, not one file.** A behaviour change ships with its
  spec clause, its test, and the user docs it makes true. Do not split the code
  from its test, or the test from the clause. A commit that changes only docs
  or only tests is valid when it has one reason.
- **Context lives in the commit message.** The diff shows what changed. The
  body says why the change was needed, what was wrong, and what you rejected.
  A reader six months from now must understand the reason without the issue.
- Reference IDs in a trailer: `Refs: P7, D4`. Use clause IDs from the spec and
  decision IDs from `docs/decisions.md`.
- Every commit builds and tests. Do not land a broken intermediate commit.
- Stage by hunk. Do not mix a refactor with a behaviour change.

Example:

```
fix(smtp): accept a second EHLO and reset the transaction

RFC 5321 §4.1.4 requires a server to accept a later EHLO and to reset
the state as if RSET had run. We returned 503, so clients that re-issue
EHLO after RSET were disconnected.

Refs: P2
```

## Where docs go

| You are writing | Put it in |
|---|---|
| A rule the server must follow | `docs/spec.md`, with a stable clause ID |
| Why a design choice was made | `docs/decisions.md`, as a new `D` entry. Never rewrite an old entry |
| How the code implements the spec | `docs/architecture.md` |
| A task for a library user | `README.md` or `docs/handler-guide.md` |
| Postcat usage and format | `docs/postcat.md` |
| A log event or its keys | `docs/logging.md` |
| A term | `CONCEPTS.md` |
| A known deviation from the spec | `docs/spec.md` §14, as an `X` entry |
| A test of a clause | The test file named in the test map |

## Writing rules

All prose follows **ASD-STE100 Simplified Technical English**. This applies to
documents, README files, comments, commit messages, and pull request text. It
does not apply to code identifiers.

Write for a reader who is tired and in a hurry.

- One instruction per sentence. Keep sentences under 20 words.
- Use the active voice. Put the actor first.
- Use the simple present. Write "the server closes the connection".
- One word for one thing. A "transaction" is always a transaction.
- Use the verb, not the noun: "validate the file", not "perform validation".
- Give a number or a source for a claim. Mark vendor claims as vendor claims.
- Do not write "simply", "just", "robust", "seamless", "powerful", or
  "effortless".
- Do not use idioms, jokes, or metaphors.
- Use a table for three or more parallel items. Use a list for steps.
- Headings name the content. No rhetorical questions.
- Start an instruction with a verb.

## Security notes

- **TLS is optional by default.** `TLSConfig` offers STARTTLS. `RequireTLS`
  returns 530 for plaintext `MAIL` and needs `TLSConfig` (E5, S15).
- **Implicit TLS is captured after the banner write.** The handshake is lazy,
  so `Tx.TLS` is set once the banner is sent (C7).
- **A handler panic drops the connection.** The server logs `handler_panic` and
  closes. It does not send 451. Do not use panics for control flow.
- **Postcat is best effort.** The client gets 250 before the file is written.
  Do not use it where durability matters.
- **Logs contain personal data.** Info message events carry the envelope
  sender. Debug `smtp_recv_args` carries command arguments. Set retention
  accordingly (G5).

## Known traps

- `TLSConfig` offers STARTTLS. It does not require it. `RequireTLS` does.
- `postcat-replay -addr` takes `host:port`. It does not accept a URL scheme.
- `MaxMessageSize` cannot be disabled. Zero means the 25 MiB default, and
  `Serve` rejects a negative value (S14).
- `NewServer` resolves the hostname eagerly. A struct literal resolves it in
  `Serve`. Reading `s.Hostname` before `Serve` returns an empty string in the
  struct-literal path.
