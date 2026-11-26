// Package siteedit is the website editor of the admin UI: it edits the `site:` section of the
// applied company.yaml, previews it, publishes it to the site pod and keeps a history
// (specs/064-site-editor, docs/site-editor.md).
package siteedit

import (
	"bytes"
	"errors"
	"reflect"

	"go.yaml.in/yaml/v3"

	"github.com/dodwmd/companyinacan/core/config"
)

// ErrDocument means company.yaml cannot be edited here.
var ErrDocument = errors.New("company.yaml cannot be edited in the admin UI: it is not a mapping, or uses YAML anchors or aliases; edit it in the repository")

// Merge returns doc with its `site:` section replaced by s. Comments, ordering and formatting of
// everything the change does not touch are kept.
func Merge(doc []byte, s config.Site) ([]byte, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(doc, &root); err != nil || len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode || hasAlias(root.Content[0]) {
		return nil, ErrDocument
	}
	next, err := siteNode(s)
	if err != nil {
		return nil, err
	}
	top := root.Content[0]
	found := false
	for i := 0; i+1 < len(top.Content); i += 2 {
		if top.Content[i].Value == "site" {
			top.Content[i+1] = mergeNode(top.Content[i+1], next)
			found = true
		}
	}
	if !found {
		if len(next.Content) == 0 {
			return doc, nil
		}
		top.Content = append(top.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "site"}, next)
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&root); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
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

// siteNode encodes s without its empty values. The sections that mean something when empty
// (contact: {}, support: {}, careers: {}) stay.
func siteNode(s config.Site) (*yaml.Node, error) {
	var n yaml.Node
	if err := n.Encode(s); err != nil {
		return nil, err
	}
	prune(&n)
	present := map[string]bool{"contact": s.Contact != nil, "support": s.Support != nil, "careers": s.Careers != nil}
	for _, k := range []string{"contact", "support", "careers"} {
		if present[k] && lookup(&n, k) == nil {
			n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k}, &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Style: yaml.FlowStyle})
		}
	}
	return &n, nil
}

// empty reports whether a value is the same as leaving it out.
func empty(n *yaml.Node) bool {
	switch n.Kind {
	case yaml.MappingNode, yaml.SequenceNode:
		return len(n.Content) == 0
	case yaml.ScalarNode:
		return n.Tag == "!!null" || (n.Tag == "!!str" && n.Value == "") || (n.Tag == "!!bool" && n.Value == "false")
	}
	return false
}

func prune(n *yaml.Node) {
	switch n.Kind {
	case yaml.MappingNode:
		kept := n.Content[:0]
		for i := 0; i+1 < len(n.Content); i += 2 {
			prune(n.Content[i+1])
			if !empty(n.Content[i+1]) {
				kept = append(kept, n.Content[i], n.Content[i+1])
			}
		}
		n.Content = kept
		n.Style = 0
	case yaml.SequenceNode:
		for _, c := range n.Content {
			prune(c)
		}
	}
}

func lookup(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func same(a, b *yaml.Node) bool {
	var x, y any
	return a.Decode(&x) == nil && b.Decode(&y) == nil && reflect.DeepEqual(x, y)
}

// mergeNode is next, written the way old was wherever they agree.
func mergeNode(old, next *yaml.Node) *yaml.Node {
	if same(old, next) {
		return old
	}
	switch {
	case old.Kind == yaml.MappingNode && next.Kind == yaml.MappingNode:
		out := *old
		out.Content = nil
		if len(old.Content) == 0 {
			out.Style = next.Style
		}
		for i := 0; i+1 < len(old.Content); i += 2 {
			k, v := old.Content[i], old.Content[i+1]
			switch n := lookup(next, k.Value); {
			case n != nil:
				out.Content = append(out.Content, k, mergeNode(v, n))
			case empty(v) && v.Kind == yaml.ScalarNode: // "noindex: false" says the same as no key
				out.Content = append(out.Content, k, v)
			}
		}
		for i := 0; i+1 < len(next.Content); i += 2 {
			if lookup(old, next.Content[i].Value) == nil {
				out.Content = append(out.Content, next.Content[i], next.Content[i+1])
			}
		}
		return &out
	case old.Kind == yaml.SequenceNode && next.Kind == yaml.SequenceNode:
		out := *old
		out.Content = nil
		for i, n := range next.Content {
			// An item that is unchanged keeps its comments, wherever it moved to.
			var keep *yaml.Node
			for _, o := range old.Content {
				if same(o, n) {
					keep = o
					break
				}
			}
			switch {
			case keep != nil:
				out.Content = append(out.Content, keep)
			case i < len(old.Content):
				out.Content = append(out.Content, mergeNode(old.Content[i], n))
			default:
				out.Content = append(out.Content, n)
			}
		}
		return &out
	}
	n := *next
	n.HeadComment, n.LineComment, n.FootComment = old.HeadComment, old.LineComment, old.FootComment
	if old.Kind == yaml.ScalarNode && n.Kind == yaml.ScalarNode && n.Tag == "!!str" && n.Style == 0 && (old.Style == yaml.DoubleQuotedStyle || old.Style == yaml.SingleQuotedStyle) {
		n.Style = old.Style
	}
	return &n
}
