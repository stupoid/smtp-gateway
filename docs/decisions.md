# Decisions

This file records why the design is what it is. It is not a changelog.

Rules:

- Every entry has a stable ID (`D1`, `D2`, ...). Never renumber.
- Never edit the body of an accepted entry. Add a new entry that supersedes it
  and set `Superseded by:` on the old one.
- Keep each entry short. Say what was decided, what was rejected, and what
  follows from it.
- A change that alters a public contract needs an entry here.

Status values: `accepted`, `provisional`, `superseded`.

## D1. Ship a library, not a daemon

**Status:** accepted.

**Context:** Users already have a process, a listener, and a signal handler.

**Decision:** The package exposes `Serve(net.Listener)` and `Shutdown(ctx)`.
The caller owns the listener and closes it. The package ships only small CLI
tools for testing and postcat inspection.

**Rejected:** A `main` package with flags and config files. It would duplicate
every caller's deployment system.

**Consequences:** The caller must close the listener to unblock `Serve`. The
docs say this in S10. TLS-wrapped listeners work without extra API.

## D2. Expose four callbacks instead of a middleware chain

**Status:** accepted.

**Context:** A gateway needs a decision point at every transaction phase.

**Decision:** The `Handler` interface has `Hello`, `MailFrom`, `RcptTo`, and
`Data`. Each returns `*Response`. A nil response becomes 503.

**Rejected:** A plugin chain, an event bus, and middleware that could rewrite
the envelope. All three hide ordering and complicate concurrency guarantees.

**Consequences:** Users who need a pipeline compose it inside their handler.
The interface is small enough to implement in a test. A new phase is a breaking
change.

## D3. Three goroutines per connection, with an explicit takeover

**Status:** accepted.

**Context:** PIPELINING needs read-ahead. DATA, BDAT, and TLS need exclusive
access to the socket.

**Decision:** A reader parses commands into a buffered channel (depth 32). A
worker serialises processing. A watchdog closes the socket after shutdown. For
DATA, BDAT, and STARTTLS the reader blocks on `resumeCh` while the worker owns
the socket.

**Rejected:** A single goroutine (no pipelining). A separate body reader
goroutine (two readers on one socket). A semaphore around the socket (harder to
reason about than a direct handoff).

**Consequences:** Every worker return path must signal `resumeCh`. A missed
signal hangs the reader until the resume timeout. The invariant is clause C3.

## D4. Serial callbacks per connection, one handler across connections

**Status:** accepted.

**Context:** Users want per-message state without locking in the common case.

**Decision:** Callbacks for one connection run in command order on the worker
goroutine. The same handler instance is shared by all connections.

**Rejected:** A handler instance per connection (hides shared state, changes
the API). A handler per message (more allocation, no ordering benefit).

**Consequences:** Handlers with shared mutable state need a mutex. The docs
repeat this in three places because users miss it.

## D5. Setting `TLSConfig` requires TLS for MAIL

**Status:** provisional.

**Context:** The initial commit added a 530 response on MAIL when `TLSConfig` is
set and the connection is not encrypted. No decision entry existed before this
one.

**Decision:** Keep the current behaviour for now. Setting `TLSConfig` means
"require TLS", not "offer STARTTLS".

**Rejected (currently):** An opportunistic mode. It needs a new option and a
compatibility plan.

**Consequences:** The library cannot serve as a public MX with opportunistic
TLS. That conflicts with the README use case and with RFC 3207 §3.1. The gap is
X5 and the planned fix is a `RequireTLS` option with a false default. Until
then, docs must say TLS is required, not optional.

## D6. Postcat is a built-in, best-effort sink

**Status:** accepted.

**Context:** Users need a way to inspect mail before they wire a real backend.

**Decision:** When `PostcatDir` is set and `Handler.Data` returns 250, the
server writes one file per message. The write happens after the handler has
committed. A failure is logged and does not change the SMTP reply.

**Rejected:** Making postcat a required durable queue. That needs an fsync
contract, retention, and recovery, which belong to the caller's storage.

**Consequences:** The client can receive 250 while the file write fails. The
docs say so. Crash durability is weaker than the word "atomic" suggests
(X9).

## D7. No SMTP AUTH

**Status:** accepted.

**Context:** The target is inbound mail on port 25. MX delivery is
unauthenticated.

