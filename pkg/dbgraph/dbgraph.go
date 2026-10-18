// Package dbgraph exposes graph operations through the shared database lifecycle.
// Bonsai supplies persistent DQL; dbstore.MemGraph supplies typed in-memory
// nodes and edges. Memory mode deliberately has no DQL interpreter.
package dbgraph

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/qiangli/yoke/pkg/dbstore"
)

const (
	OpQuery       = "graph.query"
	OpMutate      = "graph.mutate"
	OpSchemaShow  = "graph.schema.show"
	OpSchemaApply = "graph.schema.apply"
	OpNodeGet     = "graph.node.get"
	OpNodePut     = "graph.node.put"
	OpNodeRm      = "graph.node.rm"
	OpEdgeGet     = "graph.edge.get"
	OpEdgePut     = "graph.edge.put"
	OpEdgeRm      = "graph.edge.rm"
	OpEdges       = "graph.edges"
	OpExport      = "graph.export"
	OpImport      = "graph.import"
)

// Input is the typed input shared by CLI, MCP, and owner requests. IDs and
// timestamps are never generated during admission: owner replay hashes this
// logical request before Run performs any mutation.
type Input struct {
	Query      string            `json:"query,omitempty"`
	Variables  map[string]string `json:"variables,omitempty"`
	SetNQuads  string            `json:"set_nquads,omitempty"`
	DelNQuads  string            `json:"del_nquads,omitempty"`
	Schema     string            `json:"schema,omitempty"`
	Node       *dbstore.Node     `json:"node,omitempty"`
	Edge       *dbstore.Edge     `json:"edge,omitempty"`
	ID         string            `json:"id,omitempty"`
	From       string            `json:"from,omitempty"`
	Type       string            `json:"type,omitempty"`
	To         string            `json:"to,omitempty"`
	Direction  string            `json:"direction,omitempty"`
	IfAbsent   bool              `json:"if_absent,omitempty"`
	IfRevision uint64            `json:"if_revision,omitempty"`
	Limit      int               `json:"limit,omitempty"`
	MaxBytes   int               `json:"max_bytes,omitempty"`
	Out        string            `json:"out,omitempty"`
	In         string            `json:"in,omitempty"`
	OnConflict string            `json:"on_conflict,omitempty"`
	Replace    bool              `json:"replace,omitempty"`
}

const (
	DefaultLimit    = 100
	MaxLimit        = 10000
	DefaultMaxBytes = 256 << 10
	MaxMaxBytes     = 8 << 20
)

func DecodeInput(raw json.RawMessage) (Input, error) {
	var in Input
	if len(raw) == 0 {
		return in, nil
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&in); err != nil {
		return in, fmt.Errorf("%w: graph input: %v", dbstore.ErrInvalidInput, err)
	}
	if _, err := d.Token(); err != io.EOF {
		return in, fmt.Errorf("%w: trailing graph input", dbstore.ErrInvalidInput)
	}
	return in, nil
}

func (in Input) bounds() (int, int, error) {
	l, b := in.Limit, in.MaxBytes
	if l == 0 {
		l = DefaultLimit
	}
	if b == 0 {
		b = DefaultMaxBytes
	}
	if l < 1 || l > MaxLimit || b < 1 || b > MaxMaxBytes {
		return 0, 0, fmt.Errorf("%w: graph limit or max_bytes outside supported range", dbstore.ErrInvalidInput)
	}
	return l, b, nil
}

func (in Input) only(op string, allowed ...string) error {
	b, _ := json.Marshal(in)
	var m map[string]json.RawMessage
	_ = json.Unmarshal(b, &m)
	for _, k := range allowed {
		delete(m, k)
	}
	if len(m) != 0 {
		for k := range m {
			return fmt.Errorf("%w: %s does not take %s", dbstore.ErrInvalidInput, op, k)
		}
	}
	return nil
}

func checkID(s string) error {
	if s == "" || len(s) > 512 || strings.ContainsRune(s, 0) {
		return fmt.Errorf("%w: graph identity must be 1..512 bytes without NUL", dbstore.ErrInvalidInput)
	}
	return nil
}
