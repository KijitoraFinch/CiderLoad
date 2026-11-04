package download

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"time"
)

// Fallback asks the caller to run the original curl invocation, after all
// workers and file handles have been stopped. The destination is untouched.
var Fallback = errors.New("use ordinary curl for this response")

type TimedFallback struct{ Deadline time.Time }

func (e *TimedFallback) Error() string { return Fallback.Error() }
func (e *TimedFallback) Unwrap() error { return Fallback }

type Error struct {
	Code       int
	Message    string
	Retry      bool
	RetryAfter time.Duration
	Cause      error
}

func (e *Error) Error() string { return e.Message }
func (e *Error) Unwrap() error { return e.Cause }

func failure(code int, message string, cause error) error {
	return &Error{Code: code, Message: message, Cause: cause}
}

func Classify(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{Code: 28, Message: "operation timed out", Retry: true, Cause: err}
	}
	if errors.Is(err, context.Canceled) {
		return &Error{Code: 42, Message: "operation canceled", Cause: err}
	}
	var cert *tls.CertificateVerificationError
	var unknown x509.UnknownAuthorityError
	if errors.As(err, &cert) || errors.As(err, &unknown) {
		return &Error{Code: 60, Message: "TLS certificate verification failed", Cause: err}
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return &Error{Code: 6, Message: "could not resolve host", Cause: err}
	}
	var op *net.OpError
	if errors.As(err, &op) {
		if op.Timeout() {
			return &Error{Code: 28, Message: "operation timed out", Retry: true, Cause: err}
		}
		if op.Op == "dial" {
			return &Error{Code: 7, Message: "could not connect to host", Cause: err}
		}
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return &Error{Code: 18, Message: "response ended before all bytes arrived", Cause: err}
	}
	var path *os.PathError
	if errors.As(err, &path) {
		return &Error{Code: 23, Message: fmt.Sprintf("file operation failed (%s): %v", path.Op, path.Err), Cause: err}
	}
	// net/url errors embed the full URL, which may contain credentials or a
	// signed query. Diagnostics intentionally exclude URLs and request headers.
	var ue *url.Error
	if errors.As(err, &ue) {
		return &Error{Code: 56, Message: "HTTP transfer failed", Cause: err}
	}
	return &Error{Code: 56, Message: err.Error(), Cause: err}
}
