package files

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"slices"
	"time"

	"go.yaml.in/yaml/v3"
)

// Config is the service configuration. Secrets never live in it: the link secret, the S3
// credentials and the API token come from the environment, the field keyring from a mounted file.
type Config struct {
	Listen      string `yaml:"listen"`
	DBPath      string `yaml:"db_path"`
	KeyringPath string `yaml:"keyring_path"`
	// StagingDir holds uploads while they are checksummed and scanned. It must be on the data
	// volume: the root filesystem is read-only and /tmp is small.
	StagingDir string        `yaml:"staging_dir"`
	Backend    BackendConfig `yaml:"backend"`
	Limits     LimitsConfig  `yaml:"limits"`
	Scanner    ScannerConfig `yaml:"scanner"`
	Share      ShareConfig   `yaml:"share"`
	Retention  RetentionCfg  `yaml:"retention"`
	Download   DownloadCfg   `yaml:"download"`
	// TrustProxy honours the right-most X-Forwarded-For entry for rate limiting. Turn it on only
	// behind a proxy that appends the client address.
	TrustProxy bool `yaml:"trust_proxy"`
}

// BackendConfig selects where file bytes live. The database never holds file content.
type BackendConfig struct {
	Type  string `yaml:"type"` // local (default) or s3
	Local struct {
		Dir string `yaml:"dir"`
	} `yaml:"local"`
	S3 struct {
		Endpoint          string `yaml:"endpoint"`
		Bucket            string `yaml:"bucket"`
		Region            string `yaml:"region"`
		Prefix            string `yaml:"prefix"`
		AllowInsecureHTTP bool   `yaml:"allow_insecure_http"` // development only: the signed request would travel in clear
	} `yaml:"s3"`
}

// LimitsConfig bounds uploads and what one person can keep.
type LimitsConfig struct {
	MaxFileBytes       int64 `yaml:"max_file_bytes"`
	MaxFilesPerSpace   int   `yaml:"max_files_per_space"`
	MaxVersionsPerFile int   `yaml:"max_versions_per_file"`
	MaxSpacesPerOwner  int   `yaml:"max_spaces_per_owner"`
	QuotaBytesPerOwner int64 `yaml:"quota_bytes_per_owner"`
	MaxFolderDepth     int   `yaml:"max_folder_depth"`
	MaxMembersPerSpace int   `yaml:"max_members_per_space"`
	// MaxConcurrentUploads bounds the uploads in progress (staging disk, scanner load).
	MaxConcurrentUploads int      `yaml:"max_concurrent_uploads"`
	AllowedTypes         []string `yaml:"allowed_types"` // extensions, checked against the magic bytes
	UploadTimeout        string   `yaml:"upload_timeout"`
}

// ScannerConfig is the virus-scan hook.
type ScannerConfig struct {
	// Required (default true) refuses every upload while no real scanner is configured, and an
	// upload the scanner could not scan. Set false only for development.
	Required *bool  `yaml:"required"`
	Type     string `yaml:"type"`    // "" (none) or clamd
	Address  string `yaml:"address"` // clamd host:port
	Timeout  string `yaml:"timeout"`
}

// ShareConfig shapes share links.
type ShareConfig struct {
	DefaultTTLHours   int    `yaml:"default_ttl_hours"`
	MaxTTLHours       int    `yaml:"max_ttl_hours"`
	MaxPerFile        int    `yaml:"max_per_file"`
	FailuresPerMinute int    `yaml:"failures_per_minute"` // per client address, failures only
	BaseURL           string `yaml:"base_url"`            // public origin, shown in the created link
}

// RetentionCfg shapes deletion.
type RetentionCfg struct {
	TrashDays          *int `yaml:"trash_days"` // a deleted file is purged this many days later (default 30; 0: never)
	SweepIntervalHours int  `yaml:"sweep_interval_hours"`
}

// DownloadCfg decides how content is served. Default: attachment only.
type DownloadCfg struct {
	Inline      bool     `yaml:"inline"`       // allow ?inline=1 for InlineTypes (default false)
	InlineTypes []string `yaml:"inline_types"` // png, jpg, gif, webp only
}