**Decision:** Do not implement AUTH. Point users to `mox` or `go-smtp` for
submission.

**Rejected:** A partial AUTH implementation. It would attract submission use
and require SASL, policy, and rate limiting.

**Consequences:** Access control lives in the handler: `Hello`, `MailFrom`, or
mutual TLS. The README says this.

## D8. Go 1.21 is the declared floor

**Status:** accepted, unverified.

**Context:** `go.mod` says `go 1.21`. CI uses 1.26.

**Decision:** Keep 1.21 because the code uses no newer language feature.
The newest standard-library feature is `log/slog`, added in 1.21.

**Rejected:** Raising the floor to 1.26 for a newer toolchain. It would drop
users for no code benefit.

**Consequences:** A dependency or a new API can raise the floor silently. CI
must test the declared version (X12).

## D9. Conventional Commits, with reason and references

**Status:** accepted.

**Context:** The history is read by release tooling and by agents.

**Decision:** Use `<type>(<scope>): <imperative summary>`. The body says why,
what was rejected, and what follows. Add `Refs:` with clause and decision IDs.
One logical change per commit.

**Rejected:** Free-form messages. They lose the link between a diff and the
requirement it serves.

**Consequences:** A reviewer can find the spec clause from the commit. A
commit that needs "and" in the subject is two commits.

## D10. `AGENTS.md` is the only agent guide

**Status:** accepted. Supersedes the `CLAUDE.md` guidance in the initial commit.

**Context:** Agents read different filenames. Duplicated instructions drift.

**Decision:** `AGENTS.md` is canonical. `CLAUDE.md` is a one-line pointer with a
Claude Code import. No other tool-specific instruction file exists. The spec,
not the agent guide, holds the rules the code must follow.

**Rejected:** A full copy of the guide in `CLAUDE.md`. Two copies disagree
within a month.

**Consequences:** A new agent needs one file to start. A tool that insists on
its own filename gets a pointer, not a fork.

## D11. Spec clauses and a gap register drive changes

**Status:** accepted.

**Context:** The code predates a written contract. Tests encoded behaviour that
contradicts the RFCs.

**Decision:** Every behaviour has a clause in `docs/spec.md` with a stable ID
and a status. A known difference is an `X` entry in §14, with evidence and a
fix direction. Code, tests, and spec change together. Tests that protect a gap
cite the `X` ID.

**Rejected:** A prose design document with no IDs. It cannot be referenced from
a commit, a test, or a lint rule.

**Consequences:** A green test suite no longer proves conformance. The gap
register is the honest part of the spec. Fixing a gap means changing a test
that currently passes.

## D12. The postcat format is Postfix-style, not Postfix-compatible

**Status:** accepted. Corrects the claim in the initial commit and the docs.

**Context:** The format borrows Postfix's `S`, `R`, and `T` record letters. It
writes `T` as RFC 3339 and uses a space after the record letter. Postfix queue
files carry offsets and pointer records; `postcat(1)` prints a human-readable
decoding of those files.

**Decision:** Describe the format as Postfix-style. Do not claim that Postfix
tools can read the files. Keep the current format until a user needs
interchange.

**Rejected:** Renaming the records or adding pointer records for a compatibility
that no test proves.

**Consequences:** The docs and the package comment must lose the word
"compatible" (X10). An interoperability test would be required before the claim
returns.

## D13. Keep both `NewServer` and struct literals

**Status:** accepted.

**Context:** The first API was `&Server{...}`. `NewServer` was added later with
functional options.

**Decision:** Support both. `NewServer` applies defaults eagerly. `Serve`
applies the same defaults to a struct literal.

**Rejected:** Removing struct literals. It breaks every existing user for a
small consistency gain.

**Consequences:** Defaults live in two places and must agree. Hostname
resolution timing differs (S12). Tests cover both paths only partially.

## D14. Return `*Response` rather than an error

**Status:** accepted.

**Context:** A handler decides the SMTP code, not just success or failure.

**Decision:** Callbacks return `*Response` with a code and message. A nil
response becomes 503. Transport errors are returned from `Serve`, not from
callbacks.

**Rejected:** `error` plus a separate code mapping. It cannot express 4xx versus
5xx, multi-line replies, or custom text without extra types.

**Consequences:** Handlers can return any 3-digit code, including an invalid
one. The server does not validate the code. The `Resp*` values are shared
pointers; callers must not mutate them.
