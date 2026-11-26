// Command mailsink is a TEST ONLY mail server: one IMAP mailbox over implicit TLS and an SMTP
// server with STARTTLS and AUTH, both in memory, so the support module's mail can be exercised end
// to end without a real provider (specs/059-support-mailbox).
//
// The IMAP side speaks only what FreeScout's client (webklex/php-imap) sends. The certificate is
// self-signed and made at start. The HTTP side, which has no authentication, injects incoming
// mail (POST /inbound) and shows what was sent (GET /sent) and what the mailbox holds
// (GET /mailbox). It is not part of any release and must never be deployed.
package main

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	keep        = 100
	maxMessage  = 256 << 10
	maxSessions = 32
	ioTimeout   = 2 * time.Minute
)

// Mail is a message the SMTP side accepted.
type Mail struct {
	From string   `json:"from"`
	To   []string `json:"to"`
	Data string   `json:"data"`
}

// Stored is a message in the IMAP mailbox.
type Stored struct {
	UID  int    `json:"uid"`
	Seen bool   `json:"seen"`
	Data string `json:"data"`
}

type sink struct {
	user, password string
	tls            *tls.Config

	mu     sync.Mutex
	sent   []Mail
	inbox  []*Stored
	uid    int
	logins int // successful IMAP logins
}

func (s *sink) deliver(raw []byte) int {
	data := strings.ReplaceAll(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n", "\r\n")
	s.mu.Lock()
	defer s.mu.Unlock()
	s.uid++
	s.inbox = append(s.inbox, &Stored{UID: s.uid, Data: data})
	if len(s.inbox) > keep {
		s.inbox = s.inbox[len(s.inbox)-keep:]
	}
	return s.uid
}

func (s *sink) handler() http.Handler {
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
	mux.HandleFunc("GET /sent", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		reply(w, append([]Mail{}, s.sent...))
	})
	mux.HandleFunc("GET /mailbox", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		msgs := []Stored{}
		for _, m := range s.inbox {
			msgs = append(msgs, *m)
		}
		reply(w, map[string]any{"logins": s.logins, "messages": msgs})
	})
	mux.HandleFunc("POST /inbound", func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxMessage))
		if err != nil || len(raw) == 0 {
			http.Error(w, "want a message of at most 256 KiB", http.StatusBadRequest)
			return
		}
		reply(w, map[string]int{"uid": s.deliver(raw)})
	})
	return mux
}

// conn is a line-based session that can be upgraded to TLS.
type conn struct {
	net.Conn
	r *bufio.Reader
}

func newConn(c net.Conn) *conn { return &conn{Conn: c, r: bufio.NewReaderSize(c, 8<<10)} }

func (c *conn) line() (string, error) {
	_ = c.SetDeadline(time.Now().Add(ioTimeout))
	l, err := c.r.ReadSlice('\n')
	if err != nil {
		return "", err // a line longer than the buffer ends the session
	}
	return strings.TrimRight(string(l), "\r\n"), nil
}

func (c *conn) printf(format string, args ...any) { _, _ = fmt.Fprintf(c.Conn, format, args...) }

// ---- SMTP --------------------------------------------------------------------------------------

// address takes the <...> of "MAIL FROM:<a@b>" or "RCPT TO:<a@b>".
func address(line string) string {
	_, rest, ok := strings.Cut(line, "<")
	if !ok {
		return ""
	}
	a, _, _ := strings.Cut(rest, ">")
	return a
}

func (s *sink) plainOK(b64 string) bool {
	b, err := base64.StdEncoding.DecodeString(b64)
	parts := strings.Split(string(b), "\x00")
	return err == nil && len(parts) == 3 && parts[1] == s.user && parts[2] == s.password
}

func (s *sink) loginDialogue(c *conn, first string) bool {
	read := func(prompt string) string {
		c.printf("334 %s\r\n", base64.StdEncoding.EncodeToString([]byte(prompt)))
		l, _ := c.line()
		b, _ := base64.StdEncoding.DecodeString(l)
		return string(b)
	}
	var user string
	if b, err := base64.StdEncoding.DecodeString(first); err == nil && first != "" {
		user = string(b)
	} else {
		user = read("Username:")
	}
	pass := read("Password:")
	return user == s.user && pass == s.password
}

