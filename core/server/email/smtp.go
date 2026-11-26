package email

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/smtp"
	"net/textproto"
	"strconv"
	"syscall"
	"time"

	"github.com/dodwmd/companyinacan/core/server/netguard"
)

// Transport hands one message to a relay.
type Transport interface {
	Send(ctx context.Context, r Relay, to string, msg []byte) error
}

// SendError says where a delivery failed. Its text is built from fixed words and the numeric SMTP
// reply code only: server replies often repeat the recipient address or the credentials.
type SendError struct {
	Stage     string
	Code      int
	Permanent bool
	// Session: the failure is about reaching or logging in to the relay, so other messages would
	// fail the same way.
	Session bool
	// Stalled: the session was up and the relay gave no answer while taking this message. One message can
	// cause that; several different ones in a row mean the relay.
	Stalled bool
}

func (e *SendError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("%s: the relay answered %d", e.Stage, e.Code)
	}
	return e.Stage + " failed"
}

var errBlocked = errors.New("destination address is not allowed")

// metadataV6 is the IPv6 instance metadata service of AWS: a unique-local address, so "private
// relays allowed" would otherwise reach it.
var metadataV6 = netip.MustParseAddr("fd00:ec2::254")

// SMTP is the network transport.
type SMTP struct {
	// AllowPrivate permits relays on private and loopback addresses (CAN_SMTP_ALLOW_PRIVATE).
	// Link-local and cloud metadata addresses, multicast and unspecified ones are refused regardless.
	AllowPrivate bool
	// RootCAs overrides the system roots (tests).
	RootCAs *x509.CertPool
	// Timeout bounds one whole delivery (default 30s).
	Timeout time.Duration
}

// guard runs after name resolution, on the address actually dialled, so DNS rebinding does not help.
func (t SMTP) guard(network, address string, c syscall.RawConn) error {
	if !t.AllowPrivate {
		if netguard.DenyPrivate(network, address, c) != nil {
			return errBlocked
		}
		return nil
	}
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return errBlocked
	}
	ip := ap.Addr().Unmap()
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() || ip == metadataV6 {
		return errBlocked
	}
	return nil
}

func smtpErr(stage string, err error, rejectable bool) error {
	var pe *textproto.Error
	if errors.As(err, &pe) {
		// Only a refusal of this message (sender, recipient or content) is final. Anything about the
		// session (greeting, TLS, authentication) is the operator's to fix, so it is retried.
		// 530, 534, 535 and 538 ask for (other) authentication or TLS: the session again, whatever the
		// command they answer.
		auth := pe.Code == 530 || pe.Code == 534 || pe.Code == 535 || pe.Code == 538
		return &SendError{Stage: stage, Code: pe.Code, Permanent: rejectable && !auth && pe.Code >= 500, Session: !rejectable || auth || pe.Code == 421}
	}
	return &SendError{Stage: stage, Session: !rejectable, Stalled: rejectable}
}

// tlsErr says why TLS failed, in fixed words: the usual causes are the operator's to fix and
// "handshake failed" alone does not tell them apart.
func tlsErr(stage string, err error) error {
	var unknown x509.UnknownAuthorityError
	var host x509.HostnameError
	var invalid x509.CertificateInvalidError
	var record tls.RecordHeaderError
	cause := ""
	switch {
	case errors.As(err, &unknown):
		cause = " (the certificate is not signed by a trusted authority)"
	case errors.As(err, &host):
		cause = " (the certificate is for another host name)"
	case errors.As(err, &invalid) && invalid.Reason == x509.Expired:
		cause = " (the certificate is expired or not yet valid)"
	case errors.As(err, &invalid):
		cause = " (the certificate is not valid)"
	case errors.As(err, &record):
		cause = " (the relay did not answer with TLS: check the port and tls mode)"
	}
	return &SendError{Stage: stage + cause, Session: true}
}

// Send implements Transport. TLS is mandatory unless r.TLS is TLSNone (which Source only yields in
// development): a relay that does not offer STARTTLS gets nothing.
func (t SMTP) Send(ctx context.Context, r Relay, to string, msg []byte) error {
	timeout := t.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	d := &net.Dialer{Control: t.guard}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(r.Host, strconv.Itoa(r.Port)))
	if err != nil {
		if errors.Is(err, errBlocked) {
			return &SendError{Stage: "connect (the relay address is not a public address)", Session: true}
		}
		return &SendError{Stage: "connect", Session: true}
	}
	defer func() { _ = conn.Close() }()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	tc := &tls.Config{ServerName: r.Host, MinVersion: tls.VersionTLS12, RootCAs: t.RootCAs}
	if r.TLS == TLSImplicit {
		s := tls.Client(conn, tc)
		if err := s.HandshakeContext(ctx); err != nil {
			return tlsErr("TLS handshake", err)
		}
		conn = s
	}
	c, err := smtp.NewClient(conn, r.Host)
	if err != nil {
		return smtpErr("greeting", err, false)
	}
	defer func() { _ = c.Close() }()
	_, domain := splitAddr(r.From)
	if err := c.Hello(domain); err != nil {
		return smtpErr("EHLO", err, false)
	}
	switch r.TLS {
	case TLSStartTLS:
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return &SendError{Stage: "STARTTLS (the relay does not offer it)", Session: true}
		}
		if err := c.StartTLS(tc); err != nil {
			var pe *textproto.Error
			if errors.As(err, &pe) {
				return smtpErr("STARTTLS", err, false)
			}
			return tlsErr("STARTTLS", err)
		}
	case TLSImplicit, TLSNone:
	default:
		return &SendError{Stage: "configuration (unknown tls mode)", Session: true}
	}
	if r.Username != "" {
		if r.TLS == TLSNone {
			return &SendError{Stage: "authentication (refused without TLS)", Session: true}
		}
		// PlainAuth itself also refuses an unencrypted connection or another host name.
		if err := c.Auth(smtp.PlainAuth("", r.Username, r.Password, r.Host)); err != nil {
			return smtpErr("authentication", err, false)
		}
	}
	if err := c.Mail(r.From); err != nil {
		return smtpErr("MAIL FROM", err, true)
	}
	if err := c.Rcpt(to); err != nil {
		return smtpErr("RCPT TO", err, true)
	}
	w, err := c.Data()
	if err != nil {
		return smtpErr("DATA", err, true)
	}
	if _, err := w.Write(msg); err != nil {
		return smtpErr("DATA", err, true)
	}
	if err := w.Close(); err != nil {
		return smtpErr("DATA", err, true)
	}
	_ = c.Quit() // the relay has accepted the message; a failing QUIT must not send it twice
	return nil
}

func splitAddr(a string) (local, domain string) {
	for i := len(a) - 1; i >= 0; i-- {
		if a[i] == '@' {
			return a[:i], a[i+1:]
		}
	}
	return a, "localhost"
}
