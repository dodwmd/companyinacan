package backup

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
)

// KitVersion is the recovery kit format version.
const KitVersion = 1

// Errors returned by kit handling.
var (
	ErrBadPassphrase = errors.New("recovery kit: wrong passphrase or corrupted file")
	ErrNotConfirmed  = errors.New("recovery kit: not confirmed as stored")
)

// Kit is everything needed to restore onto a blank host and nothing else: the
// restic password and bucket credentials (which would be lost with the server)
// plus enough provider information to recreate it. It is never written into
// the company repository.
type Kit struct {
	Version int `json:"version"`
	// Note is a caveat printed in the header and carried in the kit (for example: the repository holds no data yet).
	Note    string    `json:"note,omitempty"`
	Created time.Time `json:"created"`
	Company struct {
		Name   string `json:"name"`
		Domain string `json:"domain"`
	} `json:"company"`
	Restic struct {
		Repository string `json:"repository"`
		Password   string `json:"password"`
	} `json:"restic"`
	Bucket struct {
		Endpoint        string `json:"endpoint"`
		Name            string `json:"name"`
		Region          string `json:"region"`
		AccessKeyID     string `json:"access_key_id"`
		SecretAccessKey string `json:"secret_access_key"`
	} `json:"bucket"`
	Provider struct {
		Type    string            `json:"type"`
		Options map[string]string `json:"options,omitempty"` // non-secret only
	} `json:"provider"`
}

// KitInput is what NewKit needs from `make up`.
type KitInput struct {
	CompanyName, Domain          string
	Endpoint, Bucket, Region     string
	AccessKeyID, SecretAccessKey string
	ProviderType                 string
	ProviderOptions              map[string]any
	// Password reuses an existing restic password: never generate a second one
	// for a repository that already exists. Empty means generate.
	Password string
}

// kitOptionAllow lists the provider options copied into a kit. It is an
// allow-list: a new option (possibly a secret) is never copied by default.
var kitOptionAllow = map[string]bool{
	"host": true, "port": true, "user": true, "ssh_port": true, "ssh_user": true,
	"location": true, "server_type": true, "image": true, "region": true, "datacenter": true,
}

// NewKit generates a kit with a fresh random restic password.
func NewKit(in KitInput) (*Kit, error) {
	password := in.Password
	if password == "" {
		pw := make([]byte, 32)
		if _, err := rand.Read(pw); err != nil {
			return nil, err
		}
		password = base64.RawURLEncoding.EncodeToString(pw)
	}
	k := &Kit{Version: KitVersion, Created: time.Now().UTC().Truncate(time.Second)}
	k.Company.Name, k.Company.Domain = in.CompanyName, in.Domain
	k.Restic.Password = password
	k.Restic.Repository = "s3:" + strings.TrimRight(in.Endpoint, "/") + "/" + in.Bucket + "/restic"
	k.Bucket.Endpoint, k.Bucket.Name, k.Bucket.Region = in.Endpoint, in.Bucket, in.Region
	k.Bucket.AccessKeyID, k.Bucket.SecretAccessKey = in.AccessKeyID, in.SecretAccessKey
	k.Provider.Type = in.ProviderType
	k.Provider.Options = map[string]string{}
	for key, v := range in.ProviderOptions {
		if s, ok := v.(string); ok && kitOptionAllow[key] {
			k.Provider.Options[key] = s
		}
	}
	return k, k.Validate()
}

// Validate checks that the kit is complete enough to restore from.
func (k *Kit) Validate() error {
	switch {
	case k.Version != KitVersion:
		return fmt.Errorf("recovery kit: unsupported version %d", k.Version)
	case len(k.Restic.Password) < 16:
		return errors.New("recovery kit: restic password missing or too short")
	case !strings.HasPrefix(k.Restic.Repository, "s3:"):
		return errors.New("recovery kit: repository must be an s3: URL")
	case !strings.HasPrefix(k.Bucket.Endpoint, "https://"):
		return errors.New("recovery kit: bucket endpoint must be an https:// URL")
	case k.Bucket.AccessKeyID == "" || k.Bucket.SecretAccessKey == "":
		return errors.New("recovery kit: bucket credentials missing")
	case k.Provider.Type == "":
		return errors.New("recovery kit: provider type missing")
	}
	return nil
}

const (
	beginPlain = "-----BEGIN CAN RECOVERY KIT-----"
	endPlain   = "-----END CAN RECOVERY KIT-----"
	beginEnc   = "-----BEGIN CAN ENCRYPTED RECOVERY KIT-----"
	endEnc     = "-----END CAN ENCRYPTED RECOVERY KIT-----"
)

