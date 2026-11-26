package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/smtp"
	"strings"
	"testing"
	"time"
)

func TestSinkKeepsWhatItReceives(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	s := &sink{}
	go s.accept(ln)
	if err := smtp.SendMail(ln.Addr().String(), nil, "no-reply@acme.example", []string{"sales@acme.example"}, []byte("Subject: hi\r\n\r\n.dotted line\r\nbye\r\n")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(s.list()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	got := s.list()
	if len(got) != 1 || got[0].From != "no-reply@acme.example" || got[0].To[0] != "sales@acme.example" || !strings.Contains(got[0].Data, "\r\n.dotted line\r\n") {
		t.Fatalf("%+v", got)
	}
	srv := httptest.NewServer(s.handler())
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/messages", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusNoContent || len(s.list()) != 0 {
		t.Fatal(resp, err)
	}
	_ = resp.Body.Close()
}
