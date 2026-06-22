# s3-gateway

SMTP gateway that stores accepted messages as S3 objects. Each message is
written in [Postfix `postcat(1)`](https://www.postfix.org/postcat.1.html)
format — envelope records followed by a blank line and the raw RFC 5322 body —
then gzip-compressed before upload. Envelope metadata is attached as S3 object
metadata for queryability.

Works with any S3-compatible storage: **AWS S3**, **Minio**, **Cloudflare R2**,
**DigitalOcean Spaces**, etc.

## Quick start with Minio (local dev)

Start a Minio container:

```bash
docker run -d --name minio \
  -p 9000:9000 -p 9001:9001 \
  -e MINIO_ROOT_USER=minioadmin \
  -e MINIO_ROOT_PASSWORD=minioadmin \
  minio/minio server /data --console-address :9001
```

Create a bucket at <http://localhost:9001> (or via `mc mb local/mail`).

```bash
go build -o s3-gateway .

S3_ENDPOINT=localhost:9000 \
S3_BUCKET=mail \
S3_REGION=us-east-1 \
S3_ACCESS_KEY=minioadmin \
S3_SECRET_KEY=minioadmin \
S3_USE_SSL=false \
S3_PATH_STYLE=true \
SMTP_LISTEN=:2525 \
./s3-gateway
```

Send a test message:

```bash
swaks --to alice@example.com --from sender@example.com \
  --server localhost:2525 --body "Hello from s3-gateway"
```

The message lands at `s3://mail/mail/YYYY/MM/DD/{unix}-{hash}.eml.gz`.

## Quick start with AWS S3

```bash
export AWS_PROFILE=my-account   # or set S3_ACCESS_KEY / S3_SECRET_KEY

S3_BUCKET=my-inbound-mail \
S3_REGION=eu-west-1 \
SMTP_LISTEN=:2525 \
./s3-gateway
```

When `S3_ACCESS_KEY` and `S3_SECRET_KEY` are unset, the SDK falls back to the
default credential chain (environment, instance profile, IAM role).

## Environment variables

| Variable | Default | Notes |
|---|---|---|
| `S3_ENDPOINT` | `s3.amazonaws.com` | S3-compatible endpoint |
| `S3_BUCKET` | *(required)* | Bucket name |
| `S3_REGION` | `us-east-1` | AWS region |
| `S3_PREFIX` | `mail` | Object key prefix (no trailing slash) |
| `S3_ACCESS_KEY` | — | Access key; omit for IAM / instance profile |
| `S3_SECRET_KEY` | — | Secret key; omit for IAM / instance profile |
| `S3_USE_SSL` | `true` | Set to `false` for local Minio |
| `S3_PATH_STYLE` | `false` | Set to `true` for Minio / path-style endpoints |
| `SMTP_LISTEN` | `:2525` | Listen address |
| `SMTP_HOSTNAME` | system hostname | Server hostname in SMTP banner |
| `SMTP_MAX_SIZE` | `26214400` (25 MiB) | Maximum message size in bytes |

## Object format

### Compression

Objects are gzip-compressed. A `sync.Pool` of `*gzip.Writer` at `BestSpeed`
avoids per-message allocations — email text is highly compressible (typically
5–10×) for negligible CPU overhead. S3 serves the original content when the
client sends `Accept-Encoding: gzip`, or you can decompress with `gzip -d`.

### Key layout

```
{prefix}/YYYY/MM/DD/{unix_timestamp}-{sha256[:8]}.eml.gz
```

Examples:

```
mail/2026/06/22/1750588800-a1b2c3d4.eml.gz
inbound/2026/01/01/1735689600-ff001122.eml.gz
```

The date-hierarchy layout works well with S3 lifecycle policies and Athena
partition projections.  The hash suffix is computed over the compressed content
and prevents collisions when multiple messages arrive in the same second.

### Object content (postcat format, gzip-compressed)

After decompression:

```
S sender@example.com
R alice@mydomain.com
R bob@mydomain.com
T 2026-06-22T14:30:05Z

Subject: Hello
From: sender@example.com
To: alice@mydomain.com

This is the raw RFC 5322 message body.
```

### S3 object metadata

| Metadata key | Content |
|---|---|
| `x-amz-meta-sender` | Envelope sender |
| `x-amz-meta-recipients` | Comma-separated accepted recipients |
| `x-amz-meta-helo` | Client HELO/EHLO domain |

Plus the standard S3 headers:

| Header | Value |
|---|---|
| `Content-Type` | `message/rfc822` |
| `Content-Encoding` | `gzip` |

## Customising

The handler in `main.go` accepts all senders and recipients.  To add your own
filtering, edit the `Hello`, `MailFrom`, and `RcptTo` methods:

```go
// Reject unauthorised senders.
func (h *s3Handler) MailFrom(_ context.Context, tx *smtpgateway.Tx) *smtpgateway.Response {
    if !strings.HasSuffix(tx.MailFrom, "@trusted.com") {
        return &smtpgateway.Response{550, "5.7.1 Sender not authorised"}
    }
    return smtpgateway.RespMailOK
}

// Accept only your own domains.
func (h *s3Handler) RcptTo(_ context.Context, tx *smtpgateway.Tx) *smtpgateway.Response {
    last := tx.Rcpts[len(tx.Rcpts)-1]
    if !strings.HasSuffix(last, "@mydomain.com") {
        return &smtpgateway.Response{550, "5.1.1 Relaying denied"}
    }
    return smtpgateway.RespRcptOK
}
```

See the [smtp-gateway README](../../README.md) for the full handler contract.

## Lifecycle policies

With the date-hierarchy key layout, you can expire old mail directly in S3:

```json
{
  "Rules": [
    {
      "Id": "expire-old-mail",
      "Status": "Enabled",
      "Prefix": "mail/",
      "Expiration": { "Days": 90 }
    }
  ]
}
```

## Testing with the CLI tools

Download an S3 object, decompress, and inspect it with `postcat`:

```bash
aws s3 cp s3://my-bucket/mail/2026/06/22/1750588800-a1b2c3d4.eml.gz /tmp/
gunzip /tmp/1750588800-a1b2c3d4.eml.gz
go run ../../cmd/postcat /tmp/1750588800-a1b2c3d4.eml
```

Or replay it through a test SMTP server:

```bash
gunzip -c /tmp/1750588800-a1b2c3d4.eml.gz | \
  go run ../../cmd/postcat-replay -addr :2525 /dev/stdin
```