// Text renders the kit for printing: a human-readable summary followed by the
// machine-readable block `can restore --kit` parses.
func (k *Kit) Text() string {
	j, _ := json.MarshalIndent(k, "", "  ")
	var opts []string
	for key, v := range k.Provider.Options {
		opts = append(opts, key+"="+v)
	}
	sort.Strings(opts)
	head := "COMPANY IN A CAN - RECOVERY KIT"
	if k.Note != "" {
		head += "\nCAVEAT: " + k.Note
	}
	return fmt.Sprintf(`%s
Company:          %s (%s)
Created:          %s
Provider:         %s %s
Restic repo:      %s
Restic password:  %s
Bucket key id:    %s
Bucket secret:    %s

Store this OUTSIDE the server (a password manager). Without it the backups cannot be
read, and it is not stored anywhere else. Restore: can restore --kit <file>

%s
%s
%s
`, head, k.Company.Name, k.Company.Domain, k.Created.Format(time.RFC3339), k.Provider.Type, strings.Join(opts, " "),
		k.Restic.Repository, k.Restic.Password, k.Bucket.AccessKeyID, k.Bucket.SecretAccessKey,
		beginPlain, j, endPlain)
}

// Repo returns the restic repository settings for this kit; passwordFile must
// hold the restic password (see PasswordFile).
func (k *Kit) Repo(passwordFile string) Repo {
	env := []string{"AWS_ACCESS_KEY_ID=" + k.Bucket.AccessKeyID, "AWS_SECRET_ACCESS_KEY=" + k.Bucket.SecretAccessKey}
	if k.Bucket.Region != "" {
		env = append(env, "AWS_DEFAULT_REGION="+k.Bucket.Region)
	}
	return Repo{Repository: k.Restic.Repository, PasswordFile: passwordFile, Env: env}
}

// PasswordFile writes the restic password to a 0600 file in dir and returns its
// path and a cleanup func. The password is never passed on a command line.
func (k *Kit) PasswordFile(dir string) (string, func(), error) {
	d, err := os.MkdirTemp(dir, "can-restic-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(d) }
	name := filepath.Join(d, "password")
	if err := os.WriteFile(name, []byte(k.Restic.Password), 0o600); err != nil {
		cleanup()
		return "", nil, err
	}
	return name, cleanup, nil
}

// --- encryption: argon2id + XChaCha20-Poly1305 ----------------------------

const (
	kitMagic   = "CANKIT1\n"
	argonTime  = 3
	argonMemKB = 64 * 1024
	argonPar   = 4
	minPassLen = 12
)

func kitHeader(salt []byte) []byte {
	h := make([]byte, 0, len(kitMagic)+len(salt)+9)
	h = append(h, kitMagic...)
	h = append(h, salt...)
	h = binary.BigEndian.AppendUint32(h, argonTime)
	h = binary.BigEndian.AppendUint32(h, argonMemKB)
	return append(h, argonPar)
}

// EncryptKit seals the kit under a passphrase and returns the armored text.
func EncryptKit(k *Kit, passphrase string) ([]byte, error) {
	if len(passphrase) < minPassLen {
		return nil, fmt.Errorf("recovery kit: passphrase must be at least %d characters", minPassLen)
	}
	plain, err := json.Marshal(k)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.NewX(argon2.IDKey([]byte(passphrase), salt, argonTime, argonMemKB, argonPar, 32))
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	hdr := kitHeader(salt)
	blob := append(append(append([]byte{}, hdr...), nonce...), aead.Seal(nil, nonce, plain, hdr)...)
	enc := base64.StdEncoding.EncodeToString(blob)
	var b strings.Builder
	b.WriteString(beginEnc + "\n")
	for len(enc) > 64 {
		b.WriteString(enc[:64] + "\n")
		enc = enc[64:]
	}
	b.WriteString(enc + "\n" + endEnc + "\n")
	return []byte(b.String()), nil
}

func decryptKit(armored, passphrase string) (*Kit, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(armored), ""))
	if err != nil {
		return nil, ErrBadPassphrase
	}
	hl := len(kitMagic) + 16 + 9
	if len(raw) < hl+chacha20poly1305.NonceSizeX+chacha20poly1305.Overhead || string(raw[:len(kitMagic)]) != kitMagic {
		return nil, ErrBadPassphrase
	}
	hdr := raw[:hl]
	salt := hdr[len(kitMagic) : len(kitMagic)+16]
	// Parameters are fixed by the format version; ignore header values so a
	// tampered file cannot request absurd memory.
	if binary.BigEndian.Uint32(hdr[len(kitMagic)+16:]) != argonTime || binary.BigEndian.Uint32(hdr[len(kitMagic)+20:]) != argonMemKB || hdr[hl-1] != argonPar {
		return nil, ErrBadPassphrase
	}
	aead, err := chacha20poly1305.NewX(argon2.IDKey([]byte(passphrase), salt, argonTime, argonMemKB, argonPar, 32))
	if err != nil {
		return nil, err
	}
	nonce := raw[hl : hl+aead.NonceSize()]
	plain, err := aead.Open(nil, nonce, raw[hl+aead.NonceSize():], hdr)
	if err != nil {
		return nil, ErrBadPassphrase
	}
	return decodeKit(plain)
}

