// Command introstub is a DEV/E2E ONLY stand-in for can-core's token introspection endpoint.
//
// In the compose stack can-core cannot verify module tokens (no Kubernetes TokenReview), so the
// people and equity services only accept their one static service token. The end-to-end suite
// needs several callers with different roles (an employee, a manager, a board member), so the
// e2e override (dev/compose/e2e.yml) points those two services here. It answers the same
// protocol: POST {"token": "..."} -> {"active", "subject", "roles", "person_id"} from a fixed
// table of public fake tokens. It is NOT part of any release, listens on the compose network
// only, and must never be deployed.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

type entry struct {
	Subject  string   `json:"subject"`
	Roles    []string `json:"roles"`
	PersonID string   `json:"person_id,omitempty"`
}

func readTable(path string) (map[string]entry, error) {
	b, err := os.ReadFile(path) //nolint:gosec // dev tool, operator-supplied path
	if err != nil {
		return nil, err
	}
	var table map[string]entry
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&table); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return table, nil
}

func handler(table map[string]entry) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
	mux.HandleFunc("POST /", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Token string `json:"token"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&in); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		e, ok := table[in.Token]
		w.Header().Set("Content-Type", "application/json")
		if !ok {
			_, _ = io.WriteString(w, `{"active":false}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"active": true, "subject": e.Subject, "roles": e.Roles, "person_id": e.PersonID})
	})
	return mux
}

func main() {
	addr := flag.String("addr", ":8099", "listen address")
	file := flag.String("tokens", "/etc/introstub/tokens.json", "JSON object: token -> {subject, roles, person_id}")
	health := flag.String("healthcheck", "", "probe this address (host:port) and exit")
	flag.Parse()
	if *health != "" {
		c := http.Client{Timeout: 3 * time.Second}
		resp, err := c.Get("http://" + *health + "/healthz")
		if err != nil || resp.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		return
	}
	table, err := readTable(*file)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Fprintf(os.Stderr, "introstub (DEV/E2E ONLY): %d fake tokens on %s\n", len(table), *addr)
	srv := &http.Server{Addr: *addr, Handler: handler(table), ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
