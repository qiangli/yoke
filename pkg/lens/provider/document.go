// Document providers expose decoded JSON and YAML files as addressable
// trees of typed Node rows with graph edges.
//
// Locator shape:
//
//	json:/path/to/file.json!/a/b/0
//	yaml:/path/to/file.yaml!/a/b/0
//
// Target names a local file. Each Inner segment descends one level: an
// object key for maps, a decimal index for arrays. An empty Inner addresses
// the document root. Providers are read-only: Open serves raw (root) or
// re-encoded JSON (inner path) bytes; there is no write path.
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/qiangli/yoke/pkg/ref"
	yaml "gopkg.in/yaml.v3"
)

// DocumentProvider is a read-only Provider over decoded JSON or YAML
// documents. Scheme is "json" or "yaml" and selects the decoder.
type DocumentProvider struct {
	Scheme string
}

// NewJSONProvider returns a DocumentProvider decoding "json" locators.
func NewJSONProvider() *DocumentProvider { return &DocumentProvider{Scheme: "json"} }

// NewYAMLProvider returns a DocumentProvider decoding "yaml" locators.
func NewYAMLProvider() *DocumentProvider { return &DocumentProvider{Scheme: "yaml"} }

// docScheme resolves the effective scheme, defaulting to "json".
func (p *DocumentProvider) docScheme() string {
	if p == nil || (p.Scheme != "json" && p.Scheme != "yaml") {
		return "json"
	}
	return p.Scheme
}

// docCheckScheme rejects locators for a different scheme.
func (p *DocumentProvider) docCheckScheme(loc ref.Locator) error {
	if loc.Scheme != "" && loc.Scheme != p.docScheme() {
		return fmt.Errorf("provider: %s: unsupported scheme %q", p.docScheme(), loc.Scheme)
	}
	return nil
}

// docDecode reads and decodes the file named by loc.Target.
func (p *DocumentProvider) docDecode(target string) (any, error) {
	if target == "" {
		return nil, fmt.Errorf("provider: %s: empty target", p.docScheme())
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		return nil, err
	}
	switch p.docScheme() {
	case "yaml":
		var v any
		if err := yaml.Unmarshal(raw, &v); err != nil {
			return nil, err
		}
		return docNormalize(v), nil
	default:
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		return docNormalize(v), nil
	}
}

// docNormalize converts decoder-specific scalars into plain values so lens
// views see stable attribute types.
func docNormalize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, e := range t {
			t[k] = docNormalize(e)
		}
		return t
	case map[any]any:
		m := make(map[string]any, len(t))
		for k, e := range t {
			m[fmt.Sprint(k)] = docNormalize(e)
		}
		return m
	case []any:
		for i, e := range t {
			t[i] = docNormalize(e)
		}
		return t
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i
		}
		if f, err := t.Float64(); err == nil {
			return f
		}
		return t.String()
	case int:
		return int64(t)
	case int32:
		return int64(t)
	case float32:
		return float64(t)
	default:
		return v
	}
}

// docWalk descends root along inner path segments.
func docWalk(root any, inner []string) (any, error) {
	cur := root
	for _, seg := range inner {
		switch t := cur.(type) {
		case map[string]any:
			next, ok := t[seg]
			if !ok {
				return nil, fmt.Errorf("provider: document: no key %q", seg)
			}
			cur = next
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(t) {
				return nil, fmt.Errorf("provider: document: no index %q", seg)
			}
			cur = t[i]
		default:
			return nil, fmt.Errorf("provider: document: cannot descend into scalar at %q", seg)
		}
	}
	return cur, nil
}

// docKind names the node kind for a decoded value.
func docKind(v any) string {
	switch v.(type) {
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case bool:
		return "bool"
	case nil:
		return "null"
	default:
		return "number"
	}
}

// docChildLoc returns the locator extended by one segment.
func docChildLoc(scheme, target string, inner []string, seg string) ref.Locator {
	next := make([]string, 0, len(inner)+1)
	next = append(next, inner...)
	next = append(next, seg)
	return ref.Locator{Scheme: scheme, Target: target, Inner: next}
}

