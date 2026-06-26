// s3-gateway is an SMTP gateway server that stores accepted messages
// as S3 objects. Each message is written in Postfix postcat(1) format —
// envelope records followed by a blank line and the raw RFC 5322 body —
// gzip-compressed, with envelope metadata attached as S3 object metadata
// for queryability.
//
// The server works with any S3-compatible storage — AWS S3, Minio,
// DigitalOcean Spaces, Cloudflare R2, etc.
//
// Configuration is via environment variables:
//
//	S3_ENDPOINT      — S3-compatible endpoint (default: s3.amazonaws.com)
//	S3_BUCKET         — bucket name (required)
//	S3_REGION         — region (default: us-east-1)
//	S3_PREFIX         — object key prefix, no trailing slash (default: mail)
//	S3_ACCESS_KEY     — access key; if empty, uses IAM / instance profile
//	S3_SECRET_KEY     — secret key; if empty, uses IAM / instance profile
//	S3_USE_SSL        — "true" or "1" to use HTTPS (default: true)
//	S3_PATH_STYLE     — "true" or "1" for path-style addressing (needed for Minio)
//	SMTP_LISTEN       — listen address (default: :2525)
//	SMTP_HOSTNAME     — server hostname (default: system hostname)
//	SMTP_MAX_SIZE     — maximum message size in bytes (default: 25 MiB)
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	smtpgateway "github.com/stupoid/smtp-gateway"
	"github.com/stupoid/smtp-gateway/internal/postcat"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", os.Args[0], err)
		os.Exit(1)
	}
}

func run() error {
	// ---- configuration ----
	cfg := loadConfig()

	// ---- structured logger ----
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	// ---- S3 client ----
	client, err := newS3Client(cfg, logger)
	if err != nil {
		return fmt.Errorf("s3 client: %w", err)
	}

	// ---- listen ----
	ln, err := net.Listen("tcp", cfg.smtpListen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer func() { _ = ln.Close() }()

	// ---- handler ----
	h := &s3Handler{
		client: client,
		bucket: cfg.s3Bucket,
		prefix: cfg.s3Prefix,
		logger: logger,
	}

	maxSize := cfg.smtpMaxSize
	if maxSize == 0 {
		maxSize = 25 << 20 // 25 MiB
	}

	srv := &smtpgateway.Server{
		Hostname:       cfg.smtpHostname,
		Handler:        h,
		MaxMessageSize: maxSize,
		MaxConnections: 100,
		ReadTimeout:    5 * time.Minute,
		WriteTimeout:   1 * time.Minute,
		IdleTimeout:    5 * time.Minute,
		Logger:         smtpgateway.Slog(logger),
	}

	logger.Info("starting",
		"listen", cfg.smtpListen,
		"hostname", srv.Hostname,
		"bucket", cfg.s3Bucket,
		"prefix", cfg.s3Prefix,
		"endpoint", cfg.s3Endpoint,
		"max_size", srv.MaxMessageSize,
	)

	// ---- graceful shutdown ----
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
		sig := <-sigCh
		logger.Info("shutting down", "signal", sig.String())

		_ = ln.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			logger.Error("shutdown timeout", "error", err)
		}
	}()

	return srv.Serve(ln)
}

// ---- configuration ----

type config struct {
	s3Endpoint   string
	s3Bucket     string
	s3Region     string
	s3Prefix     string
	s3AccessKey  string
	s3SecretKey  string
	s3UseSSL     bool
	s3PathStyle  bool
	smtpListen   string
	smtpHostname string
	smtpMaxSize  int
}

func loadConfig() config {
	return config{
		s3Endpoint:   envDefault("S3_ENDPOINT", "s3.amazonaws.com"),
		s3Bucket:     os.Getenv("S3_BUCKET"),
		s3Region:     envDefault("S3_REGION", "us-east-1"),
		s3Prefix:     envDefault("S3_PREFIX", "mail"),
		s3AccessKey:  os.Getenv("S3_ACCESS_KEY"),
		s3SecretKey:  os.Getenv("S3_SECRET_KEY"),
		s3UseSSL:     envBool("S3_USE_SSL", true),
		s3PathStyle:  envBool("S3_PATH_STYLE", false),
		smtpListen:   envDefault("SMTP_LISTEN", ":2525"),
		smtpHostname: os.Getenv("SMTP_HOSTNAME"),
		smtpMaxSize:  envInt("SMTP_MAX_SIZE", 0),
	}
}

func newS3Client(cfg config, logger *slog.Logger) (*minio.Client, error) {
	if cfg.s3Bucket == "" {
		return nil, errors.New("S3_BUCKET is required")
	}

	opts := &minio.Options{
		Region: cfg.s3Region,
		Secure: cfg.s3UseSSL,
	}

	if cfg.s3AccessKey != "" && cfg.s3SecretKey != "" {
		opts.Creds = credentials.NewStaticV4(cfg.s3AccessKey, cfg.s3SecretKey, "")
		logger.Info("s3 using static credentials")
	} else {
		logger.Info("s3 using IAM / instance profile credentials")
	}

	if cfg.s3Endpoint != "s3.amazonaws.com" {
		// Custom endpoint — likely a non-AWS S3-compatible service.
		opts.BucketLookup = minio.BucketLookupPath
	} else if cfg.s3PathStyle {
		opts.BucketLookup = minio.BucketLookupPath
	}

	client, err := minio.New(cfg.s3Endpoint, opts)
	if err != nil {
		return nil, err
	}

	// Verify the bucket exists (and credentials work).
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	exists, err := client.BucketExists(ctx, cfg.s3Bucket)
	if err != nil {
		return nil, fmt.Errorf("bucket check: %w", err)
	}
	if !exists {
		return nil, fmt.Errorf("bucket %q does not exist", cfg.s3Bucket)
	}

	return client, nil
}

