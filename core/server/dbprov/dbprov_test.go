package dbprov

import (
	"context"
	"strings"
	"testing"
)

func TestNames(t *testing.T) {
	if n, err := Names("hr-portal"); err != nil || n != "mod_hr_portal" {
		t.Fatalf("%q %v", n, err)
	}
	for _, bad := range []string{"", "Crm", "a;drop", "a b", "-x", "x'y", strings.Repeat("a", 41)} {
		if _, err := Names(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestFakeIsIdempotentPerModule(t *testing.T) {
	f := NewFake()
	for i := 0; i < 2; i++ {
		c, err := f.Ensure(context.Background(), "postgres", "crm", "pw")
		if err != nil || c.Database != "mod_crm" || c.Port != 5432 {
			t.Fatalf("%+v %v", c, err)
		}
	}
	if len(f.Databases) != 1 {
		t.Fatalf("%v", f.Databases)
	}
}

func TestPostgresRefusesUnsafeInput(t *testing.T) {
	p := &Postgres{AdminURL: "postgres://127.0.0.1:1/x"}
	if _, err := p.Ensure(context.Background(), "mariadb", "crm", "abcdefghijklmnop12"); err == nil {
		t.Error("mariadb handled by the Postgres provisioner")
	}
	if _, err := p.Ensure(context.Background(), "postgres", "crm", "x'; DROP DATABASE x; --"); err == nil || !strings.Contains(err.Error(), "alphanumeric") {
		t.Errorf("unsafe password accepted: %v", err)
	}
	if _, err := p.Ensure(context.Background(), "postgres", "bad;id", "abcdefghijklmnop12"); err == nil {
		t.Error("unsafe module id accepted")
	}
}

func TestUnconfigured(t *testing.T) {
	if _, err := (Unconfigured{}).Ensure(context.Background(), "postgres", "crm", "x"); err == nil {
		t.Fatal("expected an error")
	}
}

func TestCredentialsURL(t *testing.T) {
	c := Credentials{Kind: "postgres", Host: "h", Port: 5432, Database: "d", User: "u", Password: "p"}
	if c.URL() != "postgres://u:p@h:5432/d?sslmode=disable" {
		t.Fatal(c.URL())
	}
}
