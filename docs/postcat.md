# Postcat format guide

Postcat files are the on-disk persistence format used by smtp-gateway. They
borrow the record letters from Postfix: `S` for sender, `R` for recipient, `T`
for time. The layout is Postfix-style, not the Postfix queue-file format.
Postfix tools expect the queue-file format, and `postcat(1)` does not produce
this layout.

The normative rules are clauses F1 to F10 in [spec.md](spec.md).

## References

- [Postfix postcat(1)](https://www.postfix.org/postcat.1.html): prints Postfix
  queue files in human-readable form. Related, not compatible
- [RFC 5321](https://datatracker.ietf.org/doc/html/rfc5321): SMTP envelope
- [RFC 5322](https://datatracker.ietf.org/doc/html/rfc5322): message format
- [RFC 3339](https://datatracker.ietf.org/doc/html/rfc3339): timestamp format

## File format

```
S <sender>\n
R <recipient>\n
  ...
T <RFC3339 timestamp>\n
\n
<raw RFC 5322 message>
```

- **S line**: envelope sender. The null sender is written as `S <>`.
- **R lines**: one per accepted recipient, in RCPT order. A message with no
  accepted recipient has no R line; the server does not write such a file.
- **T line**: local time at write, in RFC 3339 format.
- **Blank line**: separates the envelope from the body.
- **Body**: dot-unstuffed raw RFC 5322 bytes. No trailing CRLF is added.

CR and LF are stripped from addresses before writing. Without this, an address
containing `\nS attacker@evil.com` could inject a fake envelope record.

File names are `<unix>-<nanosecond>-<random>.eml`. Files are created with mode
0600. The directory is created with mode 0750 when `PostcatDir` is set.

## Durability

The writer uses `CreateTemp`, write, fsync, close, rename. The rename is atomic:
a reader sees the completed file or nothing. The writer also attempts a
directory sync. On a filesystem without directory sync, a power loss can still
lose the file even though the process returned success.

The server calls the writer after `Handler.Data` has returned 250. A write
failure is logged as `postcat_write_error` and does not change the SMTP reply.
Do not use this sink as a durable queue.

## CLI tools

### postcat: inspect a file

```
postcat <file.eml>
```

Prints the envelope and the raw message. Use for debugging or manual
inspection. `-h` prints usage.

### postcat-replay: re-inject into an SMTP server

```
postcat-replay -addr 127.0.0.1:2525 <file.eml>...
```

Reads postcat files, dials the target, and replays each message with EHLO, MAIL
FROM, RCPT TO, and DATA. The body is dot-stuffed and line endings are
normalised. The tool reads multi-line replies. It handles no STARTTLS.

`-addr` takes a `host:port` pair. It does not accept a URL scheme.

### verify-postcat: batch validation

```
verify-postcat <directory>
```

Scans every entry in the directory, parses it, and exits non-zero on the first
parse error. Use it as a health check on an archive.

## Programmatic API

```go
import "github.com/stupoid/smtp-gateway/internal/postcat"
```

The package is `internal`, so only code in this module can import it. If you
need it from another module, ask for an exported package.

### Writing

```go
path, err := postcat.Write("/var/spool/postcat", mailFrom, accepted, body)
```

Writes one message and returns the full path. The write is atomic at the
directory-entry level.

### Reading

```go
msg, err := postcat.Parse("/var/spool/postcat/1719446400-123456789-1a2b3c4d.eml")
fmt.Println(msg.Sender)      // envelope sender
fmt.Println(msg.Recipients)  // envelope recipients
fmt.Println(msg.Time)        // timestamp from the T record
fmt.Println(msg.RawMessage)  // raw RFC 5322 bytes
```

`Parse` reads the envelope up to the earliest blank line, so mixed line endings
are handled. A missing `T` record leaves `Time` zero. A malformed `T` record is
an error.

### Building envelope bytes

```go
raw := postcat.FormatEnvelope(mailFrom, accepted, time.Now(), body)
// raw is the complete postcat file; write to S3, gzip, etc.
```

`FormatEnvelope` produces the complete file in memory. It uses the same record
writer as `Write`, so the bytes match.

### Utility

```go
postcat.FormatNullSender("")                    // "<>"
postcat.FormatNullSender("<>")                  // "<>"
postcat.FormatNullSender("sender@example.com")  // "sender@example.com"
```
