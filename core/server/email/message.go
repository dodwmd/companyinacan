package email

import (
	"bytes"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Content limits.
const (
	MaxSubjectBytes = 200
	MaxTextBytes    = 64 << 10
	MaxRecipients   = 10
)

// validSubject refuses anything that could end or fold a header line: every control character
// (CR, LF, NUL, tab ...) and the Unicode line and paragraph separators.
func validSubject(s string) bool {
	return s != "" && len(s) <= MaxSubjectBytes && utf8.ValidString(s) && !strings.ContainsFunc(s, func(r rune) bool {
		return unicode.IsControl(r) || r == ' ' || r == ' '
	})
}

// validText accepts plain UTF-8 text with line breaks and tabs.
func validText(s string) bool {
	return s != "" && len(s) <= MaxTextBytes && utf8.ValidString(s) && !strings.ContainsFunc(s, func(r rune) bool {
		return unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t'
	})
}

type message struct {
	ID, From, To, ReplyTo, Subject, Text string
	Date                                 time.Time
}

// build renders one text/plain message. Every header value has been validated by the caller or
// comes from can-core; the body is quoted-printable, so it cannot add headers or overlong lines.
// Dot-stuffing is done by net/smtp's data writer.
func (m message) build() []byte {
	var b bytes.Buffer
	_, domain, _ := strings.Cut(m.From, "@")
	hdr := func(k, v string) { fmt.Fprintf(&b, "%s: %s\r\n", k, v) }
	hdr("Date", m.Date.UTC().Format(time.RFC1123Z))
	hdr("From", "<"+m.From+">")
	hdr("To", "<"+m.To+">")
	if m.ReplyTo != "" {
		hdr("Reply-To", "<"+m.ReplyTo+">")
	}
	// Q-encoding yields encoded words of at most 75 characters; fold between them.
	hdr("Subject", strings.ReplaceAll(mime.QEncoding.Encode("utf-8", m.Subject), "?= =?", "?=\r\n =?"))
	hdr("Message-ID", "<"+m.ID+"@"+domain+">")
	hdr("Auto-Submitted", "auto-generated")
	hdr("MIME-Version", "1.0")
	hdr("Content-Type", "text/plain; charset=utf-8")
	hdr("Content-Transfer-Encoding", "quoted-printable")
	b.WriteString("\r\n")
	text := strings.ReplaceAll(strings.ReplaceAll(m.Text, "\r\n", "\n"), "\r", "\n")
	w := quotedprintable.NewWriter(&b) // writes "\n" as CRLF
	_, _ = w.Write([]byte(text))
	_ = w.Close()
	b.WriteString("\r\n")
	return b.Bytes()
}
