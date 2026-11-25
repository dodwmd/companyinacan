package files

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// S3Options configures the S3 backend.
type S3Options struct {
	Endpoint, Bucket, Region, Prefix string
	KeyID, Secret                    string
	// AllowInsecure permits an http:// endpoint (a local MinIO in development). The default
	// refuses it: the signed request would otherwise travel in clear.
	AllowInsecure bool
	HTTP          *http.Client
	Now           func() time.Time
}

// S3Backend is a minimal path-style S3 client (PUT, GET, DELETE and HEAD of single objects with
// AWS Signature V4) without an SDK. The signed payload hash is the file's own SHA-256, which
// the service already knows, so an upload streams from the staged file with a known length and
// S3 itself rejects a body that does not match. Server-side encryption is the bucket's setting.
// Copied in miniature from core/backup's S3Store, which it deliberately does not import.
type S3Backend struct {
	base   *url.URL
	o      S3Options
	client *http.Client
}

// NewS3Backend validates the options.
func NewS3Backend(o S3Options) (*S3Backend, error) {
	base, err := url.Parse(strings.TrimRight(o.Endpoint, "/"))
	if err != nil || base.Host == "" || base.User != nil || (base.Scheme != "https" && (base.Scheme != "http" || !o.AllowInsecure)) {
		return nil, errors.New("s3: the endpoint must be an https URL")
	}
	if o.Bucket == "" || !bucketRe.MatchString(o.Bucket) {
		return nil, errors.New("s3: invalid bucket name")
	}
	if len(o.KeyID) < 1 || len(o.Secret) < 1 {
		return nil, errors.New("s3: access key id and secret are required (CAN_FILES_S3_ACCESS_KEY_ID, CAN_FILES_S3_SECRET_ACCESS_KEY)")
	}
	if !prefixRe.MatchString(o.Prefix) {
		return nil, errors.New("s3: invalid prefix")
	}
	if o.Region == "" {
		o.Region = "us-east-1"
	}
	c := o.HTTP
	if c == nil {
		// No overall timeout (a download streams for as long as it needs); the dial and the
		// response header are bounded, and every call carries the caller's context.
		c = &http.Client{
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 60 * time.Second,
				MaxIdleConns:          2,
				IdleConnTimeout:       30 * time.Second,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	return &S3Backend{base: base, o: o, client: c}, nil
}

func hmacSHA(key []byte, s string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(s))
	return m.Sum(nil)
}

const emptySHA = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func (b *S3Backend) objectPath(key string) string {
	return strings.TrimRight(b.base.Path, "/") + "/" + b.o.Bucket + "/" + b.o.Prefix + shard(key)
}

// do signs and sends one request. payload is the hex SHA-256 of the body.
func (b *S3Backend) do(ctx context.Context, method, path string, body io.Reader, size int64, payload string) (*http.Response, error) {
	uri := (&url.URL{Path: path}).EscapedPath()
	req, err := http.NewRequestWithContext(ctx, method, b.base.Scheme+"://"+b.base.Host+uri, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = size
	}
	now := time.Now
	if b.o.Now != nil {
		now = b.o.Now
	}
	t := now().UTC()
	amzDate, day := t.Format("20060102T150405Z"), t.Format("20060102")
	canon := strings.Join([]string{
		method, uri, "",
		"host:" + b.base.Host + "\nx-amz-content-sha256:" + payload + "\nx-amz-date:" + amzDate + "\n",
		"host;x-amz-content-sha256;x-amz-date", payload,
	}, "\n")
	scope := day + "/" + b.o.Region + "/s3/aws4_request"
	ch := sha256.Sum256([]byte(canon))
	sts := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(ch[:])
	k := hmacSHA(hmacSHA(hmacSHA(hmacSHA([]byte("AWS4"+b.o.Secret), day), b.o.Region), "s3"), "aws4_request")
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payload)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+b.o.KeyID+"/"+scope+
		", SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature="+hex.EncodeToString(hmacSHA(k, sts)))
	resp, err := b.client.Do(req)
	if err != nil {
		// the transport's error names the URL, never a credential, but say less anyway
		return nil, errors.New("s3: the request failed")
	}
	return resp, nil
}

func s3Err(op string, resp *http.Response) error {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return fmt.Errorf("s3 %s: HTTP %d", op, resp.StatusCode)
}

// Put implements Backend.
func (b *S3Backend) Put(ctx context.Context, key string, r io.Reader, size int64, sha string) error {
	if !blobKeyRe.MatchString(key) {
		return errors.New("invalid blob key")
	}
	resp, err := b.do(ctx, http.MethodPut, b.objectPath(key), io.LimitReader(r, size), size, sha)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return s3Err("put", resp)
	}
	return nil
}

// Open implements Backend.
func (b *S3Backend) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	if !blobKeyRe.MatchString(key) {
		return nil, ErrNotFound
	}
	resp, err := b.do(ctx, http.MethodGet, b.objectPath(key), nil, 0, emptySHA)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		_ = resp.Body.Close()
		return nil, ErrNotFound
	case resp.StatusCode != http.StatusOK:
		defer func() { _ = resp.Body.Close() }()
		return nil, s3Err("get", resp)
	}
	return resp.Body, nil
}

// Delete implements Backend.
func (b *S3Backend) Delete(ctx context.Context, key string) error {
	if !blobKeyRe.MatchString(key) {
		return nil
	}
	resp, err := b.do(ctx, http.MethodDelete, b.objectPath(key), nil, 0, emptySHA)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusNotFound {
		return s3Err("delete", resp)
	}
	return nil
}

// Ping implements Backend: a HEAD of the bucket.
func (b *S3Backend) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := b.do(ctx, http.MethodHead, strings.TrimRight(b.base.Path, "/")+"/"+b.o.Bucket, nil, 0, emptySHA)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("s3 ping: HTTP %d", resp.StatusCode)
	}
	return nil
}
