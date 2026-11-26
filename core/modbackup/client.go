package modbackup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const defaultTokenFile = "/var/run/secrets/can/token" //nolint:gosec // a path, not a credential

// client is can-core's dump API (PUT|GET /v1/dumps/{module}/{name}).
type client struct {
	base, module, tokenFile string
	http                    *http.Client
	// backoff is the wait before the first retry; it doubles each time.
	backoff time.Duration
}

func newClient(getenv func(string) string) *client {
	return &client{base: strings.TrimRight(getenv("CAN_API_URL"), "/"), module: getenv("CAN_MODULE"),
		tokenFile: or(getenv("CAN_TOKEN_FILE"), defaultTokenFile), backoff: time.Second,
		http: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (c *client) check() error {
	u, err := url.Parse(c.base)
	if c.base == "" || err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("CAN_API_URL must be the can-core module API URL")
	}
	if c.module == "" {
		return errors.New("CAN_MODULE is required")
	}
	return nil
}

// tries covers the seconds in which a new pod's network rules are not programmed yet.
const tries = 6

// do sends one request built by build, retrying connection errors, 429 and 5xx. The caller closes
// the body of a successful response.
func (c *client) do(ctx context.Context, name string, build func(url string) (*http.Request, error)) (*http.Response, error) {
	token, err := os.ReadFile(c.tokenFile) //nolint:gosec // the projected token
	if err != nil {
		return nil, fmt.Errorf("read the token: %w", err)
	}
	target := c.base + "/v1/dumps/" + url.PathEscape(c.module) + "/" + url.PathEscape(name)
	wait := c.backoff
	for try := 1; ; try++ {
		req, err := build(target)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
		resp, err := c.http.Do(req)
		retry := err != nil
		if err == nil {
			if resp.StatusCode/100 == 2 {
				return resp, nil
			}
			detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			_ = resp.Body.Close()
			retry = resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
			err = fmt.Errorf("can-core answered %d: %s", resp.StatusCode, strings.TrimSpace(string(detail)))
		}
		if !retry || try == tries || ctx.Err() != nil {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
		wait *= 2
	}
}

// put streams the file; it is never read into memory.
func (c *client) put(ctx context.Context, name, path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	var open []*os.File
	defer func() {
		for _, f := range open {
			_ = f.Close()
		}
	}()
	resp, err := c.do(ctx, name, func(target string) (*http.Request, error) {
		f, err := os.Open(path) //nolint:gosec // our temporary file
		if err != nil {
			return nil, err
		}
		open = append(open, f)
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, target, f)
		if err != nil {
			return nil, err
		}
		req.ContentLength = fi.Size()
		req.Header.Set("Content-Type", "application/octet-stream")
		return req, nil
	})
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// get downloads one file of the module's own dump.
func (c *client) get(ctx context.Context, name, path string) error {
	resp, err := c.do(ctx, name, func(target string) (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	})
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // our temporary directory
	if err != nil {
		return err
	}
	_, err = io.Copy(f, io.LimitReader(resp.Body, 4<<30)) // the store's per-file cap
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