func (s *sink) serveSMTP(raw net.Conn) {
	c := newConn(raw)
	defer func() { _ = c.Close() }()
	var secured, authed bool
	var m Mail
	c.printf("220 mailsink ESMTP\r\n")
	for {
		line, err := c.line()
		if err != nil {
			return
		}
		verb, arg, _ := strings.Cut(line, " ")
		switch strings.ToUpper(verb) {
		case "EHLO", "HELO":
			if secured {
				c.printf("250-mailsink\r\n250 AUTH PLAIN LOGIN\r\n")
			} else {
				c.printf("250-mailsink\r\n250 STARTTLS\r\n")
			}
		case "STARTTLS":
			if secured {
				c.printf("503 already secured\r\n")
				continue
			}
			c.printf("220 go ahead\r\n")
			t := tls.Server(c.Conn, s.tls)
			if err := t.Handshake(); err != nil {
				log.Printf("smtp: TLS handshake: %v", err)
				return
			}
			c, secured = newConn(t), true
		case "AUTH":
			if !secured {
				c.printf("538 encryption required\r\n")
				continue
			}
			mech, initial, _ := strings.Cut(arg, " ")
			switch strings.ToUpper(mech) {
			case "PLAIN":
				if initial == "" {
					c.printf("334 \r\n")
					initial, _ = c.line()
				}
				authed = s.plainOK(initial)
			case "LOGIN":
				authed = s.loginDialogue(c, initial)
			}
			if authed {
				c.printf("235 ok\r\n")
			} else {
				c.printf("535 authentication failed\r\n")
			}
		case "MAIL":
			if !authed {
				c.printf("530 authentication required\r\n")
				continue
			}
			m = Mail{From: address(line)}
			c.printf("250 ok\r\n")
		case "RCPT":
			if m.From == "" || len(m.To) >= 20 {
				c.printf("503 bad sequence\r\n")
				continue
			}
			m.To = append(m.To, address(line))
			c.printf("250 ok\r\n")
		case "DATA":
			if len(m.To) == 0 {
				c.printf("503 bad sequence\r\n")
				continue
			}
			c.printf("354 end with .\r\n")
			var b strings.Builder
			for {
				l, err := c.line()
				if err != nil || b.Len() > maxMessage {
					return
				}
				if l == "." {
					break
				}
				b.WriteString(strings.TrimPrefix(l, ".") + "\r\n")
			}
			m.Data = b.String()
			s.mu.Lock()
			s.sent = append(s.sent, m)
			if len(s.sent) > keep {
				s.sent = s.sent[len(s.sent)-keep:]
			}
			s.mu.Unlock()
			m = Mail{}
			c.printf("250 queued\r\n")
		case "RSET":
			m = Mail{}
			c.printf("250 ok\r\n")
		case "NOOP":
			c.printf("250 ok\r\n")
		case "QUIT":
			c.printf("221 bye\r\n")
			return
		default:
			c.printf("502 not implemented\r\n")
		}
	}
}

// ---- IMAP --------------------------------------------------------------------------------------

// words splits an IMAP command line into atoms, quoted strings and parenthesised lists (kept
// whole, with their parentheses).
func words(line string) []string {
	var out []string
	var b strings.Builder
	depth, quoted, has := 0, false, false
	flush := func() {
		if has {
			out = append(out, b.String())
			b.Reset()
			has = false
		}
	}
	for i := 0; i < len(line); i++ {
		ch := line[i]
		switch {
		case quoted && ch == '\\' && i+1 < len(line):
			i++
			b.WriteByte(line[i])
		case ch == '"':
			quoted, has = !quoted, true
			if depth > 0 {
				b.WriteByte(ch)
			}
		case quoted:
			b.WriteByte(ch)
		case ch == '(':
			depth, has = depth+1, true
			b.WriteByte(ch)
		case ch == ')':
			depth--
			b.WriteByte(ch)
		case ch == ' ' && depth == 0:
			flush()
		default:
			b.WriteByte(ch)
			has = true
		}
	}
	flush()
	return out
}

