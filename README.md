# smtp-gateway

SMTP gateway library for Go. Accept inbound email and route it anywhere:
S3, Kafka, the local filesystem, `/dev/null`.

Status: pre-1.0. No tagged release yet, so `go get` resolves a pseudo-version.
The public API can change between commits. The normative behaviour is in
[docs/spec.md](docs/spec.md).

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
	srv := smtpgateway.NewServer(&myHandler{},
		smtpgateway.WithHostname("mx.example.com"),
	)

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

The caller owns the listener. `Serve` returns when the listener closes.
`Shutdown(ctx)` waits for in-flight connections; close the listener to unblock
`Serve`.

## Handler contract

Your handler implements four callbacks, called **serially per connection**:

| Callback | When | What you get |
|---|---|---|
| `Hello` | After HELO/EHLO | `tx.Helo`, `tx.RemoteAddr` |
| `MailFrom` | After MAIL FROM | `tx.MailFrom`, `tx.Params` |
| `RcptTo` | Per RCPT TO | `tx.Rcpts` (all), `tx.Accepted` (accepted subset) |
| `Data` | After final `.` or BDAT LAST | Raw RFC 5322 body; only called if at least one recipient was accepted |

The same `Handler` instance handles all connections concurrently. Use
`sync.Mutex` or channels if you hold mutable state. Callbacks for one connection
are serial, so per-connection state needs no lock.

### Common pitfalls

- **`nil` becomes 503.** Returning `nil` from any callback sends
  `503 5.5.1 Bad sequence of commands`. Use the `Resp*` constants to accept.
- **`TLSConfig` makes STARTTLS mandatory.** When `TLSConfig` is set, `MAIL`
  returns `530 5.7.0 STARTTLS required first` until the client upgrades. There
  is no opportunistic-TLS mode yet (gap X5). This is stricter than RFC 3207
  §3.1 requires for a public MX.
- **Don't mutate `tx`.** The server owns it; read only.
- **Copy the body if you keep it.** The server does not promise the slice's
  lifetime after the callback returns.
- **Configure before `Serve`.** Server fields are read concurrently by
  connections. Changing them after `Serve` starts is a data race.
- **`IdleTimeout` currently has no effect.** When `ReadTimeout` is set,
  `readLine` overwrites the idle deadline (gap X19).

## Customising behaviour

| What you want | Struct literal | NewServer option |
|---|---|---|
| Reject at HELO (auth, blocklist) | Return non-250 from `Handler.Hello` |
| Reject sender | Return non-250 from `Handler.MailFrom` |
| Accept/reject individual recipients | Return 250/non-250 from `Handler.RcptTo` |
| Store or reject the message body | `Handler.Data` gets the raw bytes |
| Enable STARTTLS (and require it) | `Server.TLSConfig` | `WithTLSConfig(cfg)` |
| Enable SMTPS (implicit TLS) | Pass a `tls.Listener` to `Serve` |
| Limit message size | `Server.MaxMessageSize` | `WithMaxMessageSize(n)` |
| Limit recipients | `Server.MaxRecipients` | `WithMaxRecipients(n)` |
| Limit connections | `Server.MaxConnections` | `WithMaxConnections(n)` |
| Set read/write/idle timeouts | `Server.ReadTimeout` etc. | `WithReadTimeout(d)` etc. |
| Quiet operation | `Server.Logger = nil` | `WithLogger(nil)` |
| Structured logging | `Server.Logger = smtpgateway.NewSlogLogger(slog.New(...))` | `WithLogger(smtpgateway.NewSlogLogger(slog.New(...)))` |
| Capture mail to disk | `Server.PostcatDir` | `WithPostcatDir(dir)` |

## With STARTTLS

```go
cert, _ := tls.LoadX509KeyPair("cert.pem", "key.pem")

srv := smtpgateway.NewServer(handler,
	smtpgateway.WithHostname("mx.example.com"),
	smtpgateway.WithTLSConfig(&tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}),
)

ln, _ := net.Listen("tcp", ":587")
srv.Serve(ln)
```

With `TLSConfig` set, the server advertises STARTTLS and **requires** it.
Plaintext `MAIL` gets 530. For an opportunistic setup, leave `TLSConfig` nil
until a require-TLS option lands (gap X5).

For implicit TLS (SMTPS, port 465), wrap the listener:

```go
ln, _ := net.Listen("tcp", ":465")
srv.Serve(tls.NewListener(ln, tlsConfig))
```

The SMTPS path works and `Tx.TLS` is populated.

## Protocol support