// ---- handler ----

// gzipWriterPool reuses gzip writers across messages.  BestSpeed gives
// most of the compression benefit at a fraction of the CPU cost — email
// is highly compressible text, so the trade-off is worth it.
var gzipWriterPool = sync.Pool{
	New: func() any {
		w, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed)
		return w
	},
}

// s3Handler implements smtpgateway.Handler.  Each accepted message is
// stored as a single S3 object.  The object content is gzip-compressed
// postcat format (envelope records + blank line + raw RFC 5322 body),
// and envelope metadata is attached as S3 object metadata for queryability.
//
// s3Handler is safe for concurrent use — PutObject calls are serialised
// by the library per connection, and the Minio client is goroutine-safe.
type s3Handler struct {
	client *minio.Client
	bucket string
	prefix string
	logger *slog.Logger
}

// Hello logs the client's HELO/EHLO domain and accepts.
func (h *s3Handler) Hello(_ context.Context, tx *smtpgateway.Tx) *smtpgateway.Response {
	h.logger.Info("hello", "domain", tx.Helo, "remote", tx.RemoteAddr)
	return smtpgateway.RespHelloOK
}

// MailFrom logs the envelope sender and accepts.
func (h *s3Handler) MailFrom(_ context.Context, tx *smtpgateway.Tx) *smtpgateway.Response {
	sender := tx.MailFrom
	if sender == "" {
		sender = "<>"
	}
	h.logger.Info("mail_from", "sender", sender)
	return smtpgateway.RespMailOK
}

// RcptTo accepts all recipients.  Implement your own filtering logic here
// — reject unauthorised domains, check blocklists, etc.
func (h *s3Handler) RcptTo(_ context.Context, tx *smtpgateway.Tx) *smtpgateway.Response {
	last := tx.Rcpts[len(tx.Rcpts)-1]
	h.logger.Info("rcpt_to", "recipient", last)
	return smtpgateway.RespRcptOK
}

// Data stores the message as a gzip-compressed S3 object in postcat format.
//
// Object key:  {prefix}/YYYY/MM/DD/{unix}-{sha256[:8]}.eml.gz
// This date-hierarchy layout works well with S3 lifecycle policies
// and Athena partition projections.
//
// The gzip writer is drawn from a sync.Pool to avoid per-message
// allocations.  BestSpeed compression typically shrinks text email
// by 5–10× for negligible CPU cost.
//
// On failure, it returns a transient (4xx) SMTP error so the client
// can retry.  If you want to bounce permanently, return a 5xx code.
func (h *s3Handler) Data(ctx context.Context, tx *smtpgateway.Tx, body []byte) *smtpgateway.Response {
	// 1. Build postcat-format content (uncompressed).
	now := time.Now()
	raw := postcat.FormatEnvelope(tx.MailFrom, tx.Accepted, now, body)

	// 2. Gzip-compress via pooled writer.
	gw := gzipWriterPool.Get().(*gzip.Writer)
	var gz bytes.Buffer
	gw.Reset(&gz)
	if _, err := io.Copy(gw, bytes.NewReader(raw)); err != nil {
		gzipWriterPool.Put(gw)
		h.logger.Error("gzip_failed", "error", err)
		return smtpgateway.RespSysError
	}
	if err := gw.Close(); err != nil {
		gzipWriterPool.Put(gw)
		h.logger.Error("gzip_close_failed", "error", err)
		return smtpgateway.RespSysError
	}
	gzipWriterPool.Put(gw)

	compressed := gz.Bytes()

	// 3. Build object key — hash the compressed content.
	hash := sha256.Sum256(compressed)
	key := objectKey(h.prefix, now, hex.EncodeToString(hash[:4]))

	// 4. Upload.
	opts := minio.PutObjectOptions{
		ContentType:     "message/rfc822",
		ContentEncoding: "gzip",
		UserMetadata: map[string]string{
			"sender":     postcat.FormatNullSender(tx.MailFrom),
			"recipients": strings.Join(tx.Accepted, ", "),
			"helo":       tx.Helo,
		},
	}

	_, err := h.client.PutObject(ctx, h.bucket, key, bytes.NewReader(compressed), int64(len(compressed)), opts)
	if err != nil {
		h.logger.Error("s3_put_failed",
			"error", err,
			"key", key,
			"sender", tx.MailFrom,
			"recipients", len(tx.Accepted),
		)
		return smtpgateway.RespSysError
	}

	h.logger.Info("stored",
		"key", key,
		"sender", postcat.FormatNullSender(tx.MailFrom),
		"recipients", len(tx.Accepted),
		"raw_bytes", len(raw),
		"gz_bytes", len(compressed),
	)
	return smtpgateway.RespDataOK
}

// objectKey returns an S3 object key in the form:
//
//	{prefix}/YYYY/MM/DD/{unix}-{hash}.eml.gz
//
// The date hierarchy enables range-based deletion via lifecycle policies
// and efficient prefix scans.
func objectKey(prefix string, t time.Time, hash string) string {
	return fmt.Sprintf("%s/%s/%d-%s.eml.gz",
		strings.TrimRight(prefix, "/"),
		t.Format("2006/01/02"),
		t.Unix(),
		hash,
	)
}


// ---- env helpers ----

func envDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	return v == "true" || v == "1"
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}