// set resolves a sequence set ("1", "2:4", "3:*", "1,5") against the UIDs or sequence numbers.
func (s *sink) set(spec string, byUID bool) []int {
	var out []int // indexes into inbox
	for i, m := range s.inbox {
		n, last := i+1, len(s.inbox)
		if byUID {
			n, last = m.UID, s.inbox[len(s.inbox)-1].UID
		}
		for _, part := range strings.Split(spec, ",") {
			lo, hi, ranged := strings.Cut(part, ":")
			num := func(v string) int {
				if v == "*" {
					return last
				}
				x, _ := strconv.Atoi(v)
				return x
			}
			a := num(lo)
			z := a
			if ranged {
				z = num(hi)
			}
			if n >= min(a, z) && n <= max(a, z) {
				out = append(out, i)
				break
			}
		}
	}
	return out
}

func splitMessage(data string) (header, body string) {
	if h, b, ok := strings.Cut(data, "\r\n\r\n"); ok {
		return h + "\r\n\r\n", b
	}
	return data, ""
}

func flags(m *Stored) string {
	if m.Seen {
		return `(\Seen)`
	}
	return `()`
}

// fetch answers FETCH for the items FreeScout asks for. A body item without .PEEK marks the
// message seen, as RFC 3501 says.
func (s *sink) fetch(c *conn, spec, items string, byUID bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, i := range s.set(spec, byUID) {
		m := s.inbox[i]
		header, body := splitMessage(m.Data)
		parts := []string{"UID " + strconv.Itoa(m.UID)}
		for _, item := range strings.Fields(strings.Trim(items, "()")) {
			name := strings.ToUpper(item)
			var value string
			switch name {
			case "UID":
				continue
			case "FLAGS":
				parts = append(parts, "FLAGS "+flags(m))
				continue
			case "RFC822.SIZE":
				parts = append(parts, "RFC822.SIZE "+strconv.Itoa(len(m.Data)))
				continue
			case "RFC822.HEADER", "BODY[HEADER]", "BODY.PEEK[HEADER]":
				value = header
			case "RFC822.TEXT", "BODY[TEXT]", "BODY.PEEK[TEXT]":
				value = body
			case "RFC822", "BODY[]", "BODY.PEEK[]":
				value = m.Data
			default:
				continue
			}
			if name == "RFC822" || name == "RFC822.TEXT" || name == "BODY[]" || name == "BODY[TEXT]" {
				m.Seen = true
			}
			answer := strings.Replace(name, ".PEEK", "", 1)
			parts = append(parts, fmt.Sprintf("%s {%d}\r\n%s", answer, len(value), value))
		}
		c.printf("* %d FETCH (%s)\r\n", i+1, strings.Join(parts, " "))
	}
}

// search supports what a mail fetch asks: UNSEEN, SEEN and ALL. Dates are not compared: every
// message here is new.
func (s *sink) search(c *conn, terms []string, byUID bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var unseen, seen bool
	for _, t := range terms {
		switch strings.ToUpper(t) {
		case "UNSEEN":
			unseen = true
		case "SEEN":
			seen = true
		}
	}
	line := "* SEARCH"
	for i, m := range s.inbox {
		if (unseen && m.Seen) || (seen && !m.Seen) {
			continue
		}
		n := i + 1
		if byUID {
			n = m.UID
		}
		line += " " + strconv.Itoa(n)
	}
	c.printf("%s\r\n", line)
}

func (s *sink) store(c *conn, spec, op, list string, byUID bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, i := range s.set(spec, byUID) {
		m := s.inbox[i]
		if strings.Contains(strings.ToLower(list), `\seen`) {
			m.Seen = !strings.HasPrefix(op, "-")
		}
		if !strings.Contains(strings.ToUpper(op), ".SILENT") {
			c.printf("* %d FETCH (UID %d FLAGS %s)\r\n", i+1, m.UID, flags(m))
		}
	}
}