// DefaultTypes is the default allow-list of extensions.
var DefaultTypes = []string{"pdf", "png", "jpg", "jpeg", "gif", "webp", "docx", "xlsx", "pptx", "odt", "ods", "odp", "rtf", "txt", "csv", "md"}

// DefaultConfig returns the configuration the service runs with when a key is not set.
func DefaultConfig() *Config {
	c := &Config{}
	c.defaults()
	return c
}

func (c *Config) defaults() {
	if c.Listen == "" {
		c.Listen = ":8095"
	}
	if c.DBPath == "" {
		c.DBPath = "/data/files.db"
	}
	if c.KeyringPath == "" {
		c.KeyringPath = "/etc/can-files/keyring"
	}
	if c.StagingDir == "" {
		c.StagingDir = "/data/staging"
	}
	if c.Backend.Type == "" {
		c.Backend.Type = "local"
	}
	if c.Backend.Local.Dir == "" {
		c.Backend.Local.Dir = "/data/blobs"
	}
	l := &c.Limits
	if l.MaxFileBytes == 0 {
		l.MaxFileBytes = 25 << 20
	}
	if l.MaxFilesPerSpace == 0 {
		l.MaxFilesPerSpace = 10000
	}
	if l.MaxVersionsPerFile == 0 {
		l.MaxVersionsPerFile = 100
	}
	if l.MaxSpacesPerOwner == 0 {
		l.MaxSpacesPerOwner = 100
	}
	if l.QuotaBytesPerOwner == 0 {
		l.QuotaBytesPerOwner = 5 << 30
	}
	if l.MaxFolderDepth == 0 {
		l.MaxFolderDepth = 8
	}
	if l.MaxMembersPerSpace == 0 {
		l.MaxMembersPerSpace = 500
	}
	if len(l.AllowedTypes) == 0 {
		l.AllowedTypes = DefaultTypes
	}
	if l.MaxConcurrentUploads == 0 {
		l.MaxConcurrentUploads = 4
	}
	if l.UploadTimeout == "" {
		l.UploadTimeout = "2m"
	}
	if c.Scanner.Required == nil {
		t := true
		c.Scanner.Required = &t
	}
	if c.Scanner.Timeout == "" {
		c.Scanner.Timeout = "30s"
	}
	s := &c.Share
	if s.DefaultTTLHours == 0 {
		s.DefaultTTLHours = 24
	}
	if s.MaxTTLHours == 0 {
		s.MaxTTLHours = 24 * 30
	}
	if s.MaxPerFile == 0 {
		s.MaxPerFile = 20
	}
	if s.FailuresPerMinute == 0 {
		s.FailuresPerMinute = 20
	}
	if c.Retention.TrashDays == nil {
		d := 30
		c.Retention.TrashDays = &d
	}
	if c.Retention.SweepIntervalHours == 0 {
		c.Retention.SweepIntervalHours = 6
	}
}

// ScannerRequired reports the effective value of scanner.required.
func (c *Config) ScannerRequired() bool { return c.Scanner.Required == nil || *c.Scanner.Required }

// UploadDeadline is the time one upload may take.
func (c *Config) UploadDeadline() time.Duration {
	d, err := time.ParseDuration(c.Limits.UploadTimeout)
	if err != nil {
		return 2 * time.Minute
	}
	return d
}

var (
	bucketRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	prefixRe = regexp.MustCompile(`^([A-Za-z0-9_-][A-Za-z0-9._-]*/)*$`)
	regionRe = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)
)

// LoadConfig reads and validates a config file. Unknown keys are errors. With must false a
// missing file means built-in defaults.
func LoadConfig(path string, must bool) (*Config, error) {
	b, err := os.ReadFile(path) //nolint:gosec // operator-supplied config path
	if errors.Is(err, os.ErrNotExist) && !must {
		return ParseConfig(nil)
	}
	if err != nil {
		return nil, err
	}
	return ParseConfig(b)
}

// ParseConfig parses and validates config text.
func ParseConfig(b []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("config: %w", err)
	}
	c.defaults()
	return &c, c.validate()
}

