package smtpgateway

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// BenchmarkServerSequential measures single-connection throughput.
// Each iteration opens a fresh connection and completes a full SMTP
// transaction (EHLO → MAIL → RCPT → DATA → QUIT).  Use this as the
// latency baseline.
func BenchmarkServerSequential(b *testing.B) {
	srv := &Server{
		Hostname:    "test.local",
		Handler:     &acceptAllHandler{},
		ReadTimeout: 5 * time.Second,
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("listen: %v", err)
	}
	defer func() { _ = l.Close() }()
	go func() { _ = srv.Serve(l) }()
	addr := l.Addr().String()

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			b.Fatalf("dial: %v", err)
		}
		r := bufio.NewReader(conn)

		// Banner.
		line, err := r.ReadString('\n')
		if err != nil || !strings.HasPrefix(line, "220 ") {
			_ = conn.Close()
			b.Fatalf("banner: %q, %v", line, err)
		}

		// EHLO + drain multi-line.
		if _, err := conn.Write([]byte("EHLO bench\r\n")); err != nil {
			_ = conn.Close()
			b.Fatalf("ehlo write: %v", err)
		}
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				_ = conn.Close()
				b.Fatalf("ehlo read: %v", err)
			}
			if len(line) >= 4 && line[3] == ' ' {
				break
			}
		}

		// MAIL FROM.
		if _, err := conn.Write([]byte("MAIL FROM:<s@bench>\r\n")); err != nil {
			_ = conn.Close()
			b.Fatalf("mail write: %v", err)
		}
		if line, err := r.ReadString('\n'); err != nil || !strings.HasPrefix(line, "250 ") {
			_ = conn.Close()
			b.Fatalf("mail: %q, %v", line, err)
		}

		// RCPT TO.
		if _, err := conn.Write([]byte("RCPT TO:<r@bench>\r\n")); err != nil {
			_ = conn.Close()
			b.Fatalf("rcpt write: %v", err)
		}
		if line, err := r.ReadString('\n'); err != nil || !strings.HasPrefix(line, "250 ") {
			_ = conn.Close()
			b.Fatalf("rcpt: %q, %v", line, err)
		}

		// DATA.
		if _, err := conn.Write([]byte("DATA\r\n")); err != nil {
			_ = conn.Close()
			b.Fatalf("data write: %v", err)
		}
		if line, err := r.ReadString('\n'); err != nil || !strings.HasPrefix(line, "354 ") {
			_ = conn.Close()
			b.Fatalf("data: %q, %v", line, err)
		}

		// Body + terminator.
		if _, err := conn.Write([]byte("Subject: bench\r\n\r\nmsg\r\n.\r\n")); err != nil {
			_ = conn.Close()
			b.Fatalf("body write: %v", err)
		}
		if line, err := r.ReadString('\n'); err != nil || !strings.HasPrefix(line, "250 ") {
			_ = conn.Close()
			b.Fatalf("body resp: %q, %v", line, err)
		}

		// QUIT.
		if _, err := conn.Write([]byte("QUIT\r\n")); err != nil {
			_ = conn.Close()
			b.Fatalf("quit write: %v", err)
		}
		_, _ = r.ReadString('\n') // best-effort 221
		_ = conn.Close()
	}

	_ = l.Close()
	_ = srv.Shutdown(context.Background())
}

// BenchmarkServerConcurrent measures throughput under concurrent load.
// Each goroutine in RunParallel opens its own connection and runs a
// complete transaction.  Use this to find the point where contention
// on shared server state becomes measurable.
func BenchmarkServerConcurrent(b *testing.B) {
	srv := &Server{
		Hostname:    "test.local",
		Handler:     &acceptAllHandler{},
		ReadTimeout: 5 * time.Second,
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("listen: %v", err)
	}
	defer func() { _ = l.Close() }()
	go func() { _ = srv.Serve(l) }()
	addr := l.Addr().String()

	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			conn, err := net.Dial("tcp", addr)
			if err != nil {
				b.Errorf("dial: %v", err)
				return
			}
			r := bufio.NewReader(conn)

			// Banner.
			line, err := r.ReadString('\n')
			if err != nil || !strings.HasPrefix(line, "220 ") {
				_ = conn.Close()
				b.Errorf("banner: %q, %v", line, err)
				return
			}

			// EHLO + drain.
			if _, err := conn.Write([]byte("EHLO bench\r\n")); err != nil {
				_ = conn.Close()
				return
			}
			for {
				line, err := r.ReadString('\n')
				if err != nil {
					_ = conn.Close()
					return
				}
				if len(line) >= 4 && line[3] == ' ' {
					break
				}
			}

			// MAIL FROM → RCPT TO → DATA → body.
			_, _ = conn.Write([]byte("MAIL FROM:<s@bench>\r\n"))
			readOrDiscard(r)
			_, _ = conn.Write([]byte("RCPT TO:<r@bench>\r\n"))
			readOrDiscard(r)
			_, _ = conn.Write([]byte("DATA\r\n"))
			readOrDiscard(r)
			_, _ = conn.Write([]byte("Subject: bench\r\n\r\nmsg\r\n.\r\n"))
			readOrDiscard(r)
			_, _ = conn.Write([]byte("QUIT\r\n"))
			_ = conn.Close()
		}
	})

	_ = l.Close()
	_ = srv.Shutdown(context.Background())
}

// readOrDiscard reads a line from r and discards it.  Errors are
// silently ignored — the benchmark is measuring throughput, not
// correctness.
func readOrDiscard(r *bufio.Reader) {
	_, _ = r.ReadString('\n')
}