func (s *sink) serveIMAP(raw net.Conn) {
	c := newConn(raw)
	defer func() { _ = c.Close() }()
	authed := false
	c.printf("* OK mailsink ready\r\n")
	for {
		line, err := c.line()
		if err != nil {
			return
		}
		w := words(line)
		if len(w) < 2 {
			c.printf("* BAD cannot read that\r\n")
			continue
		}
		tag, cmd, args := w[0], strings.ToUpper(w[1]), w[2:]
		byUID := false
		if cmd == "UID" && len(args) > 0 {
			byUID, cmd, args = true, strings.ToUpper(args[0]), args[1:]
		}
		ok := func() { c.printf("%s OK done\r\n", tag) }
		switch {
		case cmd == "CAPABILITY":
			c.printf("* CAPABILITY IMAP4rev1\r\n")
			ok()
		case cmd == "NOOP":
			ok()
		case cmd == "LOGOUT":
			c.printf("* BYE bye\r\n")
			ok()
			return
		case cmd == "LOGIN":
			if len(args) == 2 && args[0] == s.user && args[1] == s.password {
				authed = true
				s.mu.Lock()
				s.logins++
				s.mu.Unlock()
				ok()
			} else {
				c.printf("%s NO [AUTHENTICATIONFAILED] wrong user name or password\r\n", tag)
			}
		case !authed:
			c.printf("%s NO log in first\r\n", tag)
		case cmd == "LIST":
			c.printf("* LIST (\\HasNoChildren) \"/\" \"INBOX\"\r\n")
			ok()
		case cmd == "SELECT" || cmd == "EXAMINE":
			if len(args) < 1 || !strings.EqualFold(args[0], "INBOX") {
				c.printf("%s NO [NONEXISTENT] no such folder\r\n", tag)
				continue
			}
			s.mu.Lock()
			n, next := len(s.inbox), s.uid+1
			s.mu.Unlock()
			c.printf("* %d EXISTS\r\n* 0 RECENT\r\n* FLAGS (\\Seen)\r\n* OK [UIDVALIDITY 1] ok\r\n* OK [UIDNEXT %d] ok\r\n", n, next)
			ok()
		case cmd == "SEARCH":
			s.search(c, args, byUID)
			ok()
		case cmd == "FETCH" && len(args) >= 2:
			s.fetch(c, args[0], args[1], byUID)
			ok()
		case cmd == "STORE" && len(args) >= 3:
			s.store(c, args[0], args[1], strings.Join(args[2:], " "), byUID)
			ok()
		case cmd == "EXPUNGE" || cmd == "CLOSE":
			ok()
		default:
			log.Printf("imap: unsupported command %s", cmd)
			c.printf("%s BAD not implemented\r\n", tag)
		}
	}
}

// ---- main --------------------------------------------------------------------------------------

func selfSigned() (*tls.Config, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "mailsink"}, DNSNames: []string{"mailsink"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(7 * 24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}, nil
}

func serve(l net.Listener, handle func(net.Conn)) error {
	slots := make(chan struct{}, maxSessions)
	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		slots <- struct{}{}
		go func() {
			defer func() { <-slots }()
			handle(c)
		}()
	}
}

func run(smtpAddr, imapAddr, httpAddr, user, password string) error {
	if user == "" || password == "" {
		return errors.New("set MAILSINK_USER and MAILSINK_PASSWORD")
	}
	cfg, err := selfSigned()
	if err != nil {
		return err
	}
	s := &sink{user: user, password: password, tls: cfg}
	smtpL, err := net.Listen("tcp", smtpAddr) //nolint:noctx // test server
	if err != nil {
		return err
	}
	imapL, err := tls.Listen("tcp", imapAddr, cfg)
	if err != nil {
		return err
	}
	fail := make(chan error, 3)
	go func() { fail <- serve(smtpL, s.serveSMTP) }()
	go func() { fail <- serve(imapL, s.serveIMAP) }()
	web := &http.Server{Addr: httpAddr, Handler: s.handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { fail <- web.ListenAndServe() }()
	log.Printf("mailsink: smtp %s (STARTTLS), imap %s (TLS), http %s", smtpAddr, imapAddr, httpAddr)
	return <-fail
}

func main() {
	smtpAddr := flag.String("smtp", ":2525", "SMTP listener (STARTTLS)")
	imapAddr := flag.String("imap", ":9993", "IMAP listener (implicit TLS)")
	httpAddr := flag.String("http", ":8025", "HTTP listener")
	flag.Parse()
	if err := run(*smtpAddr, *imapAddr, *httpAddr, os.Getenv("MAILSINK_USER"), os.Getenv("MAILSINK_PASSWORD")); err != nil {
		log.Fatal(err)
	}
}
