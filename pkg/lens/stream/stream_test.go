package stream

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/lens/provider"
)

func sample() *Graph {
	n := func(ref string) provider.Node {
		return provider.Node{Ref: ref, Path: ref, Name: ref, Kind: "symbol", Type: "code"}
	}
	e := func(src, rel, dst string) provider.Edge { return provider.Edge{Src: src, Rel: rel, Dst: dst} }
	// deliberately out of canonical order
	return &Graph{
		Source: "graph:repo",
		Nodes:  []provider.Node{n("graph:repo!/b"), n("graph:repo!/a")},
		Edges:  []provider.Edge{e("graph:repo!/b", "calls", "graph:repo!/a"), e("graph:repo!/a", "calls", "graph:repo!/b")},
	}
}

func encode(t *testing.T, g *Graph) string {
	t.Helper()
	var b bytes.Buffer
	if err := Encode(&b, g, "graph", "op=neighbors"); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestEncodeFramingOrderAndDeterminism(t *testing.T) {
	out := encode(t, sample())
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 6 {
		t.Fatalf("want header + 2 nodes + 2 edges + cursor, got %d lines:\n%s", len(lines), out)
	}
	if !strings.HasPrefix(lines[0], `{"t":"header","schema_version":"bashy-graph-v1","source":"graph:repo","rev":"sha256:`) {
		t.Errorf("header: %s", lines[0])
	}
	if !strings.Contains(lines[1], `"ref":"graph:repo!/a"`) || !strings.Contains(lines[3], `"src":"graph:repo!/a"`) {
		t.Errorf("records are not in canonical order:\n%s", out)
	}
	if !strings.HasPrefix(lines[5], `{"t":"cursor","cursor":{"schema_version":"bashy-cursor-v1","kind":"pagination"`) {
		t.Errorf("cursor trailer: %s", lines[5])
	}
	// Producer order must not leak: the reversed input encodes identically.
	g := sample()
	g.Nodes[0], g.Nodes[1] = g.Nodes[1], g.Nodes[0]
	g.Edges[0], g.Edges[1] = g.Edges[1], g.Edges[0]
	if again := encode(t, g); again != out {
		t.Fatalf("encoding depends on producer order:\n%s\n---\n%s", out, again)
	}
}

func TestDecodeRoundTripAndVerify(t *testing.T) {
	out := encode(t, sample())
	g, err := Decode(strings.NewReader(out), DecodeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !g.Framed || len(g.Nodes) != 2 || len(g.Edges) != 2 || g.Cursor == nil {
		t.Fatalf("decoded %#v", g)
	}
	if err := g.Verify(); err != nil {
		t.Fatal(err)
	}
	if g.Cursor.Binding.Revision != g.Rev || g.Cursor.Binding.Target != g.Source {
		t.Errorf("cursor not bound to the snapshot: %#v", g.Cursor.Binding)
	}
	var b bytes.Buffer
	if err := Encode(&b, &Graph{Source: g.Source, Nodes: g.Nodes, Edges: g.Edges}, "graph", "op=neighbors"); err != nil || b.String() != out {
		t.Fatalf("re-encoding a decoded stream is not identity (%v):\n%s", err, b.String())
	}
}

// The epic's filter rule: a filter that keeps the header keeps the stream
// self-describing (but its rev no longer verifies); one that drops it needs
// an explicit schema declaration.
func TestFilteredStreams(t *testing.T) {
	out := encode(t, sample())
	var keepHeader, dropHeader strings.Builder
	for _, l := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if strings.Contains(l, `"rel":"calls","dst":"graph:repo!/a"`) {
			continue // a jq select() that removes one edge
		}
		keepHeader.WriteString(l + "\n")
		if !strings.HasPrefix(l, `{"t":"header"`) {
			dropHeader.WriteString(l + "\n")
		}
	}
	g, err := Decode(strings.NewReader(keepHeader.String()), DecodeOptions{})
	if err != nil || len(g.Edges) != 1 {
		t.Fatalf("header-preserving filter: %v %#v", err, g)
	}
	if g.Verify() == nil {
		t.Error("a filtered stream must not verify against the producer's rev")
	}
	if _, err := Decode(strings.NewReader(dropHeader.String()), DecodeOptions{}); !errors.Is(err, ErrNoHeader) {
		t.Fatalf("headerless input must require a declared schema, got %v", err)
	}
	if !IsRecords([]byte(dropHeader.String())) || IsFramed([]byte(dropHeader.String())) {
		t.Error("headerless records misclassified")
	}
	g, err = Decode(strings.NewReader(dropHeader.String()), DecodeOptions{Declared: true})
	if err != nil || g.Framed || len(g.Nodes) != 2 {
		t.Fatalf("declared headerless input: %v %#v", err, g)
	}
	// jq without -c pretty-prints; records may span lines.
	pretty := strings.ReplaceAll(keepHeader.String(), `,"`, ",\n  \"")
	if g, err := Decode(strings.NewReader(pretty), DecodeOptions{}); err != nil || len(g.Nodes) != 2 {
		t.Fatalf("pretty-printed stream: %v", err)
	}
}

func TestDecodeRejectsMalformedFraming(t *testing.T) {
	out := encode(t, sample())
	lines := strings.SplitAfter(out, "\n")
	cases := map[string]string{
		"header not first":    lines[1] + lines[0],
		"record after cursor": out + lines[1],
		"unknown record":      lines[0] + `{"t":"blob"}` + "\n",
		"future version":      strings.Replace(lines[0], "bashy-graph-v1", "bashy-graph-v2", 1),
		"invalid node":        lines[0] + `{"t":"node","schema_version":"bashy-node-v1","ref":"x"}` + "\n",
		"invalid cursor":      lines[0] + `{"t":"cursor","cursor":{"schema_version":"bashy-cursor-v1","kind":"since","token":"x","binding":{"provider":"p","target":"t","query":"q"}}}` + "\n",
	}
	for name, in := range cases {
		if _, err := Decode(strings.NewReader(in), DecodeOptions{}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestCursorEnvelope(t *testing.T) {
	c := SnapshotCursor("graph", "graph:repo", "op=impact", "sha256:1")
	if c != SnapshotCursor("graph", "graph:repo", "op=impact", "sha256:1") {
		t.Fatal("snapshot cursor is not a pure function of its binding")
	}
	if c.Token == SnapshotCursor("graph", "graph:repo", "op=impact", "sha256:2").Token {
		t.Error("token must change with the revision")
	}
	if err := c.Check("graph", "graph:repo", "op=impact"); err != nil {
		t.Fatal(err)
	}
	for _, other := range [][3]string{{"dir", "graph:repo", "op=impact"}, {"graph", "graph:other", "op=impact"}, {"graph", "graph:repo", "op=neighbors"}} {
		if err := c.Check(other[0], other[1], other[2]); !errors.Is(err, ErrCursorBinding) {
			t.Errorf("cursor replayed against %v: %v", other, err)
		}
	}
	in := `{"schema_version":"bashy-cursor-v1","kind":"change-stream","token":"opaque","binding":{"provider":"bus","target":"bus:topic","query":"","revision":"7"},"expiry":"2026-10-18T00:00:00Z"}`
	if _, err := ParseCursor([]byte(in)); err == nil {
		t.Error("empty query binding accepted")
	}
	in = strings.Replace(in, `"query":""`, `"query":"topic=x"`, 1)
	if c, err := ParseCursor([]byte(in)); err != nil || c.Kind != KindChangeStream {
		t.Fatalf("valid change-stream cursor: %v", err)
	}
	for _, bad := range []string{
		strings.Replace(in, `"schema_version"`, `"version"`, 1), // pre-approval spelling
		strings.Replace(in, `"2026-10-18T00:00:00Z"`, `"tomorrow"`, 1),
		strings.Replace(in, `"token":"opaque"`, `"token":""`, 1),
	} {
		if _, err := ParseCursor([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestReadAddresses(t *testing.T) {
	out := encode(t, sample())
	got, err := ReadAddresses(strings.NewReader(out))
	if err != nil || strings.Join(got, ",") != "graph:repo!/a,graph:repo!/b" {
		t.Fatalf("stream addresses: %v %v", got, err)
	}
	got, err = ReadAddresses(strings.NewReader("\n zip:a.zip!/x \n\nsprint:446\n"))
	if err != nil || strings.Join(got, ",") != "zip:a.zip!/x,sprint:446" {
		t.Fatalf("plain addresses: %v %v", got, err)
	}
	if _, err := ReadAddresses(strings.NewReader(`{"t":"blob"}`)); err == nil {
		t.Fatal("malformed stream accepted as addresses")
	}
}

// A Graph reused across Encode calls must not carry the first call's trailer
// into the second: the cursor is derived per call, never stored back.
func TestEncodeReusedGraphGetsFreshCursor(t *testing.T) {
	g := sample()
	first := encode(t, g)
	if g.Cursor != nil {
		t.Fatal("Encode stored its derived cursor on the caller's Graph")
	}
	g.Nodes = g.Nodes[:1]
	g.Edges = nil
	second := encode(t, g)
	d, err := Decode(strings.NewReader(second), DecodeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Cursor.Binding.Revision != d.Rev || second == first {
		t.Fatalf("reused graph emitted a stale binding:\n%s", second)
	}
	// Same body, other query: the trailer must follow the query.
	var b bytes.Buffer
	if err := Encode(&b, g, "graph", "op=impact"); err != nil {
		t.Fatal(err)
	}
	if d, _ := Decode(&b, DecodeOptions{}); d == nil || d.Cursor.Binding.Query != "op=impact" {
		t.Fatalf("trailer not bound to the new query: %#v", d)
	}
}

func TestEncodeRefusesForeignOrStaleCursor(t *testing.T) {
	g := sample()
	encode(t, g)
	rev := g.Rev
	cases := map[string]Cursor{
		"other provider": SnapshotCursor("dir", g.Source, "op=neighbors", rev),
		"other target":   SnapshotCursor("graph", "graph:other", "op=neighbors", rev),
		"other query":    SnapshotCursor("graph", g.Source, "op=impact", rev),
		"stale revision": SnapshotCursor("graph", g.Source, "op=neighbors", "sha256:"+strings.Repeat("0", 64)),
	}
	for name, c := range cases {
		c := c
		g := sample()
		g.Cursor = &c
		if err := Encode(&bytes.Buffer{}, g, "graph", "op=neighbors"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A change-stream cursor's revision is the provider's position.
	g = sample()
	g.Cursor = &Cursor{SchemaVersion: CursorSchema, Kind: KindChangeStream, Token: "opaque",
		Binding: Binding{Provider: "graph", Target: g.Source, Query: "op=neighbors", Revision: "42"}}
	if err := Encode(&bytes.Buffer{}, g, "graph", "op=neighbors"); err != nil {
		t.Fatalf("change-stream trailer: %v", err)
	}
}

// Undeclared input must be a complete frame; a declared reader accepts the
// filtered shapes but never a malformed header.
func TestDecodeRequiresCompleteFrame(t *testing.T) {
	out := encode(t, sample())
	lines := strings.SplitAfter(out, "\n")
	noTrailer := strings.Join(lines[:5], "")
	if _, err := Decode(strings.NewReader(noTrailer), DecodeOptions{}); !errors.Is(err, ErrNoTrailer) {
		t.Fatalf("truncated stream: %v", err)
	}
	if g, err := Decode(strings.NewReader(noTrailer), DecodeOptions{Declared: true}); err != nil || !g.Framed || g.Cursor != nil {
		t.Fatalf("declared cursor-less filter: %v", err)
	}
	headerOnly := lines[0]
	if _, err := Decode(strings.NewReader(headerOnly), DecodeOptions{}); !errors.Is(err, ErrNoTrailer) {
		t.Fatalf("header-only stream: %v", err)
	}
	body := strings.Join(lines[1:], "")
	bad := map[string]string{
		"empty source": strings.Replace(lines[0], `"source":"graph:repo"`, `"source":""`, 1),
		"no source":    strings.Replace(lines[0], `"source":"graph:repo",`, ``, 1),
		"empty rev":    `{"t":"header","schema_version":"bashy-graph-v1","source":"graph:repo","rev":""}` + "\n",
		"short rev":    `{"t":"header","schema_version":"bashy-graph-v1","source":"graph:repo","rev":"sha256:abc"}` + "\n",
	}
	for name, h := range bad {
		for _, declared := range []bool{false, true} {
			if _, err := Decode(strings.NewReader(h+body), DecodeOptions{Declared: declared}); err == nil {
				t.Errorf("%s (declared=%v): accepted", name, declared)
			}
		}
	}
	// A trailer bound to another target or snapshot is a splice, not a frame.
	other := encode(t, &Graph{Source: "graph:other", Nodes: sample().Nodes})
	foreign := strings.SplitAfter(other, "\n")
	spliced := strings.Join(lines[:5], "") + foreign[len(foreign)-2]
	if _, err := Decode(strings.NewReader(spliced), DecodeOptions{Declared: true}); err == nil {
		t.Error("foreign cursor trailer accepted")
	}
}

func TestParseCursorRejectsTrailingData(t *testing.T) {
	c := SnapshotCursor("graph", "graph:repo", "op=impact", "sha256:1")
	b, _ := json.Marshal(c)
	if _, err := ParseCursor(append(append([]byte{}, b...), " \n"...)); err != nil {
		t.Fatalf("trailing whitespace: %v", err)
	}
	for _, tail := range []string{string(b), `{}`, `x`, `]`} {
		if _, err := ParseCursor(append(append([]byte{}, b...), tail...)); err == nil {
			t.Errorf("trailing %q accepted", tail)
		}
	}
}

func TestReadAddressesIsBounded(t *testing.T) {
	defer func(n int64) { maxAddressInput = n }(maxAddressInput)
	maxAddressInput = 16
	if got, err := ReadAddresses(strings.NewReader("sprint:446\n")); err != nil || len(got) != 1 {
		t.Fatalf("under the bound: %v %v", got, err)
	}
	if _, err := ReadAddresses(strings.NewReader(strings.Repeat("a\n", 9))); err == nil {
		t.Fatal("oversized input accepted")
	}
}

// The determinism contract is byte-level: repeated encodes of equal
// snapshots built in any order are identical, attrs maps included.
func TestEncodeByteDeterminism(t *testing.T) {
	mk := func(rev bool) *Graph {
		g := sample()
		g.Nodes[0].Attrs = map[string]any{"z": 1, "a": "x", "m": []any{"b", "a"}}
		if rev {
			g.Nodes[0], g.Nodes[1] = g.Nodes[1], g.Nodes[0]
		}
		return g
	}
	want := encode(t, mk(false))
	for i := 0; i < 20; i++ {
		if got := encode(t, mk(i%2 == 1)); got != want {
			t.Fatalf("run %d differs:\n%s\n---\n%s", i, want, got)
		}
	}
}
