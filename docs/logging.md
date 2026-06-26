# Logging guide

smtp-gateway logs at three levels: Debug, Info, and Error.  The `Logger`
interface follows `log/slog` conventions — all variadic args are key=value
pairs.

## Quick start

```go
import (
    "log/slog"
    "github.com/stupoid/smtp-gateway"
)

srv := &smtpgateway.Server{
    Logger: smtpgateway.Slog(slog.Default()),
}
```

Pass `nil` to discard all server logs.  The handler callbacks use the
standard library's `slog` directly — set `slog.SetDefault(...)` to
control both server and handler logging from one place.

## Custom adapters

Implement the `Logger` interface to send events to your own system:

```go
type Logger interface {
    Debug(msg string, args ...any)
    Info(msg string, args ...any)
    Error(msg string, args ...any)
}
```

The `args` are key=value pairs using `slog.String`, `slog.Int`,
`slog.Any`, etc.  Building your own adapter is a thin wrapper — see
`SlogAdapter` in `gateway.go` for the reference implementation.

## Event catalog

### Connection lifecycle (Debug)

| Event | Keys | Meaning |
|-------|------|---------|
| `connection_opened` | `remote` (string) | TCP connection accepted |
| `connection_closed` | `remote` (string) | `conn.Close()` returned |

### SMTP command traffic (Info)

| Event | Keys | Meaning |
|-------|------|---------|
| `smtp_recv` | `verb` (string), `args` (string, truncated to 120 chars) | Every SMTP command line received |

### Message reception (Info)

| Event | Keys | Meaning |
|-------|------|---------|
| `data_received` | `bytes` (int), `mail_from` (string), `recipients` (int) | DATA command body fully received |
| `bdat_received` | `bytes` (int), `mail_from` (string), `recipients` (int) | BDAT LAST chunk received, message complete |
| `8bit_rejected` | `mail_from` (string) | Message bounced because 8-bit data arrived without BODY=8BITMIME |

### Errors (Error)

| Event | Keys | Meaning |
|-------|------|---------|
| `banner_write_error` | `error` | Failed to write 220 banner |
| `read_error` | `error` | Reader goroutine read failure (timeout, EOF, buffer overflow) |
| `write_error` | `error` | Worker goroutine write failure |
| `handler_panic` | `panic` (any) | Panic recovered in worker goroutine |
| `reader_panic` | `panic` (any) | Panic recovered in reader goroutine |
| `ehlo_write_error` | `error` | Failed to write EHLO multi-line response |
| `tls_handshake_error` | `error` | STARTTLS handshake failed |
| `data_read_error` | `error` | Body read failed during DATA |
| `bdat_read_error` | `error` | Body read failed during BDAT chunk |
| `postcat_write_error` | `error`, `path` (string) | Failed to persist message to postcat dir |
| `resume_timeout` | — | Reader waited > bodyReadTimeout + 1 min for worker to finish DATA/STARTTLS/BDAT |
| `max_connections_reached` | — | New connection rejected because connSem is full |

### Startup (Info)

| Event | Keys | Meaning |
|-------|------|---------|
| `max_message_size` | `bytes` (int) | Configured MaxMessageSize emitted once at Serve() |

## Event flow for a typical transaction

```
connection_opened  (Debug, remote=1.2.3.4:56789)
smtp_recv          (Info,  verb=EHLO, args=mx.example.com)
smtp_recv          (Info,  verb=MAIL, args=FROM:<sender@example.com>)
smtp_recv          (Info,  verb=RCPT, args=TO:<recipient@example.com>)
smtp_recv          (Info,  verb=DATA, args=)
data_received      (Info,  bytes=1234, mail_from=sender@example.com, recipients=1)
connection_closed  (Debug, remote=1.2.3.4:56789)
```

If a postcat write fails after a successful DATA, `postcat_write_error`
(Error) appears after `data_received`.  The SMTP client still receives a
250 response — the message was accepted into the handler, the postcat
write is best-effort persistence.
