# Concepts

Shared vocabulary for this project. This is a glossary, not a spec. The rules
live in [docs/spec.md](docs/spec.md). Use these terms in code, comments, logs,
and documents. Do not rotate synonyms.

## Core components

### Server

The `Server` value. It accepts connections from a `net.Listener` and dispatches
each one to a Handler. It owns configuration: hostname, TLS, timeouts, message
size and recipient limits, connection cap, logger, postcat directory. One Server
handles many concurrent connections. See clause group S.

### Handler

The interface a user implements to decide what happens at each transaction
phase. Four callbacks: `Hello`, `MailFrom`, `RcptTo`, `Data`. Serial per
connection, concurrent across connections. One Handler instance serves every
connection, so mutable state must be safe. See clause group H.

### Session

One TCP connection, from the 220 banner to the close. A session may contain zero
or more transactions. The code calls this a connection.

### Transaction

One mail delivery. It starts with MAIL, contains one or more RCPT commands, and
ends with DATA, `BDAT ... LAST`, or RSET. The code and the documents use
"transaction" for this, never "session".

### Tx

The per-transaction state passed to the handler: HELO domain, envelope sender,
MAIL parameters, presented recipients, accepted and rejected subsets, remote
address, and TLS state. The server owns it and replaces it after RSET and after
a completed transaction. Handlers read it; they do not mutate it.

### Response

The SMTP reply a handler returns: a three-digit code and a message. The `Resp*`
values are shared pointers. Do not mutate them; call `Copy()` when you need a
different message.

## SMTP processing

### Envelope

The SMTP envelope: MAIL FROM and RCPT TO. It controls routing and delivery. The
RFC 5322 headers inside DATA are payload and are not part of the envelope.
Postcat files record the envelope before the message body.

### Envelope sender

The MAIL FROM address. An empty string means the null sender, written as `<>`
in SMTP and in postcat files.

### Connection takeover

The worker taking direct control of the socket while the reader goroutine is
paused on `resumeCh`. It happens for DATA, BDAT, and STARTTLS. The pause
prevents two goroutines from racing for bytes on one socket. See clause C3.

### Pipelining

Sending several SMTP commands without waiting for each reply (RFC 2920). The
reader queues commands in a buffered channel (depth 32) while the worker
processes them in order. DATA, BDAT, and STARTTLS stop read-ahead.

### Phase

The transaction stage used for command sequencing: init, HELO received, MAIL
received, RCPT received. `phase` is separate from `gotHelo`. RSET clears the
phase but keeps the HELO state.

### BDAT / CHUNKING

RFC 3030 message transfer in counted chunks instead of dot-stuffed lines. A
non-LAST chunk gets 250. The LAST chunk calls `Handler.Data` with the
concatenated body. CHUNKING does not imply BINARYMIME; they are separate
extensions.

### STARTTLS

RFC 3207 upgrade of a plaintext session to TLS. The server advertises it only
when `TLSConfig` is set. Setting `TLSConfig` currently makes TLS mandatory for
MAIL; see gap X5.

### SMTPS

Implicit TLS. The caller wraps the listener in `tls.Listener` and passes it to
`Serve`. `Tx.TLS` is populated after the banner write completes the lazy
handshake.

## Persistence

### Postcat

A flat-file format for persisting an accepted message. It borrows Postfix's
record letters: `S` sender, `R` recipient, `T` timestamp. A blank line separates
the envelope from the raw RFC 5322 message. It is Postfix-style, not the Postfix
queue-file format, and it is not intended for Postfix tools. The `internal/postcat`
package provides `Write`, `Parse`, and `FormatEnvelope`. CLI tools:
`cmd/postcat`, `cmd/postcat-replay`, `cmd/verify-postcat`.

### Best-effort write

The server writes the postcat file after the handler returns 250. The client has
already received 250, so a write failure is logged and the reply does not
change. Do not confuse this with durability.

## Project process

These terms describe how this repository is maintained. They are not SMTP
concepts.

### Clause

A numbered requirement in `docs/spec.md`, such as `P4` or `H1`. Clause IDs are
stable and never renumbered. A commit that changes behaviour cites the clause.

### Gap

A known difference between a clause and the code, recorded in `docs/spec.md`
§14 as an `X` entry. A gap is a bug with evidence, not a permanent exception.

### Decision

An entry in `docs/decisions.md`, such as `D5`. It records why a choice was made.
Accepted entries are never rewritten; a later entry supersedes them.

### Agent guide

`AGENTS.md`. The single entry point for contributors and coding agents.
`CLAUDE.md` is a pointer to it, not a second copy.

## Flagged ambiguities

- "Transaction" was once used for a whole connection. The project settled on
  "session" for the connection and "transaction" for one delivery.
- "Postfix-compatible" was used for the postcat format. It is not compatible.
  Use "Postfix-style".
- "TLS", "STARTTLS", and "SMTPS" are not synonyms. TLS is the protocol,
  STARTTLS is the upgrade command, and SMTPS is implicit TLS on connect.
