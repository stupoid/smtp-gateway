# smtp-gateway

SMTP gateway library for Go. Accept inbound email and route it anywhere —
S3, Kafka, the local filesystem, `/dev/null`, whatever.

## Install

```
go get github.com/stupoid/smtp-gateway
```

## Quick start

```go
package main

import (
	"context"
	"log/slog"
	"net"
	"os"
	"strings"

	"github.com/stupoid/smtp-gateway"
)

type myHandler struct{}

func (h *myHandler) Hello(ctx context.Context, tx *smtpgateway.Tx) *smtpgateway.Response {
	slog.Info("hello", "domain", tx.Helo)
	return smtpgateway.RespHelloOK
}

func (h *myHandler) MailFrom(ctx context.Context, tx *smtpgateway.Tx) *smtpgateway.Response {
	if tx.MailFrom == "spam@evil.com" {
		return &smtpgateway.Response{Code: 550, Message: "5.7.1 Go away"}
	}
	return smtpgateway.RespMailOK
}

func (h *myHandler) RcptTo(ctx context.Context, tx *smtpgateway.Tx) *smtpgateway.Response {
	last := tx.Rcpts[len(tx.Rcpts)-1]
	if !strings.Contains(last, "@mydomain.com") {
		return &smtpgateway.Response{Code: 550, Message: "5.1.1 Relaying denied"}
	}
	return smtpgateway.RespRcptOK
}

func (h *myHandler) Data(ctx context.Context, tx *smtpgateway.Tx, body []byte) *smtpgateway.Response {
	slog.Info("got message", "from", tx.MailFrom, "rcpts", tx.Accepted, "bytes", len(body))
	// Write to S3, Kafka, or wherever.
	return smtpgateway.RespDataOK
}

func main() {
	srv := smtpgateway.Server{
		Hostname: "mx.example.com",
		Handler:  &myHandler{},
	}

	ln, err := net.Listen("tcp", ":25")
	if err != nil {
		slog.Error("listen", "error", err)
		os.Exit(1)
	}
	defer ln.Close()

	slog.Info("listening", "addr", ln.Addr())
	if err := srv.Serve(ln); err != nil {
		slog.Error("serve", "error", err)
	}
}
```

## Handler contract

Your handler implements four callbacks, called **serially per connection**:

| Callback | When | What you get |
|---|---|---|
| `Hello` | After HELO/EHLO | `tx.Helo`, `tx.RemoteAddr` |
| `MailFrom` | After MAIL FROM | `tx.MailFrom`, `tx.Params` |
| `RcptTo` | Per RCPT TO | `tx.Rcpts` (all), `tx.Accepted` (accepted subset) |
| `Data` | After final `.` | Raw RFC 5322 body; only called if ≥1 rcpt accepted |

The same `Handler` instance handles all connections concurrently — use
`sync.Mutex` or channels if you hold mutable state.

### Common pitfalls

- **`nil` → 503.** Returning `nil` from any callback sends `503 Bad sequence`.
  Use the `Resp*` constants (`RespHelloOK`, `RespMailOK`, etc.) to accept.
- **STARTTLS needs `TLSConfig` set before `Serve`.** The EHLO banner is built
  once per connection, not lazily.
- **Don't mutate `tx`.** It's owned by the server; read only.

## Customising behaviour

| What you want | How to do it |
|---|---|
| Reject at HELO (auth, blocklist) | Return non-250 from `Handler.Hello` |
| Reject sender | Return non-250 from `Handler.MailFrom` |
| Accept/reject individual recipients | Return 250/non-250 from `Handler.RcptTo` |
| Store/reject message body | `Handler.Data` gets raw bytes |
| Enable STARTTLS | Set `Server.TLSConfig` |
| Enable SMTPS (implicit TLS) | Pass a `tls.Listener` to `Serve` |
| Limit message size | Set `Server.MaxMessageSize` |
| Limit recipients | Set `Server.MaxRecipients` |
| Limit connections | Set `Server.MaxConnections` |
| Set read/write/idle timeouts | Set `Server.ReadTimeout`, `WriteTimeout`, `IdleTimeout` |
| Quiet operation | Set `Server.Logger = nil` |
| Structured JSON logging | `Server.Logger = smtpgateway.Slog(slog.New(slog.NewJSONHandler(...)))` |
| Capture mail to disk | Set `Server.PostcatDir` |