| Feature | Status |
|---|---|
| EHLO / HELO | Yes |
| PIPELINING (RFC 2920) | Yes |
| STARTTLS (RFC 3207) | Yes, required when configured (X5) |
| SMTPS (implicit TLS) | Yes via `tls.Listener`; `Tx.TLS` is populated |
| 8BITMIME | Advertised; enforced for DATA only |
| ENHANCEDSTATUSCODES | Advertised; the 354 reply has no 2.x.x code |
| SMTPUTF8 | Advertised; 8-bit bodies accepted with `SMTPUTF8` |
| SIZE | Always advertised (default 25 MiB) |
| CHUNKING / BDAT (RFC 3030) | Yes |
| Graceful shutdown | Yes; the caller closes the listener |
| Re-EHLO in one session | Rejected with 503 (X1) |

`README.md` claims and the code can differ. The spec wins.

### No SMTP AUTH

This is an **inbound gateway**. It receives mail from the internet on port 25
and routes it to storage. MX delivery is unauthenticated by design.

For access control, use the callbacks: check `tx.RemoteAddr` in `Hello`, inspect
`tx.MailFrom`, or use mutual TLS (`tls.Config.ClientAuth`). If you need
submission with AUTH (port 587), use [mox](https://github.com/mjl-/mox) or
[go-smtp](https://github.com/emersion/go-smtp).

## Postcat sink

Set `PostcatDir` and every accepted message lands as one flat file. The format
is Postfix-style: envelope records, a blank line, then the raw message. It is
not the Postfix queue-file format, and it is not intended for Postfix tools.

```go
srv := smtpgateway.NewServer(handler,
	smtpgateway.WithPostcatDir("/var/spool/mail/incoming"),
)
```

The write is best effort. The handler has already returned 250, so a disk
failure is logged and the client still gets 250. Do not use this sink where
durability matters.

CLI tools ship in the repo: `cmd/postcat` reads individual files,
`cmd/postcat-replay` replays them through any SMTP server, and
`cmd/verify-postcat` does batch verification. The `internal/postcat` package
provides `postcat.Parse(path)` for programmatic access.

→ [Postcat guide](docs/postcat.md)
→ [Handler guide](docs/handler-guide.md)

## Logging

Set `Server.Logger` to `smtpgateway.NewSlogLogger(slog.Default())` for text
output, `nil` to discard, or implement the three-method interface:

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
srv.Logger = smtpgateway.NewSlogLogger(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
	Level: slog.LevelDebug,
})))

// Text to a file
f, _ := os.OpenFile("/var/log/mail.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
srv.Logger = smtpgateway.NewSlogLogger(slog.New(slog.NewTextHandler(f, nil)))
```

Key events: `connection_opened` / `connection_closed` (Debug), `smtp_recv` /
`data_received` (Info), `read_error` / `tls_handshake_error` (Error).

`smtp_recv` logs every command with its arguments at Info, including envelope
addresses. Treat the log stream as personal data (gap X17).

→ [Logging guide](docs/logging.md) (full event catalog, custom adapters)

## How it works

Each connection runs three goroutines:

1. **Reader**: reads lines, parses SMTP verbs, and pushes commands into a
   buffered channel (depth 32). This is what enables PIPELINING.
2. **Worker**: receives from the channel and dispatches to your callbacks.
3. **Watchdog**: waits for shutdown, then closes the connection to unblock a
   stuck body read or TLS handshake.

During DATA, BDAT, and STARTTLS the reader **pauses** so the worker can take
exclusive control of the connection. After RSET or a completed transaction, the
transaction state resets.

→ [Architecture](docs/architecture.md)
→ [Specification](docs/spec.md)

## Documentation

| Document | Purpose |
|---|---|
| [docs/spec.md](docs/spec.md) | Normative clauses and known gaps |
| [docs/architecture.md](docs/architecture.md) | How the code implements the spec |
| [docs/decisions.md](docs/decisions.md) | Why the design is what it is |
| [docs/handler-guide.md](docs/handler-guide.md) | Phase-by-phase handler walkthrough |
| [docs/postcat.md](docs/postcat.md) | Postcat format and CLI tools |
| [docs/logging.md](docs/logging.md) | Log event catalog |
| [CONCEPTS.md](CONCEPTS.md) | Vocabulary |
| [AGENTS.md](AGENTS.md) | Guide for contributors and coding agents |

## Contributing

Read `AGENTS.md`. Run `make check` before you commit. A behaviour change needs a
spec clause, a test, and the documents in the same commit.

## License

MIT
