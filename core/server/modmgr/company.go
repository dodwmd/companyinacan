package modmgr

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"go.yaml.in/yaml/v3"

	"github.com/dodwmd/companyinacan/core/server/kube"
)

// Limits of the company.yaml edit path.
const (
	MaxCompanyBytes = 256 << 10
	MaxPatchBytes   = 64 << 10
	MaxPatchOps     = 200
	MaxDiffLines    = 1500
	diffContext     = 2
)

// Errors of the edit path.
var (
	// ErrConflict means company.yaml changed after the version the change was based on.
	ErrConflict = errors.New("company.yaml changed since this change was prepared")
	// ErrNoCompany means there is no company.yaml to edit yet.
	ErrNoCompany = errors.New("no company.yaml is stored")
	// ErrPatchPath means a patch touches something the UI may not edit.
	ErrPatchPath = errors.New("patch path is not editable from the admin UI")
)

// Snapshot is a stored company.yaml and its version.
type Snapshot struct {
	Data    []byte
	Version string
}

// CompanyStore is the single source of truth for the applied company.yaml. In a cluster it is the
// ConfigMap can-system/company (KubeStore); in development a file (FileStore).
type CompanyStore interface {
	Get(ctx context.Context) (Snapshot, error)
	// Put replaces the document if the stored version is still expected, and returns the new version;
	// otherwise ErrConflict.
	Put(ctx context.Context, data []byte, expected string) (string, error)
}

func contentVersion(b []byte) string {
	h := sha256.Sum256(b)
	return "sha:" + hex.EncodeToString(h[:8])
}

// FileStore keeps company.yaml in a file; writes are atomic (temp file and rename).
type FileStore struct {
	Path string
	mu   sync.Mutex
}

// Get implements CompanyStore.
func (f *FileStore) Get(context.Context) (Snapshot, error) {
	b, err := os.ReadFile(f.Path)
	if errors.Is(err, os.ErrNotExist) {
		return Snapshot{}, ErrNoCompany
	}
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Data: b, Version: contentVersion(b)}, nil
}