func (c *Config) validate() error {
	l := c.Limits
	if l.MaxFileBytes < 1 || l.MaxFileBytes > 1<<30 {
		return errors.New("config: limits.max_file_bytes must be between 1 and 1073741824")
	}
	for _, f := range []struct {
		name string
		v    int
		max  int
	}{{"max_files_per_space", l.MaxFilesPerSpace, 1_000_000}, {"max_versions_per_file", l.MaxVersionsPerFile, 10_000},
		{"max_spaces_per_owner", l.MaxSpacesPerOwner, 100_000}, {"max_folder_depth", l.MaxFolderDepth, 32}, {"max_members_per_space", l.MaxMembersPerSpace, 10_000},
		{"max_concurrent_uploads", l.MaxConcurrentUploads, 1000}} {
		if f.v < 1 || f.v > f.max {
			return fmt.Errorf("config: limits.%s must be between 1 and %d", f.name, f.max)
		}
	}
	if l.QuotaBytesPerOwner < l.MaxFileBytes {
		return errors.New("config: limits.quota_bytes_per_owner must be at least max_file_bytes")
	}
	for _, t := range l.AllowedTypes {
		if _, ok := fileTypes[t]; !ok {
			return fmt.Errorf("config: limits.allowed_types: %q is not a supported type", t)
		}
	}
	if d, err := time.ParseDuration(l.UploadTimeout); err != nil || d < time.Second || d > time.Hour {
		return errors.New("config: limits.upload_timeout must be a duration between 1s and 1h")
	}
	switch c.Scanner.Type {
	case "":
		if c.Scanner.Address != "" {
			return errors.New("config: scanner.address needs scanner.type")
		}
	case "clamd":
		if c.Scanner.Address == "" {
			return errors.New("config: scanner.address is required for clamd")
		}
	default:
		return fmt.Errorf("config: unknown scanner.type %q", c.Scanner.Type)
	}
	if d, err := time.ParseDuration(c.Scanner.Timeout); err != nil || d < time.Second || d > 10*time.Minute {
		return errors.New("config: scanner.timeout must be a duration between 1s and 10m")
	}
	s := c.Share
	if s.DefaultTTLHours < 1 || s.MaxTTLHours < s.DefaultTTLHours || s.MaxTTLHours > 24*365 || s.MaxPerFile < 1 || s.MaxPerFile > 1000 || s.FailuresPerMinute < 1 {
		return errors.New("config: share: default_ttl_hours, max_ttl_hours (at most a year), max_per_file and failures_per_minute are out of range")
	}
	if s.BaseURL != "" {
		u, err := url.Parse(s.BaseURL)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("config: share.base_url must be an http(s) origin")
		}
	}
	if *c.Retention.TrashDays < 0 || *c.Retention.TrashDays > 3650 || c.Retention.SweepIntervalHours < 1 {
		return errors.New("config: retention: trash_days 0..3650 and sweep_interval_hours >= 1")
	}
	for _, t := range c.Download.InlineTypes {
		if !slices.Contains(inlineSafe, t) {
			return fmt.Errorf("config: download.inline_types: %q cannot be shown inline (only %v)", t, inlineSafe)
		}
	}
	if c.Download.Inline && len(c.Download.InlineTypes) == 0 {
		return errors.New("config: download.inline needs download.inline_types")
	}
	switch c.Backend.Type {
	case "local":
	case "s3":
		s3 := c.Backend.S3
		u, err := url.Parse(s3.Endpoint)
		if err != nil || u.Host == "" || (u.Scheme != "https" && (u.Scheme != "http" || !s3.AllowInsecureHTTP)) || u.User != nil || u.RawQuery != "" {
			return errors.New("config: backend.s3.endpoint must be an https URL (http only with allow_insecure_http, for development)")
		}
		if !bucketRe.MatchString(s3.Bucket) {
			return errors.New("config: backend.s3.bucket is not a valid bucket name")
		}
		if s3.Region != "" && !regionRe.MatchString(s3.Region) {
			return errors.New("config: backend.s3.region is invalid")
		}
		if !prefixRe.MatchString(s3.Prefix) || len(s3.Prefix) > 128 {
			return errors.New("config: backend.s3.prefix must be empty or end with a slash (letters, digits, . _ -)")
		}
	default:
		return fmt.Errorf("config: unknown backend.type %q", c.Backend.Type)
	}
	return nil
}