// docNode builds the typed Node row for value v at loc.
func docNode(scheme string, loc ref.Locator, v any) Node {
	nodePath := "/" + strings.Join(loc.Inner, "/")
	name := path.Base(loc.Target)
	if len(loc.Inner) > 0 {
		name = loc.Inner[len(loc.Inner)-1]
	}
	n := Node{
		SchemaVersion: NodeSchema,
		Ref:           loc.String(),
		Path:          nodePath,
		Name:          name,
		Kind:          docKind(v),
		Type:          scheme,
		Attrs:         map[string]any{},
	}
	switch t := v.(type) {
	case map[string]any:
		count := uint64(len(t))
		n.Children = &count
		n.Attrs["count"] = len(t)
	case []any:
		count := uint64(len(t))
		n.Children = &count
		n.Attrs["count"] = len(t)
	default:
		n.Attrs["value"] = v
	}
	return n
}

// docResolve decodes the target file and walks to loc.Inner.
func (p *DocumentProvider) docResolve(loc ref.Locator) (any, error) {
	if err := p.docCheckScheme(loc); err != nil {
		return nil, err
	}
	root, err := p.docDecode(loc.Target)
	if err != nil {
		return nil, err
	}
	return docWalk(root, loc.Inner)
}

// Stat returns the Node row for the decoded value at loc.
func (p *DocumentProvider) Stat(_ context.Context, loc ref.Locator) (Node, error) {
	v, err := p.docResolve(loc)
	if err != nil {
		return Node{}, err
	}
	scheme := p.docScheme()
	addr := ref.Locator{Scheme: scheme, Target: loc.Target, Inner: loc.Inner}
	return docNode(scheme, addr, v), nil
}

// List returns one Node row per child of the container at loc.
// Scalars have no children and yield an empty list.
func (p *DocumentProvider) List(_ context.Context, loc ref.Locator) ([]Node, error) {
	v, err := p.docResolve(loc)
	if err != nil {
		return nil, err
	}
	scheme := p.docScheme()
	switch t := v.(type) {
	case map[string]any:
		out := make([]Node, 0, len(t))
		for _, k := range docSortedKeys(t) {
			out = append(out, docNode(scheme, docChildLoc(scheme, loc.Target, loc.Inner, k), t[k]))
		}
		return out, nil
	case []any:
		out := make([]Node, 0, len(t))
		for i, e := range t {
			out = append(out, docNode(scheme, docChildLoc(scheme, loc.Target, loc.Inner, strconv.Itoa(i)), e))
		}
		return out, nil
	default:
		return []Node{}, nil
	}
}

// Edges returns one contains edge per child of the container at loc.
func (p *DocumentProvider) Edges(_ context.Context, loc ref.Locator) ([]Edge, error) {
	v, err := p.docResolve(loc)
	if err != nil {
		return nil, err
	}
	scheme := p.docScheme()
	src := ref.Locator{Scheme: scheme, Target: loc.Target, Inner: loc.Inner}.String()
	var segs []string
	switch t := v.(type) {
	case map[string]any:
		segs = docSortedKeys(t)
	case []any:
		segs = make([]string, 0, len(t))
		for i := range t {
			segs = append(segs, strconv.Itoa(i))
		}
	default:
		return []Edge{}, nil
	}
	out := make([]Edge, 0, len(segs))
	for _, seg := range segs {
		out = append(out, Edge{
			SchemaVersion: EdgeSchema,
			Src:           src,
			Rel:           "contains",
			Dst:           docChildLoc(scheme, loc.Target, loc.Inner, seg).String(),
			Attrs:         map[string]any{},
		})
	}
	return out, nil
}

// Open serves file bytes read-only: the raw file at the document root, or
// the addressed sub-value re-encoded as JSON for an inner path.
func (p *DocumentProvider) Open(_ context.Context, loc ref.Locator) (io.ReadCloser, error) {
	if err := p.docCheckScheme(loc); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(loc.Target)
	if err != nil {
		return nil, err
	}
	if len(loc.Inner) == 0 {
		return io.NopCloser(bytes.NewReader(raw)), nil
	}
	v, err := p.docResolve(loc)
	if err != nil {
		return nil, err
	}
	enc, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(enc)), nil
}

// docSortedKeys returns map keys in stable order for deterministic List.
func docSortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
