# Concepts

Shared domain vocabulary for this project — entities, named processes, and status concepts with project-specific meaning. Seeded with core domain vocabulary, then accretes as ce-compound and ce-compound-refresh process learnings; direct edits are fine. Glossary only, not a spec or catch-all.

## Core Components

### Server

The SMTP gateway server that accepts connections from a `net.Listener` and dispatches each to a Handler. Owns all runtime configuration: hostname, TLS, timeouts, message size and recipient limits, connection caps, and graceful shutdown via `Shutdown`. A single Server instance handles many concurrent connections.

### Handler

The interface users implement to decide what happens at each SMTP transaction phase. Four callbacks — `Hello`, `MailFrom`, `RcptTo`, `Data` — are called serially per connection but concurrently across connections. One Handler instance is shared across all connections, so mutable state must be goroutine-safe.

## SMTP Processing

### Transaction (Tx)

The state accumulated across the phases of a single SMTP transaction: HELO domain, envelope sender, MAIL FROM parameters, all presented recipients, and the split between accepted and rejected recipients. Reset on RSET or after a successful DATA/BDAT completion. The server owns the Tx; handlers read it, they do not mutate it.

### Envelope

The SMTP envelope — the MAIL FROM sender and RCPT TO recipients — as distinct from the RFC 5322 message headers carried inside DATA. The envelope determines routing and delivery; the headers are opaque payload. Postcat files record the envelope explicitly before the message body.

### Connection Takeover

When the worker goroutine takes direct control of the network connection from the reader goroutine during DATA and STARTTLS. The reader pauses (blocks on a resume channel) while the worker reads the dot-stuffed body or performs the TLS handshake directly on the raw connection. This prevents the reader and worker from racing for bytes on the same socket.

## Persistence

### Postcat

A flat-file format for persisting accepted messages to disk, compatible with Postfix's `postcat(1)` output. Each file contains envelope records (S for sender, R for each recipient, T for timestamp), a blank separator line, and the raw RFC 5322 message body. The `internal/postcat` package provides `Write`, `Parse`, and `FormatEnvelope`; CLI tools (`cmd/postcat`, `cmd/postcat-replay`, `cmd/verify-postcat`) provide inspection, replay, and batch verification.

## Flagged Ambiguities

- "Transaction" had been used interchangeably with "session" — the project settled on Transaction (Tx) as the canonical term. A session spans the TCP connection; a transaction spans one mail delivery (HELO through DATA/RSET).