## With STARTTLS

```go
cert, _ := tls.LoadX509KeyPair("cert.pem", "key.pem")

srv := smtpgateway.Server{
	Hostname:  "mx.example.com",
	Handler:   handler,
	TLSConfig: &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	},
}

ln, _ := net.Listen("tcp", ":587")
srv.Serve(ln)
```

For implicit TLS (SMTPS, port 465), wrap the listener:

```go
ln, _ := net.Listen("tcp", ":465")
srv.Serve(tls.NewListener(ln, tlsConfig))
```

## Protocol support

| Feature | Status |
|---|---|
| EHLO / HELO | ✅ |
| PIPELINING (RFC 2920) | ✅ |
| STARTTLS (RFC 3207) | ✅ |
| SMTPS (implicit TLS) | ✅ via `tls.Listener` |
| 8BITMIME | ✅ enforcement |
| ENHANCEDSTATUSCODES | ✅ |
| SMTPUTF8 | ✅ |
| SIZE | ✅ always advertised (default 25 MiB) |
| CHUNKING / BDAT (RFC 3030) | ✅ |
| Graceful shutdown | ✅ |

### No SMTP AUTH

This is an **inbound gateway** — it receives mail from the internet on port 25
and routes it to storage. MX delivery is unauthenticated by design.

For access control, use the callbacks you already have: check
`tx.RemoteAddr` in `Hello`, inspect `tx.MailFrom`, or use mutual TLS
(`tls.Config.ClientAuth`). If you need full submission with AUTH (port 587),
try [mox](https://github.com/mjl-/mox) or
[go-smtp](https://github.com/emersion/go-smtp).

## Postcat sink

Set `Server.PostcatDir` and every message lands as a flat file in
[Postfix-compatible](https://www.postfix.org/postcat.1.html) format. Handy
during setup before you wire up a real backend.

```go
srv := smtpgateway.Server{
	PostcatDir: "/var/spool/mail/incoming",
}
```

CLI tools ship in the repo: `cmd/postcat` reads individual files,
`cmd/postcat-replay` replays them through any SMTP server, and
`cmd/verify-postcat` does batch verification. The `internal/postcat` package
provides a `postcat.Parse(path)` function for programmatic access.

→ [Postcat guide](docs/postcat.md) (CLI usage, replay, programmatic API)

## Logging

Set `Server.Logger` to `smtpgateway.Slog(slog.Default())` for text output,
`nil` to discard, or implement the three-method interface:

```go
type Logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Error(msg string, args ...any)
}
```

Use `slog.String`, `slog.Int`, etc. as `args`. Control format via standard
`slog` handlers:

```go
// JSON to stderr
srv.Logger = smtpgateway.Slog(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
	Level: slog.LevelDebug,
})))

// Text to a file
f, _ := os.OpenFile("/var/log/mail.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
srv.Logger = smtpgateway.Slog(slog.New(slog.NewTextHandler(f, nil)))
```

Key events: `connection_opened`/`closed` (Debug), `smtp_recv`/`data_received`
(Info), `read_error`/`tls_handshake_error` (Error).

→ [Logging guide](docs/logging.md) (full event catalog, custom adapters)

## How it works

Each connection spawns two primary goroutines plus a cleanup goroutine:

1. **Reader** — reads lines, parses SMTP verbs, pushes into a buffered channel
   (depth 32). This enables RFC 2920 PIPELINING.
2. **Worker** — receives from the channel and dispatches to your handler
   callbacks.
3. **Cleanup** — waits for server shutdown, then force-closes the connection
   to unblock stuck reads.

During DATA and STARTTLS the reader **pauses** so the worker can take
exclusive control of the connection (reading the body, or performing the TLS
handshake). After RSET or successful DATA, transaction state resets.

→ [Full architecture](docs/architecture.md) (diagram, Tx fields, goroutine details)

## License

MIT
