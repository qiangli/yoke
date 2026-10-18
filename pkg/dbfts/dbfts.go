package dbfts

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/qiangli/yoke/pkg/dblifecycle"
	"github.com/qiangli/yoke/pkg/dbstore"
)

// Operation names registered with dblifecycle.
const (
	OpSearch      = "fts.search"
	OpGet         = "fts.get"
	OpPut         = "fts.put"
	OpDelete      = "fts.delete"
	OpBatch       = "fts.batch"
	OpRebuild     = "fts.rebuild"
	OpCheck       = "fts.check"
	OpMappingShow = "fts.mapping.show"
	OpExport      = "fts.export"
	OpImport      = "fts.import"
)

// Bounds declared for the fts category (contract §6).
const (
	DefaultLimit    = 100
	MaxLimit        = 10000
	DefaultMaxBytes = 256 << 10
	MaxMaxBytes     = 8 << 20
)

// Input is the typed input for full-text search operations.
type Input struct {
	Query      string          `json:"query,omitempty"`
	Field      string          `json:"field,omitempty"`
	Filter     string          `json:"filter,omitempty"`
	ID         string          `json:"id,omitempty"`
	Doc        json.RawMessage `json:"doc,omitempty"`
	Documents  []DocumentEntry `json:"documents,omitempty"`
	Deletes    []string        `json:"deletes,omitempty"`
	Mapping    json.RawMessage `json:"mapping,omitempty"`
	Limit      int             `json:"limit,omitempty"`
	MaxBytes   int             `json:"max_bytes,omitempty"`
	Since      string          `json:"since,omitempty"`
	Out        string          `json:"out,omitempty"`
	In         string          `json:"in,omitempty"`
	OnConflict string          `json:"on_conflict,omitempty"`
	Replace    bool            `json:"replace,omitempty"`
	IfRevision string          `json:"if_revision,omitempty"`
}

// DocumentEntry is one document in a batch put.
type DocumentEntry struct {
	ID         string          `json:"id"`
	Doc        json.RawMessage `json:"doc"`
	IfRevision string          `json:"if_revision,omitempty"`
}

func decodeInput(raw json.RawMessage) (Input, error) {
	var in Input
	if len(raw) == 0 {
		return in, nil
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return in, fmt.Errorf("%w: fts input: %v", dbstore.ErrInvalidInput, err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return in, fmt.Errorf("%w: trailing fts input", dbstore.ErrInvalidInput)
	}
	return in, nil
}

func (in Input) bounds() (limit, maxBytes int, err error) {
	limit, maxBytes = in.Limit, in.MaxBytes
	if limit == 0 {
		limit = DefaultLimit
	}
	if maxBytes == 0 {
		maxBytes = DefaultMaxBytes
	}
	if limit < 0 || limit > MaxLimit || maxBytes < 0 || maxBytes > MaxMaxBytes {
		return 0, 0, fmt.Errorf("%w: --limit 1..%d and --max-bytes 1..%d", dbstore.ErrInvalidInput, MaxLimit, MaxMaxBytes)
	}
	return limit, maxBytes, nil
}

// Spec is the operation declaration shared by CLI, --explain, MCP and owner IPC.
func Spec(name string, mode dbstore.Mode, a dblifecycle.Access) dblifecycle.Operation {
	s := dblifecycle.Operation{
		Name:          name,
		MutationClass: "read",
		Hold:          "none",
		Atomic:        "op",
		DryRun:        "local-preview",
		Modes:         []string{"memory", "persistent"},
	}
	fs := func(tokens ...string) []string {
		if mode == dbstore.Persistent {
			return tokens
		}
		var out []string
		for _, t := range tokens {
			if !strings.HasPrefix(t, "fs.") {
				out = append(out, t)
			}
		}
		return out
	}
	switch a {
	case dblifecycle.AccessRead:
		s.Effects = fs("fs.read", "db.read")
	case dblifecycle.AccessWrite:
		s.MutationClass, s.Hold = "write", "shared-writer"
		s.Effects = fs("fs.write", "db.write")
	case dblifecycle.AccessExclusive:
		s.MutationClass, s.Hold = "destructive", "exclusive"
		s.Effects = fs("fs.write", "db.write", "db.delete")
	}
	switch name {
	case OpExport:
		s.Effects = append(fs("fs.read", "db.read"), "fs.write")
		s.MutationClass, s.Hold = "read", "exclusive"
	case OpImport:
		s.Effects = append([]string{"fs.read"}, s.Effects...)
	}
	return s
}
