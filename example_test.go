package smtpgateway_test

import (
	"context"
	"fmt"

	smtpgateway "github.com/stupoid/smtp-gateway"
)

// Example demonstrates creating a Server with the NewServer constructor
// and functional options.
func Example() {
	h := &exampleHandler{}
	srv := smtpgateway.NewServer(
		h,
		smtpgateway.WithHostname("mx.example.com"),
		smtpgateway.WithMaxMessageSize(10<<20),
		smtpgateway.WithMaxRecipients(50),
	)
	fmt.Println(srv.Hostname)
	fmt.Println(srv.MaxMessageSize)
	fmt.Println(srv.MaxRecipients)
	// Output:
	// mx.example.com
	// 10485760
	// 50
}

type exampleHandler struct{}

func (h *exampleHandler) Hello(_ context.Context, tx *smtpgateway.Tx) *smtpgateway.Response {
	return smtpgateway.RespHelloOK
}
func (h *exampleHandler) MailFrom(_ context.Context, tx *smtpgateway.Tx) *smtpgateway.Response {
	return smtpgateway.RespMailOK
}
func (h *exampleHandler) RcptTo(_ context.Context, tx *smtpgateway.Tx) *smtpgateway.Response {
	return smtpgateway.RespRcptOK
}
func (h *exampleHandler) Data(_ context.Context, tx *smtpgateway.Tx, body []byte) *smtpgateway.Response {
	return smtpgateway.RespDataOK
}
