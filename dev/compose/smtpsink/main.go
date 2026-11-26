// Command smtpsink is a DEV/E2E ONLY SMTP server that keeps what it receives in memory instead of
// delivering it, so the compose stack can exercise email.send end to end without a real relay.
//
// By default it has no TLS and no authentication. -tls-cert and -tls-key add STARTTLS, and with
// SINK_USERNAME and SINK_PASSWORD set it takes mail only after AUTH PLAIN over TLS (the acceptance
// test runs it that way, so that the real relay path is exercised). It shows every message over HTTP
// (GET /messages, DELETE /messages). It is not part of any release and must never be deployed.
package main

import (
	"bufio"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	keep        = 200
	maxMessage  = 256 << 10
	maxSessions = 32
)

type received struct {
	From string   `json:"from"`
	To   []string `json:"to"`
	Data string   `json:"data"` // headers and body as sent (quoted-printable)
	TLS  bool     `json:"tls,omitempty"`
	Auth string   `json:"auth,omitempty"` // the user the session signed in as
}

type sink struct {
	mu   sync.Mutex
	msgs []received

	tls            *tls.Config // nil: no STARTTLS
	user, password string      // "": no AUTH needed
}

func (s *sink) add(m received) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs = append(s.msgs, m)
	if len(s.msgs) > keep {
		s.msgs = s.msgs[len(s.msgs)-keep:]
	}
}

func (s *sink) list() []received {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]received{}, s.msgs...)
}

func (s *sink) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
	mux.HandleFunc("GET /messages", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_ = json.NewEncoder(w).Encode(s.list())
	})
	mux.HandleFunc("DELETE /messages", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		s.msgs = nil
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

// address takes the <...> of "MAIL FROM:<a@b>" or "RCPT TO:<a@b>".
func address(line string) string {
	_, rest, ok := strings.Cut(line, "<")
	if !ok {
		return ""
	}
	a, _, _ := strings.Cut(rest, ">")
	return a
}

// readData reads up to the lone dot, undoing dot-stuffing.
func readData(r *bufio.Reader) (string, error) {
	var b strings.Builder
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return "", err
		}
		if line == ".\r\n" || line == ".\n" {
			return b.String(), nil
		}
		b.WriteString(strings.TrimPrefix(line, "."))
		if b.Len() > maxMessage {
			return "", errors.New("message too large")
		}
	}
}

// plainAuth checks the argument of AUTH PLAIN: base64 of "\x00user\x00password".
func (s *sink) plainAuth(arg string) bool {
	b, err := base64.StdEncoding.DecodeString(arg)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(b, []byte("\x00"+s.user+"\x00"+s.password)) == 1
}

func (s *sink) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(time.Minute))
	// The limit bounds a whole session, so no line or message can grow without end.
	r := bufio.NewReader(io.LimitReader(c, 4*maxMessage))
	var w io.Writer = c
	say := func(line string) { _, _ = io.WriteString(w, line+"\r\n") }
	say("220 smtpsink ESMTP (DEV ONLY)")
	var m received
	secure, authed := false, ""
	needAuth := func() bool {
		if s.user == "" || authed != "" {
			return false
		}
		say("530 authentication required")
		return true
	}
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		verb, arg, _ := strings.Cut(line, " ")
		switch strings.ToUpper(verb) {
		case "EHLO":
			switch {
			case s.tls != nil && !secure:
				say("250-smtpsink")
				say("250 STARTTLS")
			case s.user != "":
				say("250-smtpsink")
				say("250 AUTH PLAIN")
			default:
				say("250 smtpsink")
			}
		case "HELO":
			say("250 smtpsink")
		case "STARTTLS":
			if s.tls == nil || secure {
				say("502 not implemented")
				continue
			}
			say("220 go ahead")
			t := tls.Server(c, s.tls)
			if err := t.Handshake(); err != nil {
				return
			}
			// Nothing read before the handshake is kept.
			w, r = t, bufio.NewReader(io.LimitReader(t, 4*maxMessage))
			secure, m = true, received{}
		case "AUTH":
			mech, resp, _ := strings.Cut(arg, " ")
			if s.user == "" || !secure || !strings.EqualFold(mech, "PLAIN") || !s.plainAuth(resp) {
				say("535 authentication failed")
				continue
			}
			authed = s.user
			say("235 ok")
		case "MAIL":
			if needAuth() {
				continue
			}
			m = received{From: address(line), TLS: secure, Auth: authed}
			say("250 ok")
		case "RCPT":
			if needAuth() {
				continue
			}
			m.To = append(m.To, address(line))
			say("250 ok")
		case "DATA":
			if needAuth() {
				continue
			}
			say("354 end with a lone dot")
			data, err := readData(r)
			if err != nil {
				return
			}
			m.Data = data
			s.add(m)
			m = received{}
			say("250 kept")
		case "RSET":
			m = received{}
			say("250 ok")
		case "NOOP":
			say("250 ok")
		case "QUIT":
			say("221 bye")
			return
		default:
			say("502 not implemented")
		}
	}
}

// configure turns STARTTLS and AUTH on. Credentials are never taken without TLS.
func (s *sink) configure(certFile, keyFile, user, password string) error {
	if (certFile == "") != (keyFile == "") || (user == "") != (password == "") {
		return errors.New("-tls-cert needs -tls-key, and SINK_USERNAME needs SINK_PASSWORD")
	}
	if user != "" && certFile == "" {
		return errors.New("AUTH needs TLS: pass -tls-cert and -tls-key")
	}
	if certFile != "" {
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return err
		}
		s.tls = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	}
	s.user, s.password = user, password
	return nil
}

func (s *sink) accept(ln net.Listener) {
	slots := make(chan struct{}, maxSessions)
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		slots <- struct{}{}
		go func() {
			defer func() { <-slots }()
			s.serve(c)
		}()
	}
}

func main() {
	smtpAddr := flag.String("smtp", ":1025", "SMTP listen address")
	httpAddr := flag.String("http", ":8025", "HTTP listen address")
	health := flag.String("healthcheck", "", "probe this HTTP address (host:port) and exit")
	certFile := flag.String("tls-cert", "", "certificate file: offer STARTTLS")
	keyFile := flag.String("tls-key", "", "key file of -tls-cert")
	flag.Parse()
	if *health != "" {
		c := http.Client{Timeout: 3 * time.Second}
		resp, err := c.Get("http://" + *health + "/healthz")
		if err != nil || resp.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		return
	}
	ln, err := net.Listen("tcp", *smtpAddr)
	if err != nil {
		log.Fatal(err)
	}
	s := &sink{}
	if err := s.configure(*certFile, *keyFile, os.Getenv("SINK_USERNAME"), os.Getenv("SINK_PASSWORD")); err != nil {
		log.Fatal(err)
	}
	go s.accept(ln)
	fmt.Fprintf(os.Stderr, "smtpsink (DEV/E2E ONLY): SMTP on %s, messages on %s/messages\n", *smtpAddr, *httpAddr)
	srv := &http.Server{Addr: *httpAddr, Handler: s.handler(), ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
