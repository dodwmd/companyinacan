// Package exportcrypt encrypts an export with a passphrase (specs/071-company-export), as a
// stream: Argon2id for the key, then XChaCha20-Poly1305 over chunks, so that neither side holds
// the archive in memory and a truncated, reordered or modified file does not decrypt.
//
// Layout (docs/export.md):
//
//	magic  "CANEXPORT1\n"
//	u32 passes, u32 memory (KiB), u8 threads, 16 bytes salt, 15 bytes nonce prefix (big endian)
//	chunks: seal(64 KiB of plaintext, nonce = prefix || u64 counter || u8 last), aad = the header
//
// Every chunk but the last holds exactly 64 KiB; the last holds 0 to 64 KiB and has last = 1.
package exportcrypt

import (
	"bufio"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
)

// Extension is the file name suffix of an encrypted export.
const Extension = ".canenc"

// MinPassphrase is the shortest passphrase accepted when encrypting.
const MinPassphrase = 12

const (
	magic      = "CANEXPORT1\n"
	chunkSize  = 64 << 10
	saltSize   = 16
	prefixSize = 15
	headerSize = len(magic) + 4 + 4 + 1 + saltSize + prefixSize
	// OWASP's Argon2id minimum: 19 MiB, 2 passes. can-core derives it within its memory limit
	// when the admin UI encrypts.
	passes    = 2
	memoryKiB = 19 << 10
	threads   = 1
	// A file asking for more is refused: the numbers come from the file.
	maxPasses    = 16
	maxMemoryKiB = 512 << 10
)

// ErrDecrypt is a wrong passphrase or a damaged, truncated or altered file.
var ErrDecrypt = errors.New("cannot decrypt: wrong passphrase, or the file is damaged or incomplete")

// CheckPassphrase says whether a passphrase may be used to encrypt.
func CheckPassphrase(p []byte) error {
	if len([]rune(string(p))) < MinPassphrase {
		return fmt.Errorf("the passphrase must be at least %d characters", MinPassphrase)
	}
	return nil
}

func newAEAD(passphrase, header []byte) (cipher.AEAD, error) {
	p := header[len(magic):]
	t, m, th := binary.BigEndian.Uint32(p), binary.BigEndian.Uint32(p[4:]), p[8]
	if t < 1 || t > maxPasses || m < 8 || m > maxMemoryKiB || th < 1 || th > 16 {
		return nil, ErrDecrypt
	}
	salt := p[9 : 9+saltSize]
	return chacha20poly1305.NewX(argon2.IDKey(passphrase, salt, t, m, th, chacha20poly1305.KeySize))
}

func nonce(header []byte, counter uint64, last bool) []byte {
	n := make([]byte, chacha20poly1305.NonceSizeX)
	copy(n, header[headerSize-prefixSize:])
	binary.BigEndian.PutUint64(n[prefixSize:], counter)
	if last {
		n[len(n)-1] = 1
	}
	return n
}

type writer struct {
	w       io.Writer
	aead    cipher.AEAD
	header  []byte
	buf     []byte
	counter uint64
	err     error
}

// NewWriter encrypts what is written to it into w. Close seals the last chunk: without it the
// file does not decrypt, so an export that failed must not close it.
func NewWriter(w io.Writer, passphrase []byte) (io.WriteCloser, error) {
	if err := CheckPassphrase(passphrase); err != nil {
		return nil, err
	}
	h := append(make([]byte, 0, headerSize), magic...)
	h = binary.BigEndian.AppendUint32(h, passes)
	h = binary.BigEndian.AppendUint32(h, memoryKiB)
	h = append(h, threads)
	random := make([]byte, saltSize+prefixSize)
	if _, err := rand.Read(random); err != nil {
		return nil, err
	}
	h = append(h, random...)
	aead, err := newAEAD(passphrase, h)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(h); err != nil {
		return nil, err
	}
	return &writer{w: w, aead: aead, header: h, buf: make([]byte, 0, chunkSize)}, nil
}

func (e *writer) seal(last bool) error {
	ct := e.aead.Seal(nil, nonce(e.header, e.counter, last), e.buf, e.header)
	e.counter++
	e.buf = e.buf[:0]
	_, err := e.w.Write(ct)
	return err
}

func (e *writer) Write(p []byte) (int, error) {
	n := 0
	for len(p) > 0 && e.err == nil {
		// A full chunk is sealed only when more follows: the last one must be marked.
		if len(e.buf) == chunkSize {
			e.err = e.seal(false)
			continue
		}
		c := min(chunkSize-len(e.buf), len(p))
		e.buf = append(e.buf, p[:c]...)
		p, n = p[c:], n+c
	}
	return n, e.err
}

func (e *writer) Close() error {
	if e.err != nil {
		return e.err
	}
	err := e.seal(true)
	e.err = errors.New("exportcrypt: writer is closed")
	return err
}

// Decrypt writes the plaintext of r to w. On an error what was written is incomplete and must be
// discarded.
func Decrypt(w io.Writer, r io.Reader, passphrase []byte) error {
	br := bufio.NewReaderSize(r, chunkSize+chacha20poly1305.Overhead+1)
	h := make([]byte, headerSize)
	if _, err := io.ReadFull(br, h); err != nil || string(h[:len(magic)]) != magic {
		return errors.New("this is not an encrypted export")
	}
	aead, err := newAEAD(passphrase, h)
	if err != nil {
		return err
	}
	buf := make([]byte, chunkSize+aead.Overhead())
	for counter := uint64(0); ; counter++ {
		n, err := io.ReadFull(br, buf)
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
			return ErrDecrypt
		}
		_, more := br.Peek(1)
		if more != nil && !errors.Is(more, io.EOF) {
			return more
		}
		last := more != nil
		pt, err := aead.Open(buf[:0], nonce(h, counter, last), buf[:n], h)
		if err != nil {
			return ErrDecrypt
		}
		if _, err := w.Write(pt); err != nil {
			return err
		}
		if last {
			return nil
		}
	}
}
