package sso

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
)

// Outbound call limits: every request to the identity provider is bounded in time and size.
const (
	callTimeout  = 10 * time.Second
	maxDiscBytes = 64 << 10
	maxJWKSBytes = 256 << 10
	maxTokenResp = 128 << 10
	maxInfoBytes = 64 << 10
)

// ErrSSRF is returned when a connection would go to a non-public address.
var ErrSSRF = errors.New("connection to a non-public address refused")

// NewHTTPClient builds the client used for every call to the identity provider. It does not use
// proxy environment variables, never follows a redirect (a 3xx answer is an error, so a provider
// URL cannot bounce the request to another host), and unless insecure it refuses to connect to
// loopback, private, link-local and other non-public addresses. The check runs on the address the
// socket is really about to connect to (after DNS resolution), which also defeats DNS rebinding.
func NewHTTPClient(insecure bool) *http.Client {
	d := &net.Dialer{Timeout: 5 * time.Second}
	if !insecure {
		d.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return ErrSSRF
			}
			if ip := net.ParseIP(host); ip == nil || !config.PublicIP(ip) {
				return ErrSSRF
			}
			return nil
		}
	}
	return &http.Client{
		Timeout: callTimeout,
		Transport: &http.Transport{
			Proxy:                  nil,
			DialContext:            d.DialContext,
			TLSHandshakeTimeout:    5 * time.Second,
			ResponseHeaderTimeout:  callTimeout,
			MaxIdleConns:           2,
			MaxIdleConnsPerHost:    1,
			IdleConnTimeout:        30 * time.Second,
			MaxResponseHeaderBytes: 16 << 10,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// call performs one request and decodes a JSON object answer of at most max bytes. Any status
// other than 200 is an error that carries no body text (bodies can echo credentials).
func call(ctx context.Context, c *http.Client, method, target string, form url.Values, basic [2]string, bearer string, max int64, out any) error {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if basic[0] != "" {
		// RFC 6749 2.3.1: the id and secret are form-urlencoded before Basic encoding.
		req.SetBasicAuth(url.QueryEscape(basic[0]), url.QueryEscape(basic[1]))
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("request to %s failed: %w", hostOf(target), unwrapURLErr(err))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("%s answered HTTP %d", hostOf(target), resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return fmt.Errorf("reading answer of %s: %w", hostOf(target), err)
	}
	if int64(len(b)) > max {
		return fmt.Errorf("answer of %s is larger than %d bytes", hostOf(target), max)
	}
	if raw, ok := out.(*[]byte); ok {
		*raw = b
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("answer of %s is not valid JSON", hostOf(target))
	}
	return nil
}

func hostOf(u string) string {
	if p, err := url.Parse(u); err == nil {
		return p.Host
	}
	return "the identity provider"
}

func unwrapURLErr(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
