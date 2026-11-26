package main

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"testing"
)

func testSink(t *testing.T) *sink {
	t.Helper()
	cfg, err := selfSigned()
	if err != nil {
		t.Fatal(err)
	}
	return &sink{user: "help@acme.example", password: `pw "with" $pace`, tls: cfg}
}

func listen(t *testing.T, handle func(net.Conn)) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() { _ = serve(l, handle) }()
	return l.Addr().String()
}

func TestSMTPNeedsTLSAndTheRightPassword(t *testing.T) {
	s := testSink(t)
	addr := listen(t, s.serveSMTP)
	send := func(password string) error {
		c, err := smtp.Dial(addr)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		if ok, _ := c.Extension("AUTH"); ok {
			t.Error("AUTH is offered before TLS")
		}
		if err := c.StartTLS(&tls.Config{InsecureSkipVerify: true}); err != nil { //nolint:gosec // self-signed test server
			t.Fatal(err)
		}
		if err := c.Auth(smtp.PlainAuth("", s.user, password, "127.0.0.1")); err != nil {
			return err
		}
		if err := c.Mail("help@acme.example"); err != nil {
			return err
		}
		if err := c.Rcpt("casey@customer.example"); err != nil {
			return err
		}
		w, err := c.Data()
		if err != nil {
			return err
		}
		_, _ = fmt.Fprint(w, "Subject: hi\r\n\r\n.dot\r\n")
		return w.Close()
	}
	if err := send("wrong"); err == nil {
		t.Error("a wrong password was accepted")
	}
	if err := send(s.password); err != nil {
		t.Fatal(err)
	}
	if len(s.sent) != 1 || s.sent[0].From != "help@acme.example" || !strings.Contains(s.sent[0].Data, "\r\n.dot\r\n") {
		t.Errorf("%+v", s.sent)
	}
}

func TestIMAPFetchAsFreeScoutDoes(t *testing.T) {
	s := testSink(t)
	s.deliver([]byte("Subject: hello\n\nbody\n"))
	client, server := net.Pipe()
	go s.serveIMAP(server)
	t.Cleanup(func() { _ = client.Close() })
	r := bufio.NewReader(client)
	read := func() string {
		l, err := r.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	read() // greeting
	n := 0
	ask := func(command string) string {
		n++
		tag := fmt.Sprintf("TAG%d", n)
		if _, err := fmt.Fprintf(client, "%s %s\r\n", tag, command); err != nil {
			t.Fatal(err)
		}
		var all strings.Builder
		for {
			l := read()
			all.WriteString(l)
			if strings.HasPrefix(l, tag+" ") {
				return all.String()
			}
		}
	}
	if got := ask(`SELECT "INBOX"`); !strings.Contains(got, "NO log in first") {
		t.Error("selected without signing in: " + got)
	}
	if got := ask(`LOGIN "help@acme.example" "pw \"with\" $pace"`); !strings.Contains(got, "OK") {
		t.Fatal(got)
	}
	for command, want := range map[string]string{
		`SELECT "INBOX"`:                        "* 1 EXISTS",
		`UID SEARCH UNSEEN SINCE "23-Nov-2026"`: "* SEARCH 1\r\n",
		`UID FETCH 1:1 (RFC822.HEADER)`:         "* 1 FETCH (UID 1 RFC822.HEADER {18}\r\nSubject: hello",
		`UID FETCH 1:* (UID)`:                   "* 1 FETCH (UID 1)",
	} {
		if got := ask(command); !strings.Contains(got, want) {
			t.Errorf("%s: %q lacks %q", command, got, want)
		}
	}
	ask(`UID STORE 1 +FLAGS.SILENT (\Seen)`)
	if got := ask(`UID SEARCH UNSEEN`); !strings.Contains(got, "* SEARCH\r\n") {
		t.Errorf("seen mail is still unseen: %q", got)
	}
}
