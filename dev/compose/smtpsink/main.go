// Command smtpsink is a DEV/E2E ONLY SMTP server that keeps what it receives in memory instead of
// delivering it, so the compose stack can exercise email.send end to end without a real relay.
//
// It has no TLS and no authentication and shows every message over HTTP (GET /messages, DELETE
// /messages). It is not part of any release and must never be deployed.
package main

import (
	"bufio"
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
}

type sink struct {
	mu   sync.Mutex
	msgs []received
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

func (s *sink) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(time.Minute))
	// The limit bounds a whole session, so no line or message can grow without end.
	r := bufio.NewReader(io.LimitReader(c, 4*maxMessage))
	say := func(line string) { _, _ = io.WriteString(c, line+"\r\n") }
	say("220 smtpsink ESMTP (DEV ONLY)")
	var m received
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		verb, _, _ := strings.Cut(line, " ")
		switch strings.ToUpper(verb) {
		case "EHLO", "HELO":
			say("250 smtpsink")
		case "MAIL":
			m = received{From: address(line)}
			say("250 ok")
		case "RCPT":
			m.To = append(m.To, address(line))
			say("250 ok")
		case "DATA":
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
	go s.accept(ln)
	fmt.Fprintf(os.Stderr, "smtpsink (DEV/E2E ONLY): SMTP on %s, messages on %s/messages\n", *smtpAddr, *httpAddr)
	srv := &http.Server{Addr: *httpAddr, Handler: s.handler(), ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
