package exportcrypt

import (
	"bytes"
	"errors"
	"testing"
)

var pass = []byte("correct horse battery")

func seal(t *testing.T, plain []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	w, err := NewWriter(&out, pass)
	if err != nil {
		t.Fatal(err)
	}
	// Uneven writes, to cross chunk boundaries.
	for p := plain; len(p) > 0; {
		n := min(40_000, len(p))
		if _, err := w.Write(p[:n]); err != nil {
			t.Fatal(err)
		}
		p = p[n:]
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestRoundTrip(t *testing.T) {
	for _, size := range []int{0, 1, chunkSize - 1, chunkSize, chunkSize + 1, 3 * chunkSize, 200_000} {
		plain := bytes.Repeat([]byte("secret salary "), size/14+1)[:size]
		sealed := seal(t, plain)
		if size > 20 && bytes.Contains(sealed, []byte("secret salary")) {
			t.Fatal("plaintext in the output")
		}
		var out bytes.Buffer
		if err := Decrypt(&out, bytes.NewReader(sealed), pass); err != nil {
			t.Fatalf("%d: %v", size, err)
		}
		if !bytes.Equal(out.Bytes(), plain) {
			t.Fatalf("%d: differs", size)
		}
	}
}

func TestTamperingFails(t *testing.T) {
	sealed := seal(t, bytes.Repeat([]byte("x"), 2*chunkSize+100))
	chunk := chunkSize + 16
	flip := func(i int) []byte {
		b := bytes.Clone(sealed)
		b[i] ^= 1
		return b
	}
	swapped := bytes.Clone(sealed)
	copy(swapped[headerSize:], sealed[headerSize+chunk:headerSize+2*chunk])
	copy(swapped[headerSize+chunk:], sealed[headerSize:headerSize+chunk])
	for name, b := range map[string][]byte{
		"truncated at a chunk": sealed[:headerSize+2*chunk],
		"truncated inside":     sealed[:len(sealed)-1],
		"header only":          sealed[:headerSize],
		"extended":             append(bytes.Clone(sealed), 0),
		"salt":                 flip(len(magic) + 12),
		"body":                 flip(headerSize + 5),
		"reordered":            swapped,
	} {
		var out bytes.Buffer
		if err := Decrypt(&out, bytes.NewReader(b), pass); !errors.Is(err, ErrDecrypt) {
			t.Errorf("%s: %v", name, err)
		}
	}
	var out bytes.Buffer
	if err := Decrypt(&out, bytes.NewReader(sealed), []byte("wrong passphrase!!")); !errors.Is(err, ErrDecrypt) {
		t.Error(err)
	}
	// Parameters from the file are bounded before any memory is taken.
	huge := bytes.Clone(sealed)
	huge[len(magic)+4] = 0xff
	if err := Decrypt(&out, bytes.NewReader(huge), pass); !errors.Is(err, ErrDecrypt) {
		t.Error(err)
	}
	if err := Decrypt(&out, bytes.NewReader([]byte("PK\x03\x04 something else entirely, long enough to be a header ......")), pass); err == nil {
		t.Error("accepted another file")
	}
}

func TestShortPassphraseRefused(t *testing.T) {
	if _, err := NewWriter(&bytes.Buffer{}, []byte("short")); err == nil {
		t.Fatal("accepted")
	}
}
