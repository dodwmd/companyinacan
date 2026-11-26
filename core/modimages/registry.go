package modimages

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Checker asks a registry, anonymously, whether it serves a reference. Nothing about the company
// is sent: only the repository and the tag.
type Checker struct{ Client *http.Client }

const manifestTypes = "application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, " +
	"application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json"

var challengeRe = regexp.MustCompile(`([a-z]+)="([^"]*)"`)

// Pullable reports whether ref can be pulled without credentials. false with a nil error means the
// registry answered that it cannot (not published, or private); an error means there was no answer.
func (c Checker) Pullable(ctx context.Context, ref string) (bool, error) {
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	host, rest, _ := strings.Cut(Normalize(ref), "/")
	repo, version, ok := strings.Cut(rest, "@")
	if !ok {
		i := strings.LastIndex(rest, ":")
		repo, version = rest[:i], rest[i+1:]
	}
	if host == "docker.io" {
		host = "registry-1.docker.io"
	}
	u := "https://" + host + "/v2/" + repo + "/manifests/" + url.PathEscape(version)
	head := func(token string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodHead, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", manifestTypes)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		_ = res.Body.Close()
		return res, nil
	}
	res, err := head("")
	if err != nil {
		return false, err
	}
	if res.StatusCode == http.StatusUnauthorized {
		token, err := anonymousToken(ctx, client, res.Header.Get("Www-Authenticate"))
		if err != nil || token == "" {
			return false, err
		}
		if res, err = head(token); err != nil {
			return false, err
		}
	}
	switch res.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound, http.StatusUnauthorized, http.StatusForbidden:
		return false, nil
	}
	return false, fmt.Errorf("%s answered %s", host, res.Status)
}

// anonymousToken follows a Bearer challenge. "" with no error: the registry refuses anonymous pulls.
func anonymousToken(ctx context.Context, client *http.Client, challenge string) (string, error) {
	if !strings.HasPrefix(challenge, "Bearer ") {
		return "", nil
	}
	p := map[string]string{}
	for _, m := range challengeRe.FindAllStringSubmatch(challenge, -1) {
		p[m[1]] = m[2]
	}
	u, err := url.Parse(p["realm"])
	if err != nil || u.Scheme != "https" {
		return "", errors.New("the registry's token address is not https")
	}
	q := u.Query()
	for _, k := range []string{"service", "scope"} {
		if p[k] != "" {
			q.Set(k, p[k])
		}
	}
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	res, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = res.Body.Close() }()
	switch res.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return "", nil
	default:
		return "", fmt.Errorf("the registry's token service answered %s", res.Status)
	}
	var t struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<16)).Decode(&t); err != nil {
		return "", fmt.Errorf("the registry's token answer: %w", err)
	}
	if t.Token == "" {
		return t.AccessToken, nil
	}
	return t.Token, nil
}
