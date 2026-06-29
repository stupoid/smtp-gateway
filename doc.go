// Package smtpgateway provides an SMTP server library with per-phase hooks.
// Users implement the Handler interface to accept or reject mail at each
// SMTP transaction phase and route message data to arbitrary sinks.
//
// # Handler contract
//
// Your handler implements four callbacks, called serially per connection but
// concurrently across connections. One Handler instance is shared across all
// connections — make it goroutine-safe if it holds mutable state.
//
//	Hello(ctx, tx)    — called after HELO/EHLO
//	MailFrom(ctx, tx)  — called after MAIL FROM
//	RcptTo(ctx, tx)    — called once per RCPT TO
//	Data(ctx, tx, body) — called after DATA or BDAT LAST
//
// Key invariants:
//
//   - Return nil from any callback → server sends 503 Bad sequence. Use the
//     Resp* constants (RespHelloOK, RespMailOK, etc.) to accept.
//   - Callbacks are serial per connection — Hello → MailFrom → RcptTo* → Data
//     proceed sequentially from the worker loop.
//   - tx is read-only for callbacks. The server manages Tx fields; handlers
//     read them, they do not mutate them.
//   - Data is only called if at least one recipient was accepted. If all
//     RCPT TOs are rejected, Data is skipped.
//   - After RSET or successful DATA, tx is replaced with a fresh Tx.
//
// # Transaction lifecycle
//
// A Tx accumulates state across the phases of a single SMTP transaction:
//
//	New connection →  HELO/EHLO  →  tx.Helo populated, Hello callback
//	After HELO    →  MAIL FROM  →  tx.MailFrom + tx.Params populated, MailFrom callback
//	After MAIL    →  RCPT TO    →  tx.Rcpts appended, tx.Accepted/rejected split, RcptTo callback
//	After RCPT    →  DATA/BDAT  →  body delivered, Data callback (if accepted recipients > 0)
//	              →  RSET       →  tx reset, back to start
//
// See docs/handler-guide.md for a detailed walkthrough with a state diagram.
package smtpgateway
