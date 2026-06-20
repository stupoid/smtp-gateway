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
- **Postcat output** — optional built-in sink that writes Postfix-compatible
  queue files (readable by `postcat`)
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

## Write to postcat directory

Set `PostcatDir` and accepted messages are automatically written in a format
compatible with Postfix `postcat(1)`:

```go
srv.PostcatDir = "/var/spool/mail/incoming"
```

Then parse them later:

```go
msg, err := smtpgateway.ParsePostcat("/var/spool/mail/incoming/1718832000-12345.eml")
fmt.Println("from:", msg.Sender)
fmt.Println("to:",   msg.Recipients)
fmt.Println("raw:",  string(msg.RawMessage))
```

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
