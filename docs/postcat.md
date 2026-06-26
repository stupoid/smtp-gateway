# Postcat format guide

Postcat files are the on-disk persistence format used by smtp-gateway.
They follow the same layout as Postfix's
[postcat(1)](https://www.postfix.org/postcat.1.html) output: envelope
records, a blank separator line, and the raw RFC 5322 message body.

## References

- [Postfix postcat(1)](https://www.postfix.org/postcat.1.html) — the
  original format this implementation follows
- [RFC 5321](https://datatracker.ietf.org/doc/html/rfc5321) — SMTP
  (envelope, relay)
- [RFC 5322](https://datatracker.ietf.org/doc/html/rfc5322) — Internet
  Message Format (headers + body structure)

## File format

```
S <sender>\n
R <recipient>\n
  ...
T <RFC3339 timestamp>\n
\n
<raw RFC 5322 message>
```

- **S line** — envelope sender.  The null sender `<>` is normalised to
  `S <>`.
- **R lines** — one per accepted recipient, in the order RCPT TO was
  received.
- **T line** — server-local timestamp at write time, RFC 3339 format.
- **Blank line** — separates envelope metadata from the message body.
  The body is dot-unstuffed, raw RFC 5322 (headers + body).

Newlines in addresses are stripped before writing to prevent record
injection.

## CLI tools

### postcat — inspect a file

```
postcat <file.eml>
```

Prints the decoded envelope and body to stdout.  Use for debugging or
manual inspection.

### postcat-replay — re-inject into an SMTP server

```
postcat-replay [-addr smtp://127.0.0.1:2525] <file.eml>...
```

Reads postcat files, dials the target SMTP server, and replays each
message via standard SMTP commands (EHLO, MAIL FROM, RCPT TO, DATA).
Useful for testing handler logic against historical traffic or
re-delivering messages after a config change.

### verify-postcat — batch validation

```
verify-postcat <directory>
```

Scans a directory of postcat files and reports parse errors.  Use for
health checks on your postcat archive.

## Programmatic API

```go
import "github.com/stupoid/smtp-gateway/internal/postcat"
```

### Writing

```go
path, err := postcat.Write("/var/spool/postcat", mailFrom, accepted, body)
```

Atomically writes a message (CreateTemp + fsync + Rename).  The
filename is `<unix>-<nanosecond>-<random>.eml`.  Returns the full path
on success.

### Reading

```go
msg, err := postcat.Parse("/var/spool/postcat/1719446400-123456789-1a2b3c4d.eml")
fmt.Println(msg.Sender)      // envelope sender
fmt.Println(msg.Recipients)  // envelope recipients
fmt.Println(msg.Time)        // timestamp from T record
fmt.Println(msg.RawMessage)  // raw RFC 5322 body
```

### Building envelope bytes

```go
raw := postcat.FormatEnvelope(mailFrom, accepted, time.Now(), body)
// raw is the complete postcat-format []byte — write to S3, gzip, etc.
```

`FormatEnvelope` is the single authority for the wire format.  Use it
instead of hand-rolling S/R/T lines so format changes stay in one
place.

### Utility

```go
postcat.FormatNullSender("")   // "<>"
postcat.FormatNullSender("<>") // "<>"
postcat.FormatNullSender("sender@example.com") // "sender@example.com"
```
