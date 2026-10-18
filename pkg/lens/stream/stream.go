// Package stream is the bashy-graph-v1 framing every tree/graph producer
// emits under --json and every consumer (lens, `graph impact -`, …) reads.
//
// A stream is JSONL, one record per line, in this order:
//
//	{"t":"header","schema_version":"bashy-graph-v1","source":LOCATOR,"rev":"sha256:…"}
//	{"t":"node",…bashy-node-v1}      zero or more, sorted by ref
//	{"t":"edge",…bashy-edge-v1}      zero or more, sorted by src, rel, dst
//	{"t":"cursor","cursor":{…bashy-cursor-v1}}
//
// There is no wall-clock field: rev is the sha256 of the canonical node and
// edge lines, so the same snapshot and options give the same bytes (prompt
// caching, and the pull/push identity: a provider pulled by lens and the
// producer piped into lens go through these exact bytes).
//
// Filtering. A filter (jq, grep) that keeps the header line keeps the stream
// self-describing; one that drops it does not, and a reader then needs the
// schema declared explicitly (`lens -f graph`). Records are still validated
// one by one by their own schema_version. A filtered stream's rev and cursor
// still name the snapshot it was cut from, not the filtered content —
// Verify reports the difference. Nothing here claims ordinary filtering
// preserves framing on its own.
package stream

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/lens/provider"
)

const (
	GraphSchema  = "bashy-graph-v1"
	CursorSchema = "bashy-cursor-v1"

	KindPagination   = "pagination"
	KindChangeStream = "change-stream"
)

// ErrNoHeader means the input carries no bashy-graph-v1 header — typically a
// filter removed it — and the caller did not declare the schema explicitly.
var ErrNoHeader = errors.New("stream: no " + GraphSchema + " header (a filter may have removed it; declare the input schema, e.g. lens -f graph)")

// ErrNoTrailer means a framed stream ended without its cursor trailer — it
// was truncated or filtered — and the caller did not declare the schema.
var ErrNoTrailer = errors.New("stream: framed stream has no " + CursorSchema + " trailer (truncated or filtered; declare the input schema, e.g. lens -f graph)")

// ErrCursorBinding means a cursor was presented to a provider, target or
// query it was not issued for. Cursors are never converted across providers.
var ErrCursorBinding = errors.New("stream: cursor is bound to a different provider, target or query")

// Header is the first record of a framed stream.
type Header struct {
	T             string `json:"t"`
	SchemaVersion string `json:"schema_version"`
	Source        string `json:"source"`
	Rev           string `json:"rev"`
}

// Binding ties an opaque token to the context it was issued for.
type Binding struct {
	Provider string `json:"provider"`
	Target   string `json:"target"`
	Query    string `json:"query"`
	Revision string `json:"revision,omitempty"`
}

// Cursor is the bashy-cursor-v1 envelope. Token is opaque to everyone but the
// issuing provider; consumers compare bindings, never parse tokens.
type Cursor struct {
	SchemaVersion string  `json:"schema_version"`
	Kind          string  `json:"kind"`
	Token         string  `json:"token"`
	Binding       Binding `json:"binding"`
	Expiry        string  `json:"expiry,omitempty"`
}

// Validate checks the envelope's required fields and enums.
func (c Cursor) Validate() error {
	switch {
	case c.SchemaVersion != CursorSchema:
		return fmt.Errorf("stream: cursor schema_version %q, want %q", c.SchemaVersion, CursorSchema)
	case c.Kind != KindPagination && c.Kind != KindChangeStream:
		return fmt.Errorf("stream: cursor kind %q, want %q or %q", c.Kind, KindPagination, KindChangeStream)
	case c.Token == "":
		return errors.New("stream: cursor token is empty")
	case c.Binding.Provider == "" || c.Binding.Target == "" || c.Binding.Query == "":
		return errors.New("stream: cursor binding needs provider, target and query")
	}
	if c.Expiry != "" {
		if _, err := time.Parse(time.RFC3339, c.Expiry); err != nil {
			return fmt.Errorf("stream: cursor expiry: %w", err)
		}
	}
	return nil
}

// Check refuses a cursor issued for another provider, target or query. The
// revision is the provider's to judge (a change stream moves past it).
func (c Cursor) Check(provider, target, query string) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if c.Binding.Provider != provider || c.Binding.Target != target || c.Binding.Query != query {
		return ErrCursorBinding
	}
	return nil
}

