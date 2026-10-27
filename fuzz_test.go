package smtpgateway

import (
	"bufio"
	"bytes"
	"testing"
)

// FuzzParseSMTPCommand covers the command splitter.  The parser must not
// panic on any input from the wire.
func FuzzParseSMTPCommand(f *testing.F) {
	for _, seed := range []string{
		"MAIL FROM:<a@b>\r\n",
		"HELO",
		"",
		" \tDATA x ",
		"RCPT TO:<>\r\n",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, line string) {
		_, _ = parseSMTPCommand(line)
	})
}

// FuzzParseMailFrom covers the MAIL FROM parser.
func FuzzParseMailFrom(f *testing.F) {
	for _, seed := range []string{
		"FROM:<a@b>",
		"FROM:<>",
		"FROM:<a@b> SIZE=10 BODY=8BITMIME SMTPUTF8",
		"",
		"x",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, args string) {
		_, _, _ = parseMailFrom(args)
	})
}

// FuzzParseRcptTo covers the RCPT TO parser.
func FuzzParseRcptTo(f *testing.F) {
	for _, seed := range []string{
		"TO:<a@b>",
		"TO:a@b",
		"",
		"junk",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, args string) {
		_ = parseRcptTo(args)
	})
}

// FuzzParseBdatArgs covers the BDAT argument parser.
func FuzzParseBdatArgs(f *testing.F) {
	for _, seed := range []string{
		"0",
		"10 LAST",
		"-1",
		"x",
		"1 2 3",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, args string) {
		_, _, _ = parseBdatArgs(args)
	})
}

// FuzzReadDotUnstuffed covers the body reader.  A zero timeout and a zero
// size limit avoid the connection deadline, so the test needs no socket.
func FuzzReadDotUnstuffed(f *testing.F) {
	for _, seed := range [][]byte{
		[]byte("body\r\n.\r\n"),
		[]byte(".\r\n"),
		[]byte("..x\r\n.\r\n"),
		[]byte(""),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = readDotUnstuffed(bufio.NewReader(bytes.NewReader(data)), 0, &connState{}, 0)
	})
}