func decodeKit(b []byte) (*Kit, error) {
	var k Kit
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&k); err != nil {
		return nil, fmt.Errorf("recovery kit: %w", err)
	}
	if err := k.Validate(); err != nil {
		return nil, err
	}
	return &k, nil
}

// ParseKit reads a kit from the printed text, raw JSON or an encrypted file.
// ask supplies the passphrase for an encrypted kit; it may be nil.
func ParseKit(data []byte, ask func(prompt string) (string, error)) (*Kit, error) {
	s := string(data)
	if i := strings.Index(s, beginEnc); i >= 0 {
		j := strings.Index(s, endEnc)
		if j < i {
			return nil, errors.New("recovery kit: unterminated encrypted block")
		}
		if ask == nil {
			return nil, errors.New("recovery kit: encrypted; a passphrase source is required (--passphrase-file or a terminal)")
		}
		pass, err := ask("Recovery kit passphrase: ")
		if err != nil {
			return nil, err
		}
		return decryptKit(s[i+len(beginEnc):j], pass)
	}
	if i := strings.Index(s, beginPlain); i >= 0 {
		j := strings.Index(s, endPlain)
		if j < i {
			return nil, errors.New("recovery kit: unterminated block")
		}
		return decodeKit([]byte(s[i+len(beginPlain) : j]))
	}
	return decodeKit(bytes.TrimSpace(data))
}

// insideGitRepo reports whether path lies in a git working tree. Symlinks are
// resolved first, so a link from outside into a repository does not hide it.
func insideGitRepo(path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return true // fail closed
	}
	existing, tail := filepath.Dir(abs), ""
	for { // deepest existing ancestor
		if _, err := os.Lstat(existing); err == nil || existing == filepath.Dir(existing) {
			break
		}
		tail = filepath.Join(filepath.Base(existing), tail)
		existing = filepath.Dir(existing)
	}
	if r, err := filepath.EvalSymlinks(existing); err == nil {
		existing = r
	}
	dir := filepath.Join(existing, tail)
	for ; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return true
		}
		if dir == filepath.Dir(dir) {
			return false
		}
	}
}

// WriteKitFile writes the kit encrypted under passphrase to a new 0600 file.
// It refuses paths inside a git working tree (kits must never reach a repo)
// and never overwrites.
func WriteKitFile(path string, k *Kit, passphrase string) error {
	if insideGitRepo(path) {
		return fmt.Errorf("refusing to write the recovery kit inside a git working tree (%s): store it outside the repository", path)
	}
	enc, err := EncryptKit(k, passphrase)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // operator-chosen path
	if err != nil {
		return err
	}
	if _, err := f.Write(enc); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}
	// Read it back and decrypt it: a kit that cannot be opened is worse than none.
	back, err := os.ReadFile(path) //nolint:gosec // just written
	if err == nil {
		var got *Kit
		if got, err = ParseKit(back, func(string) (string, error) { return passphrase, nil }); err == nil && got.Restic.Password != k.Restic.Password {
			err = errors.New("decrypted kit differs from the original")
		}
	}
	if err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("verifying the written kit file: %w", err)
	}
	return nil
}

// PrintOnce shows the kit after an explicit confirmation and asks the operator
// to acknowledge having stored it. The kit is not printed unless the first
// answer is "show"; ErrNotConfirmed is returned if the acknowledgement ("saved")
// is not given within three attempts, so callers can warn loudly.
func PrintOnce(in io.Reader, out io.Writer, k *Kit) error {
	r := bufio.NewReader(in)
	ask := func(prompt string) string {
		_, _ = fmt.Fprint(out, prompt)
		l, _ := r.ReadString('\n')
		return strings.ToLower(strings.TrimSpace(l))
	}
	_, _ = fmt.Fprintln(out, "The recovery kit is shown ONCE. Without it your backups cannot be restored if the server is lost.")
	if ask("Type 'show' to display it: ") != "show" {
		return ErrNotConfirmed
	}
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprint(out, k.Text())
	for i := 0; i < 3; i++ {
		if ask("Type 'saved' once the kit is stored outside this server: ") == "saved" {
			return nil
		}
	}
	return ErrNotConfirmed
}