// checkTrailer enforces the trailer's revision policy: a pagination cursor
// resumes one snapshot, so it must name the revision it was cut from; a
// change-stream cursor's revision is the provider's position, not ours.
func (c Cursor) checkTrailer(target, rev string) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if c.Binding.Target != target {
		return ErrCursorBinding
	}
	if c.Kind == KindPagination && c.Binding.Revision != rev {
		return fmt.Errorf("stream: pagination cursor revision %q does not match stream rev %q", c.Binding.Revision, rev)
	}
	return nil
}

// ParseCursor decodes and validates exactly one envelope (the --since input
// form); anything after it but whitespace is an error.
func ParseCursor(data []byte) (Cursor, error) {
	var c Cursor
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Cursor{}, fmt.Errorf("stream: cursor: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return Cursor{}, errors.New("stream: cursor: trailing data after the envelope")
	}
	return c, c.Validate()
}

// SnapshotCursor is the trailer of a complete snapshot: a pagination cursor
// whose token marks the end of revision rev for this binding. It is a pure
// function of the binding, so it adds no nondeterminism.
func SnapshotCursor(provider, target, query, rev string) Cursor {
	sum := sha256.Sum256([]byte(strings.Join([]string{CursorSchema, provider, target, query, rev, "end"}, "\x00")))
	return Cursor{
		SchemaVersion: CursorSchema,
		Kind:          KindPagination,
		Token:         hex.EncodeToString(sum[:16]),
		Binding:       Binding{Provider: provider, Target: target, Query: query, Revision: rev},
	}
}

// Graph is one decoded or to-be-encoded stream.
type Graph struct {
	Source string
	Rev    string
	Nodes  []provider.Node
	Edges  []provider.Edge
	Cursor *Cursor
	// Framed reports that the input began with a header (Decode only).
	Framed bool
}

type nodeRec struct {
	T string `json:"t"`
	provider.Node
}

type edgeRec struct {
	T string `json:"t"`
	provider.Edge
}

type cursorRec struct {
	T      string `json:"t"`
	Cursor Cursor `json:"cursor"`
}

// Canonicalize sorts nodes by ref and edges by (src, rel, dst), fills empty
// attrs and schema versions, and refuses duplicate node refs.
func (g *Graph) Canonicalize() error {
	sort.SliceStable(g.Nodes, func(i, j int) bool { return g.Nodes[i].Ref < g.Nodes[j].Ref })
	for i := range g.Nodes {
		n := &g.Nodes[i]
		if i > 0 && g.Nodes[i-1].Ref == n.Ref {
			return fmt.Errorf("stream: duplicate node ref %q", n.Ref)
		}
		if n.SchemaVersion == "" {
			n.SchemaVersion = provider.NodeSchema
		}
		if n.Attrs == nil {
			n.Attrs = map[string]any{}
		}
	}
	sort.SliceStable(g.Edges, func(i, j int) bool {
		a, b := g.Edges[i], g.Edges[j]
		if a.Src != b.Src {
			return a.Src < b.Src
		}
		if a.Rel != b.Rel {
			return a.Rel < b.Rel
		}
		return a.Dst < b.Dst
	})
	for i := range g.Edges {
		e := &g.Edges[i]
		if e.SchemaVersion == "" {
			e.SchemaVersion = provider.EdgeSchema
		}
		if e.Attrs == nil {
			e.Attrs = map[string]any{}
		}
	}
	return nil
}

// body is the canonical node and edge lines; rev hashes exactly these bytes.
func (g *Graph) body() ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	for _, n := range g.Nodes {
		if err := validNode(n); err != nil {
			return nil, err
		}
		if err := enc.Encode(nodeRec{T: "node", Node: n}); err != nil {
			return nil, err
		}
	}
	for _, e := range g.Edges {
		if err := validEdge(e); err != nil {
			return nil, err
		}
		if err := enc.Encode(edgeRec{T: "edge", Edge: e}); err != nil {
			return nil, err
		}
	}
	return b.Bytes(), nil
}

