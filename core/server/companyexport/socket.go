package companyexport

// `can export` reaches the running can-core through a Unix socket next to the state database:
// `can-core export`, run with kubectl exec, copies the stream to its standard output. Nothing new
// listens on the network, and only someone who may exec into the pod can use it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	socketPath = "/export"
	// errorTrailer says why a stream that started did not finish.
	errorTrailer = "X-Export-Error"
	// Timeout bounds a whole export.
	Timeout = 12 * time.Hour
)

type socketRequest struct {
	IncludeSealed bool     `json:"include_sealed"`
	Only          []string `json:"only,omitempty"`
	Skip          []string `json:"skip,omitempty"`
}

// SocketActor is the audit actor of an export asked for on the command line.
const SocketActor = "cli"

// Listen opens the socket, replacing one a previous run left behind. Mode 0600: only the user
// can-core runs as.
func Listen(path string) (net.Listener, error) {
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = l.Close()
		return nil, err
	}
	return l, nil
}

// Serve answers on l until ctx ends.
func (s *Service) Serve(ctx context.Context, l net.Listener) error {
	srv := &http.Server{Handler: http.HandlerFunc(s.serveSocket), ReadHeaderTimeout: 10 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx }}
	stop := context.AfterFunc(ctx, func() { _ = srv.Close() })
	defer stop()
	if err := srv.Serve(l); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Service) serveSocket(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != socketPath {
		http.NotFound(w, r)
		return
	}
	var q socketRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&q); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), Timeout)
	defer cancel()
	req, err := s.Prepare(ctx, Options{IncludeSealed: q.IncludeSealed, Only: q.Only, Skip: q.Skip, Actor: SocketActor})
	switch {
	case errors.Is(err, ErrBusy):
		http.Error(w, err.Error(), http.StatusConflict)
		return
	case errors.Is(err, ErrBadRequest):
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	case err != nil:
		s.log().Error("export refused", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Trailer", errorTrailer)
	if _, err := req.Write(ctx, w); err != nil {
		w.Header().Set(errorTrailer, strings.Join(strings.Fields(err.Error()), " "))
	}
}

// Fetch asks the can-core listening on socket for an export and copies it to w. It is what
// `can-core export` runs.
func Fetch(ctx context.Context, socket string, o Options, w io.Writer) error {
	body, err := json.Marshal(socketRequest{IncludeSealed: o.IncludeSealed, Only: o.Only, Skip: o.Skip})
	if err != nil {
		return err
	}
	client := &http.Client{Transport: &http.Transport{DisableCompression: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		}}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://can-core"+socketPath, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("can-core does not answer on %s (is it running, and not in restore mode?): %w", socket, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return errors.New(strings.TrimSpace(string(b)))
	}
	if _, err := io.Copy(w, resp.Body); err != nil {
		return fmt.Errorf("the export broke off: %w", err)
	}
	if why := resp.Trailer.Get(errorTrailer); why != "" {
		return fmt.Errorf("the export failed: %s", why)
	}
	return nil
}
