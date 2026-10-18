package dataclients

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"

	"github.com/qiangli/coreutils/pkg/weavecli"
	"github.com/qiangli/yoke/external/gws"
)

// Result is one finite, bounded, typed result. Partial is explicit: a limit
// that stopped the read is never reported as completion, and there is no
// resume cursor (these clients offer no snapshot or continuation token —
// narrow the statement, e.g. with keyset predicates, and re-run).
type Result struct {
	Client      string   `json:"client"`
	Columns     []string `json:"columns,omitempty"`
	Records     []any    `json:"records"`
	RecordCount int      `json:"record_count"`
	BytesRead   int64    `json:"bytes_read"`
	Partial     bool     `json:"partial"`
	Reason      string   `json:"reason,omitempty"` // "byte-limit" | "row-limit"
	Fidelity    string   `json:"fidelity"`
	Mutation    string   `json:"mutation"` // none | rolled-back | committed
	Scope       string   `json:"scope,omitempty"`
}

// readBounded reads at most limit bytes and reports whether more was
// available. It stops reading at the limit instead of buffering the rest.
func readBounded(r io.Reader, limit int64) (data []byte, overflow bool, err error) {
	data, err = io.ReadAll(io.LimitReader(r, limit+1))
	if int64(len(data)) > limit {
		return data[:limit], true, err
	}
	return data, false, err
}

// Parse turns recorded or live client stdout into a typed Result under the
// invocation's bounds. nullToken is the psql NULL sentinel ("" otherwise).
func Parse(client string, r io.Reader, op Op, nullToken string) (*Result, *gws.Error) {
	op, _ = op.normalized2()
	data, overflow, err := readBounded(r, op.MaxBytes)
	if err != nil {
		return nil, gws.NewError(gws.CodeProviderFailure, client+": reading client output failed", nil)
	}
	res := &Result{Client: client, BytesRead: int64(len(data)), Fidelity: clients[client].Fidelity, Records: []any{}}
	if overflow {
		res.Partial, res.Reason = true, "byte-limit"
	}
	if client == "psql" {
		parsePsql(res, data, overflow, nullToken)
	} else if e := parseJSONStream(res, data, overflow); e != nil {
		return nil, e
	}
	if len(res.Records) > op.MaxRows {
		res.Records = res.Records[:op.MaxRows]
		res.Partial, res.Reason = true, "row-limit"
	}
	res.RecordCount = len(res.Records)
	return res, nil
}

// normalized2 applies defaults only (bounds were validated at Build).
func (o Op) normalized2() (Op, bool) {
	if o.MaxRows <= 0 {
		o.MaxRows = DefaultMaxRows
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = DefaultMaxBytes
	}
	return o, true
}

func parsePsql(res *Result, data []byte, overflow bool, nullToken string) {
	s := string(data)
	if overflow { // drop the cut-off tail record
		if i := strings.LastIndex(s, "\x1e"); i >= 0 {
			s = s[:i+1]
		} else {
			s = ""
		}
	}
	s = strings.TrimSuffix(strings.TrimRight(s, "\r\n"), "\x1e")
	if s == "" {
		return
	}
	recs := strings.Split(s, "\x1e")
	res.Columns = strings.Split(recs[0], "\x1f")
	for _, rec := range recs[1:] {
		fields := strings.Split(rec, "\x1f")
		row := make([]any, len(fields))
		for i, f := range fields {
			if nullToken != "" && f == nullToken {
				row[i] = nil
			} else {
				row[i] = f
			}
		}
		res.Records = append(res.Records, row)
	}
}

// parseJSONStream decodes a stream of JSON values with numbers preserved
// verbatim (json.Number), so int64/decimal precision survives. On a
// byte-limit cut the incomplete trailing value is dropped, not an error.
func parseJSONStream(res *Result, data []byte, overflow bool) *gws.Error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	for {
		var v any
		if err := dec.Decode(&v); err != nil {
			if err == io.EOF || overflow {
				return nil
			}
			return gws.NewError(gws.CodeProviderFailure, res.Client+": client output is not valid JSON — refusing to guess types", nil)
		}
		res.Records = append(res.Records, v)
	}
}

// Emit writes a result as a loom-v2 ok envelope and returns the exit code.
func Emit(w io.Writer, command string, res *Result) int {
	return weavecli.EmitOK(w, weavecli.OutputJSON, command, res)
}
