package probe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"strings"
	"syscall"
)

// Classify maps a transport or assertion error to an error class.
//
// lastPhase is the phase the probe had reached, which disambiguates the two
// timeout flavours: a timeout before the first byte is a connect/timeout problem,
// a timeout after it is the server being slow.
func Classify(err error, lastPhase Phase) ErrorClass {
	if err == nil {
		return ClassOK
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return ClassDNSFail
	}

	var recordErr tls.RecordHeaderError
	if errors.As(err, &recordErr) {
		return ClassTLSHandshake
	}

	var certInvalid x509.CertificateInvalidError
	if errors.As(err, &certInvalid) {
		if certInvalid.Reason == x509.Expired {
			return ClassTLSExpired
		}
		return ClassTLSHandshake
	}
	var hostnameErr x509.HostnameError
	if errors.As(err, &hostnameErr) {
		return ClassTLSHandshake
	}
	var unknownAuth x509.UnknownAuthorityError
	if errors.As(err, &unknownAuth) {
		return ClassTLSHandshake
	}

	if errors.Is(err, syscall.ECONNREFUSED) {
		return ClassConnectRefused
	}
	if errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ENETUNREACH) {
		return ClassConnectTimeout
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return timeoutClass(lastPhase)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return timeoutClass(lastPhase)
	}

	// Fallbacks for wrapped messages the typed checks above miss.
	msg := err.Error()
	switch {
	case strings.Contains(msg, "certificate has expired"):
		return ClassTLSExpired
	case strings.Contains(msg, "tls:"):
		return ClassTLSHandshake
	}
	return ClassError
}

func timeoutClass(lastPhase Phase) ErrorClass {
	if lastPhase == PhaseTTFB || lastPhase == PhaseTotal {
		return ClassHTTPTimeout
	}
	return ClassConnectTimeout
}