// Put implements CompanyStore.
func (f *FileStore) Put(ctx context.Context, data []byte, expected string) (string, error) {
	if len(data) > MaxCompanyBytes {
		return "", fmt.Errorf("company.yaml would be larger than %d KiB", MaxCompanyBytes>>10)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	cur, err := f.Get(ctx)
	switch {
	case errors.Is(err, ErrNoCompany):
		if expected != "" {
			return "", ErrConflict
		}
	case err != nil:
		return "", err
	case cur.Version != expected:
		return "", ErrConflict
	}
	tmp, err := os.CreateTemp(filepath.Dir(f.Path), ".company-*.yaml")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil { //nolint:gosec // the document holds no secrets (settings carry Secret names only)
		return "", err
	}
	if err := os.Rename(tmp.Name(), f.Path); err != nil {
		return "", err
	}
	return contentVersion(data), nil
}

// KubeStore keeps company.yaml in the ConfigMap can-system/company (the one `can apply` writes and
// can-core mounts). Concurrency control is the API server's resourceVersion precondition.
type KubeStore struct {
	API       kube.API
	Namespace string // default can-system
	Name      string // default company
}

const companyKey = "company.yaml"

// ServerEditAnnotation on ConfigMap can-system/company holds the SHA-256 (hex) of the document
// can-core last wrote. While it matches the document, the latest change was made in the admin UI.
const ServerEditAnnotation = "can.dev/server-edit-sha256"

func (k *KubeStore) ref() (string, string) {
	ns, name := k.Namespace, k.Name
	if ns == "" {
		ns = "can-system"
	}
	if name == "" {
		name = "company"
	}
	return ns, name
}

// Get implements CompanyStore.
func (k *KubeStore) Get(ctx context.Context) (Snapshot, error) {
	ns, name := k.ref()
	o, err := k.API.Get(ctx, "ConfigMap", ns, name)
	if kube.IsNotFound(err) {
		return Snapshot{}, ErrNoCompany
	}
	if err != nil {
		return Snapshot{}, err
	}
	data, _ := o["data"].(map[string]any)
	s, ok := data[companyKey].(string)
	if !ok {
		return Snapshot{}, ErrNoCompany
	}
	v := contentVersion([]byte(s))
	if md, ok := o["metadata"].(map[string]any); ok {
		if rv, _ := md["resourceVersion"].(string); rv != "" {
			v = "rv:" + rv
		}
	}
	return Snapshot{Data: []byte(s), Version: v}, nil
}

// Put implements CompanyStore.
func (k *KubeStore) Put(ctx context.Context, data []byte, expected string) (string, error) {
	if len(data) > MaxCompanyBytes {
		return "", fmt.Errorf("company.yaml would be larger than %d KiB", MaxCompanyBytes>>10)
	}
	cur, err := k.Get(ctx)
	if err != nil {
		return "", err
	}
	if cur.Version != expected {
		return "", ErrConflict
	}
	ns, name := k.ref()
	// The annotation says the document was written here, not by the CLI: `can apply` then refuses to
	// overwrite it with a company.yaml that has not seen it.
	sum := sha256.Sum256(data)
	md := map[string]any{"annotations": map[string]any{ServerEditAnnotation: hex.EncodeToString(sum[:])}}
	if rv, ok := strings.CutPrefix(expected, "rv:"); ok {
		md["resourceVersion"] = rv
	}
	patch := kube.Object{"data": map[string]any{companyKey: string(data)}, "metadata": md}
	if err := k.API.MergePatch(ctx, "ConfigMap", ns, name, patch); err != nil {
		if kube.IsConflict(err) {
			return "", ErrConflict
		}
		return "", err
	}
	next, err := k.Get(ctx)
	if err != nil {
		return "", err
	}
	return next.Version, nil
}

// --- patches ------------------------------------------------------------------------------------

// Op sets one value of company.yaml.
type Op struct {
	Path  []string `json:"path"`
	Value any      `json:"value"`
}

// Patch is the list of edits a UI action makes to company.yaml.
type Patch struct {
	Ops []Op `json:"ops"`
}

// SetEnabled enables or disables a module.
func SetEnabled(module string, on bool) Op {
	return Op{Path: []string{"modules", module, "enabled"}, Value: on}
}

// SetSettings replaces a module's settings.
func SetSettings(module string, settings map[string]any) Op {
	return Op{Path: []string{"modules", module, "settings"}, Value: settings}
}

// SetString sets a top-level string (name, domain).
func SetString(key, v string) Op { return Op{Path: []string{key}, Value: v} }

// Marshal encodes the patch as stored in the database.
func (p Patch) Marshal() ([]byte, error) { return json.Marshal(p) }

// ParsePatch decodes a stored patch; integers stay integers.
func ParsePatch(b []byte) (Patch, error) {
	if len(b) > MaxPatchBytes {
		return Patch{}, fmt.Errorf("patch is larger than %d KiB", MaxPatchBytes>>10)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var p Patch
	if err := dec.Decode(&p); err != nil {
		return Patch{}, fmt.Errorf("patch is not valid: %w", err)
	}
	if len(p.Ops) > MaxPatchOps {
		return Patch{}, errors.New("patch has too many operations")
	}
	for i := range p.Ops {
		p.Ops[i].Value = normaliseNumbers(p.Ops[i].Value)
	}
	return p, nil
}

func normaliseNumbers(v any) any {
	switch x := v.(type) {
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i
		}
		f, _ := x.Float64()
		return f
	case map[string]any:
		for k, e := range x {
			x[k] = normaliseNumbers(e)
		}
	case []any:
		for i, e := range x {
			x[i] = normaliseNumbers(e)
		}
	}
	return v
}

var (
	moduleIDRe = regexp.MustCompile(`^[a-z][a-z0-9-]{1,30}$`)
	capRe      = regexp.MustCompile(`^[a-z][a-z0-9]*(\.[a-z0-9]+)*$`)
)

// checkOp is the allow-list: the UI edits the company name and domain, the declared server size,
// the site template, capability primaries and a module's enabled flag and settings, nothing else.
func checkOp(o Op) error {
	p := o.Path
	bad := func() error { return fmt.Errorf("%w: %s", ErrPatchPath, strings.Join(p, ".")) }
	switch {
	case len(p) == 1 && (p[0] == "name" || p[0] == "domain"):
		if _, ok := o.Value.(string); !ok {
			return bad()
		}
	case len(p) == 2 && p[0] == "site" && p[1] == "template":
		if _, ok := o.Value.(string); !ok {
			return bad()
		}
	case len(p) == 3 && p[0] == "provider" && p[1] == "options" && p[2] == "server_ram_mb":
		if _, ok := o.Value.(int64); !ok {
			return bad()
		}
	case len(p) == 2 && p[0] == "primary" && capRe.MatchString(p[1]):
		if s, ok := o.Value.(string); !ok || !moduleIDRe.MatchString(s) {
			return bad()
		}
	case len(p) == 3 && p[0] == "modules" && moduleIDRe.MatchString(p[1]) && p[2] == "enabled":
		if _, ok := o.Value.(bool); !ok {
			return bad()
		}
	case len(p) == 3 && p[0] == "modules" && moduleIDRe.MatchString(p[1]) && p[2] == "settings":
		if _, ok := o.Value.(map[string]any); !ok {
			return bad()
		}
	default:
		return bad()
	}
	return nil
}

// ApplyPatch applies the patch to a company.yaml document and returns the new document. Comments and
// every key the patch does not touch are preserved. Any path outside the allow-list fails the whole
// patch.
func ApplyPatch(data []byte, p Patch) ([]byte, error) {
	if len(p.Ops) > MaxPatchOps {
		return nil, errors.New("patch has too many operations")
	}
	for _, o := range p.Ops {
		if err := checkOp(o); err != nil {
			return nil, err
		}
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, errors.New("company.yaml cannot be parsed")
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("company.yaml must be a mapping")
	}
	if hasAlias(doc.Content[0]) {
		return nil, errors.New("company.yaml uses YAML aliases or anchors, which the admin UI will not edit; edit it in the repository")
	}
	for _, o := range p.Ops {
		// A settings edit must never create an enabled module: a missing `enabled` key means true.
		if len(o.Path) == 3 && o.Path[0] == "modules" && o.Path[2] == "settings" {
			if mods := mapGet(doc.Content[0], "modules"); mods == nil || mapGet(mods, o.Path[1]) == nil {
				if err := setPath(doc.Content[0], []string{"modules", o.Path[1], "enabled"}, false); err != nil {
					return nil, err
				}
			}
		}
		if err := setPath(doc.Content[0], o.Path, o.Value); err != nil {
			return nil, err
		}
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	_ = enc.Close()
	if buf.Len() > MaxCompanyBytes {
		return nil, fmt.Errorf("company.yaml would be larger than %d KiB", MaxCompanyBytes>>10)
	}
	return buf.Bytes(), nil
}

func hasAlias(n *yaml.Node) bool {
	if n.Kind == yaml.AliasNode || n.Anchor != "" {
		return true
	}
	for _, c := range n.Content {
		if hasAlias(c) {
			return true
		}
	}
	return false
}

func mapGet(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func setPath(root *yaml.Node, path []string, v any) error {
	cur := root
	for i, key := range path {
		last := i == len(path)-1
		if last {
			var val yaml.Node
			if err := val.Encode(v); err != nil {
				return err
			}
			for j := 0; j+1 < len(cur.Content); j += 2 {
				if cur.Content[j].Value == key {
					val.HeadComment, val.LineComment = cur.Content[j+1].HeadComment, cur.Content[j+1].LineComment
					cur.Content[j+1] = &val
					return nil
				}
			}
			cur.Content = append(cur.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, &val)
			return nil
		}
		next := mapGet(cur, key)
		switch {
		case next == nil || (next.Kind == yaml.ScalarNode && next.Tag == "!!null"):
			fresh := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			if next == nil {
				cur.Content = append(cur.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, fresh)
			} else {
				*next = *fresh
				fresh = next
			}
			cur = fresh
		case next.Kind == yaml.MappingNode:
			cur = next
		default:
			return fmt.Errorf("company.yaml: %s is not a mapping", strings.Join(path[:i+1], "."))
		}
	}
	return nil
}

// --- diff ---------------------------------------------------------------------------------------

// DiffLine is one line of a unified-style diff: Op is ' ', '+', '-' or '~' (collapsed unchanged lines).
type DiffLine struct {
	Op   byte
	Text string
}

// DiffTooLarge reports whether either document is too long to diff.
func DiffTooLarge(a, b string) bool {
	return strings.Count(a, "\n") > MaxDiffLines || strings.Count(b, "\n") > MaxDiffLines
}

// Diff is a line diff of two documents with two lines of context; long unchanged runs collapse to a
// single '~' line. Identical documents give no lines. Callers check DiffTooLarge first.
func Diff(oldS, newS string) []DiffLine {
	if oldS == newS {
		return nil
	}
	a, b := strings.Split(oldS, "\n"), strings.Split(newS, "\n")
	n, m := len(a), len(b)
	lcs := make([][]uint16, n+1)
	for i := range lcs {
		lcs[i] = make([]uint16, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var full []DiffLine
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			full = append(full, DiffLine{' ', a[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			full = append(full, DiffLine{'-', a[i]})
			i++
		default:
			full = append(full, DiffLine{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		full = append(full, DiffLine{'-', a[i]})
	}
	for ; j < m; j++ {
		full = append(full, DiffLine{'+', b[j]})
	}
	keep := make([]bool, len(full))
	for k, l := range full {
		if l.Op == ' ' {
			continue
		}
		for d := -diffContext; d <= diffContext; d++ {
			if k+d >= 0 && k+d < len(full) {
				keep[k+d] = true
			}
		}
	}
	var out []DiffLine
	skipped := false
	for k, l := range full {
		if keep[k] {
			out = append(out, l)
			skipped = false
		} else if !skipped {
			out = append(out, DiffLine{'~', ""})
			skipped = true
		}
	}
	return out
}
