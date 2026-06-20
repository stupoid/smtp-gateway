# smtp-gateway

SMTP gateway library for Go. Accept inbound email and route it anywhere.

The server calls your handler at each SMTP transaction phase — HELO, MAIL
FROM, RCPT TO, DATA — so you can accept, reject, or redirect messages to S3,
Kafka, the local filesystem, `/dev/null`, or anywhere else.

## Features

- **PIPELINING** — full RFC 2920 support with backpressure
- **STARTTLS** — bring your own `tls.Config`
- **Per-phase hooks** — inspect and reject at HELO, MAIL FROM, RCPT TO, DATA
- **Partial failure** — accept some recipients, reject others
- **Raw body** — receive the complete RFC 5322 message (headers + body) at DATA
- **Configurable responses** — set exact SMTP status codes from your handler
- **Graceful shutdown** — drain active connections
- **Postcat sink** — optional flat-file output in Postfix-compatible format;
  capture messages during setup before you wire up a real backend
- **Minimal dependencies** — standard library only
- **Concurrency** — one goroutine per connection with configurable limits

## Install

```
go get github.com/stupoid/smtp-gateway
```

## Quick start

```go
package main

import (
    "context"
    "crypto/tls"
    "log/slog"
    "net"
    "os"

    "github.com/stupoid/smtp-gateway"
)

type myHandler struct{}

func (h *myHandler) Hello(ctx context.Context, tx *smtpgateway.Tx) *smtpgateway.Response {
    slog.Info("hello", "domain", tx.Helo)
    return smtpgateway.RespHelloOK
}

func (h *myHandler) MailFrom(ctx context.Context, tx *smtpgateway.Tx) *smtpgateway.Response {
    if tx.MailFrom == "spam@evil.com" {
        return &smtpgateway.Response{550, "5.7.1 Go away"}
    }
    return smtpgateway.RespMailOK
}

func (h *myHandler) RcptTo(ctx context.Context, tx *smtpgateway.Tx) *smtpgateway.Response {
    last := tx.Rcpts[len(tx.Rcpts)-1]
    if !strings.Contains(last, "@mydomain.com") {
        return &smtpgateway.Response{550, "5.1.1 Relaying denied"}
    }
    return smtpgateway.RespRcptOK
}

func (h *myHandler) Data(ctx context.Context, tx *smtpgateway.Tx, body []byte) *smtpgateway.Response {
    slog.Info("got message",
        "from", tx.MailFrom,
        "recipients", tx.Accepted,
        "bytes", len(body),
    )
    // Write to S3, Kafka, or wherever.
    return smtpgateway.RespDataOK
}

func main() {
    srv := smtpgateway.Server{
        Hostname:       "mx.example.com",
        Handler:        &myHandler{},
        MaxMessageSize: 10 << 20, // 10 MiB
        MaxConnections: 100,
        ReadTimeout:    5 * time.Minute,
        WriteTimeout:   1 * time.Minute,
        IdleTimeout:    5 * time.Minute,
        Logger:         smtpgateway.Slog(slog.Default()),
        PostcatDir:     "/var/spool/mail/incoming",
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

## With STARTTLS

```go
cert, _ := tls.LoadX509KeyPair("cert.pem", "key.pem")

srv := smtpgateway.Server{
    Hostname:  "mx.example.com",
    Handler:   handler,
    TLSConfig: &tls.Config{
        Certificates: []tls.Certificate{cert},
    },
}

ln, _ := net.Listen("tcp", ":587")
srv.Serve(ln)
```

## Postcat sink

The `PostcatDir` option writes each accepted message as a flat file in
[Postfix `postcat(1)`](https://www.postfix.org/postcat.1.html) format.
It's a convenience sink — use it when you're setting up the gateway and
haven't decided on a final storage backend yet. Every file captures the
full envelope (sender, recipients, timestamp) and raw message body, so you
can replay or migrate them later without losing any data.

```go
srv := smtpgateway.Server{
    PostcatDir: "/var/spool/mail/incoming",
    // ... other fields
}
```

### CLI reader

A standalone `postcat` command reads individual files for testing and
inspection:

```
$ go build -o postcat ./cmd/postcat/
$ ./postcat $HOME/mail/incoming/1718832000-12345.eml
Sender:     sender@example.com
Recipients: [rcpt1@example.com rcpt2@example.com]
Time:       2024-06-20 09:30:00
Body size:  1024 bytes

--- Raw message ---
Subject: Test
...
```

### Programmatic access

```go
import "github.com/stupoid/smtp-gateway/internal/postcat"

msg, err := postcat.Parse("/path/to/file.eml")
fmt.Println("from:", msg.Sender)
fmt.Println("to:",   msg.Recipients)
fmt.Println("raw:",  string(msg.RawMessage))
```

See `cmd/verify-postcat/` for a batch verification tool that scans
directories.

## Handler contract

- **Hello** — called after HELO/EHLO. Reject to close the connection.
- **MailFrom** — called after MAIL FROM. `tx.MailFrom` and `tx.Params` are populated.
- **RcptTo** — called for each RCPT TO. Accept/reject individually.
  `tx.Rcpts` has all presented; `tx.Accepted` has the accepted subset.
- **Data** — called with the raw RFC 5322 message. Only called if ≥1
  recipient was accepted. Return non-250 to bounce.

Callbacks are **serialized per connection**. The same handler instance
handles multiple connections concurrently — make it goroutine-safe if it
needs mutable state.

## Protocol support

| Feature | Status |
|---------|--------|
| EHLO / HELO | ✅ |
| PIPELINING (RFC 2920) | ✅ |
| STARTTLS | ✅ |
| 8BITMIME | ✅ |
| SMTPUTF8 | ✅ |
| SIZE | ✅ |
| Graceful shutdown | ✅ |
| CHUNKING / BDAT (RFC 3030) | Planned |
| SMTP AUTH | Not yet |

## License

MIT
