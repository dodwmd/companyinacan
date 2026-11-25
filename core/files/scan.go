package files

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// ScanVerdict is the outcome of a scan.
type ScanVerdict string

// Scan verdicts. "skipped" means no scanner is configured and the operator turned
// scanner.required off: the file is kept but labelled, so nobody mistakes it for scanned.
const (
	ScanClean    ScanVerdict = "clean"
	ScanInfected ScanVerdict = "infected"
	ScanSkipped  ScanVerdict = "skipped"
)

// Scanner is the virus-scan hook. An error means "could not scan": the upload is refused, never
// let through unscanned. Scanners see the staged bytes before the backend does.
type Scanner interface {
	Scan(ctx context.Context, r io.Reader) (ScanVerdict, error)
}

// NoopScanner is the zero configuration: it scans nothing and says so.
type NoopScanner struct{}

// Scan implements Scanner.
func (NoopScanner) Scan(context.Context, io.Reader) (ScanVerdict, error) { return ScanSkipped, nil }

// FakeScanner flags content that contains InfectedMarker; for tests.
type FakeScanner struct {
	InfectedMarker string
	Err            error
}

// Scan implements Scanner.
func (f *FakeScanner) Scan(_ context.Context, r io.Reader) (ScanVerdict, error) {
	if f.Err != nil {
		return "", f.Err
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	if f.InfectedMarker != "" && bytes.Contains(b, []byte(f.InfectedMarker)) {
		return ScanInfected, nil
	}
	return ScanClean, nil
}

// ClamdScanner talks to a clamd daemon with the INSTREAM command over TCP. The file is streamed
// in chunks; nothing is buffered. Only an explicit "OK" is clean and only an explicit "FOUND" is
// infected: every other answer, a timeout and a dropped connection are errors.
type ClamdScanner struct {
	Address string
	Timeout time.Duration
}

// Scan implements Scanner.
func (c *ClamdScanner) Scan(ctx context.Context, r io.Reader) (ScanVerdict, error) {
	to := c.Timeout
	if to <= 0 {
		to = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", c.Address)
	if err != nil {
		return "", errors.New("the virus scanner is unreachable")
	}
	defer func() { _ = conn.Close() }()
	dl, _ := ctx.Deadline()
	_ = conn.SetDeadline(dl)
	if _, err := conn.Write([]byte("zINSTREAM\x00")); err != nil {
		return "", errors.New("the virus scanner did not accept the request")
	}
	buf := make([]byte, 32<<10)
	var hdr [4]byte
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			binary.BigEndian.PutUint32(hdr[:], uint32(n)) //nolint:gosec // n <= 32 KiB
			if _, err := conn.Write(append(hdr[:], buf[:n]...)); err != nil {
				return "", errors.New("the virus scanner closed the connection")
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return "", rerr
		}
	}
	if _, err := conn.Write([]byte{0, 0, 0, 0}); err != nil {
		return "", errors.New("the virus scanner closed the connection")
	}
	ans, err := io.ReadAll(io.LimitReader(conn, 4096))
	if err != nil && len(ans) == 0 {
		return "", errors.New("the virus scanner gave no answer")
	}
	s := strings.TrimRight(string(ans), "\x00\n ")
	switch {
	case strings.HasSuffix(s, " OK"):
		return ScanClean, nil
	case strings.HasSuffix(s, " FOUND"):
		return ScanInfected, nil
	}
	return "", fmt.Errorf("the virus scanner answered with an error")
}