func revOf(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Encode canonicalizes g, sets g.Rev, and writes the framed stream. With no
// Cursor, the trailer is SnapshotCursor(providerID, g.Source, query, rev),
// computed fresh on every call and never stored back, so a Graph reused after
// its body changed cannot emit a stale binding. A caller-supplied Cursor must
// be bound to this provider, source and query, and — for pagination — to
// the revision being emitted.
func Encode(w io.Writer, g *Graph, providerID, query string) error {
	if g.Source == "" {
		return errors.New("stream: source locator is required")
	}
	if err := g.Canonicalize(); err != nil {
		return err
	}
	body, err := g.body()
	if err != nil {
		return err
	}
	g.Rev = revOf(body)
	trailer := SnapshotCursor(providerID, g.Source, query, g.Rev)
	if g.Cursor != nil {
		if err := g.Cursor.Check(providerID, g.Source, query); err != nil {
			return err
		}
		if err := g.Cursor.checkTrailer(g.Source, g.Rev); err != nil {
			return err
		}
		trailer = *g.Cursor
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(Header{T: "header", SchemaVersion: GraphSchema, Source: g.Source, Rev: g.Rev}); err != nil {
		return err
	}
	b.Write(body)
	if err := enc.Encode(cursorRec{T: "cursor", Cursor: trailer}); err != nil {
		return err
	}
	_, err = w.Write(b.Bytes())
	return err
}

// Verify reports whether the decoded records still hash to the header's rev,
// i.e. whether the stream arrived unfiltered and in canonical order.
func (g *Graph) Verify() error {
	if !g.Framed {
		return ErrNoHeader
	}
	body, err := g.body()
	if err != nil {
		return err
	}
	if got := revOf(body); got != g.Rev {
		return fmt.Errorf("stream: content %s does not match header rev %s (filtered or reordered)", got, g.Rev)
	}
	return nil
}

// DecodeOptions controls how much self-description Decode demands.
type DecodeOptions struct {
	// Declared is set when the caller declared the input schema explicitly;
	// a filtered record sequence — headerless, or framed but missing its
	// cursor trailer — is then accepted. A header that is present must be
	// well formed either way.
	Declared bool
}

// IsFramed reports whether data starts with a bashy-graph-v1 header record.
// It is the declared-header recognizer lens uses instead of guessing.
func IsFramed(data []byte) bool {
	var h struct {
		T             string `json:"t"`
		SchemaVersion string `json:"schema_version"`
	}
	if json.NewDecoder(bytes.NewReader(data)).Decode(&h) != nil {
		return false
	}
	return h.T == "header" && h.SchemaVersion == GraphSchema
}

// IsRecords reports whether data starts with a headerless stream record —
// what a filter that dropped the header leaves behind.
func IsRecords(data []byte) bool {
	var h struct {
		T             string `json:"t"`
		SchemaVersion string `json:"schema_version"`
	}
	if json.NewDecoder(bytes.NewReader(data)).Decode(&h) != nil {
		return false
	}
	switch h.T {
	case "node", "edge":
		return h.SchemaVersion == provider.NodeSchema || h.SchemaVersion == provider.EdgeSchema
	case "cursor":
		return true
	}
	return false
}

// Decode reads one stream. Records are JSON values in sequence — one per
// line as producers write them, or pretty-printed as `jq` without -c writes
// them. Records are validated individually; a header anywhere but first, a
// record after the cursor, or an unknown record type is an error. Undeclared
// input must be a complete frame: a header naming source and rev, and a
// cursor trailer bound to that source (and rev, for pagination).
func Decode(r io.Reader, o DecodeOptions) (*Graph, error) {
	g := &Graph{}
	dec := json.NewDecoder(r)
	first, done := true, false
	line := 0 // record number in diagnostics
	for {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("stream: record %d: %w", line+1, err)
		}
		line++
		var probe struct {
			T             string `json:"t"`
			SchemaVersion string `json:"schema_version"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			return nil, fmt.Errorf("stream: record %d: %w", line, err)
		}
		if done {
			return nil, fmt.Errorf("stream: record %d: record after the cursor trailer", line)
		}
		isFirst := first
		first = false
		switch probe.T {
		case "header":
			if !isFirst {
				return nil, fmt.Errorf("stream: record %d: header is not the first record", line)
			}
			if probe.SchemaVersion != GraphSchema {
				return nil, fmt.Errorf("stream: unsupported schema_version %q, want %q", probe.SchemaVersion, GraphSchema)
			}
			var h Header
			if err := json.Unmarshal(raw, &h); err != nil {
				return nil, fmt.Errorf("stream: record %d: %w", line, err)
			}
			if h.Source == "" {
				return nil, fmt.Errorf("stream: record %d: header has no source", line)
			}
			if !validRev(h.Rev) {
				return nil, fmt.Errorf("stream: record %d: header rev %q is not sha256:<64 hex>", line, h.Rev)
			}
			g.Framed, g.Source, g.Rev = true, h.Source, h.Rev
			continue
		}
		if isFirst && !o.Declared {
			return nil, ErrNoHeader
		}
		switch probe.T {
		case "node":
			var rec nodeRec
			if err := json.Unmarshal(raw, &rec); err != nil {
				return nil, fmt.Errorf("stream: record %d: %w", line, err)
			}
			if err := validNode(rec.Node); err != nil {
				return nil, fmt.Errorf("stream: record %d: %w", line, err)
			}
			g.Nodes = append(g.Nodes, rec.Node)
		case "edge":
			var rec edgeRec
			if err := json.Unmarshal(raw, &rec); err != nil {
				return nil, fmt.Errorf("stream: record %d: %w", line, err)
			}
			if err := validEdge(rec.Edge); err != nil {
				return nil, fmt.Errorf("stream: record %d: %w", line, err)
			}
			g.Edges = append(g.Edges, rec.Edge)
		case "cursor":
			var rec cursorRec
			if err := json.Unmarshal(raw, &rec); err != nil {
				return nil, fmt.Errorf("stream: record %d: %w", line, err)
			}
			if err := rec.Cursor.Validate(); err != nil {
				return nil, fmt.Errorf("stream: record %d: %w", line, err)
			}
			if g.Framed {
				if err := rec.Cursor.checkTrailer(g.Source, g.Rev); err != nil {
					return nil, fmt.Errorf("stream: record %d: %w", line, err)
				}
			}
			g.Cursor, done = &rec.Cursor, true
		default:
			return nil, fmt.Errorf("stream: record %d: unknown record type %q", line, probe.T)
		}
	}
	if first && !o.Declared {
		return nil, ErrNoHeader
	}
	if g.Framed && !done && !o.Declared {
		return nil, ErrNoTrailer
	}
	return g, nil
}

func validRev(rev string) bool {
	h, ok := strings.CutPrefix(rev, "sha256:")
	if !ok || len(h) != 2*sha256.Size {
		return false
	}
	_, err := hex.DecodeString(h)
	return err == nil
}

func validNode(n provider.Node) error {
	if n.SchemaVersion != provider.NodeSchema || n.Ref == "" || n.Kind == "" || n.Type == "" || n.Attrs == nil {
		return fmt.Errorf("stream: invalid %s record", provider.NodeSchema)
	}
	return nil
}

func validEdge(e provider.Edge) error {
	if e.SchemaVersion != provider.EdgeSchema || e.Src == "" || e.Rel == "" || e.Dst == "" || e.Attrs == nil {
		return fmt.Errorf("stream: invalid %s record", provider.EdgeSchema)
	}
	return nil
}

// maxAddressInput bounds what ReadAddresses buffers from stdin (a var so
// tests can lower it).
var maxAddressInput int64 = 64 << 20

// ReadAddresses is the consumer side of "commands take addresses in": stdin
// is either a stream (framed or a filtered record sequence — its node refs,
// in order) or plain addresses one per line. Blank lines are skipped. Input
// over maxAddressInput bytes is refused rather than truncated.
func ReadAddresses(r io.Reader) ([]string, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxAddressInput+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxAddressInput {
		return nil, fmt.Errorf("stream: address input exceeds %d bytes", maxAddressInput)
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, nil
	}
	if trimmed[0] == '{' {
		g, err := Decode(bytes.NewReader(trimmed), DecodeOptions{Declared: true})
		if err != nil {
			return nil, err
		}
		refs := make([]string, 0, len(g.Nodes))
		for _, n := range g.Nodes {
			refs = append(refs, n.Ref)
		}
		return refs, nil
	}
	var refs []string
	for _, l := range strings.Split(string(trimmed), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			refs = append(refs, l)
		}
	}
	return refs, nil
}
