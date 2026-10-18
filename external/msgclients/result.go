package msgclients

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
// narrow the topic or the bounds and re-run).
type Result struct {
	Client      string `json:"client"`
	Mode        string `json:"mode"`
	Topic       string `json:"topic"`
	Records     []any  `json:"records"`
	RecordCount int    `json:"record_count"`
	BytesRead   int64  `json:"bytes_read"`
	Partial     bool   `json:"partial"`
	Reason      string `json:"reason,omitempty"` // "byte-limit" | "row-limit"
	Fidelity    string `json:"fidelity"`
	Mutation    string `json:"mutation"` // published | none
	Scope       string `json:"scope,omitempty"`
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
// invocation's bounds. Payloads are preserved verbatim as strings; numbers
// in provider JSON keep full precision (json.Number). A byte-limit cut
// drops the incomplete trailing record instead of erroring.
func Parse(client, topic string, r io.Reader, op Op) (*Result, *gws.Error) {
	op = op.withDefaults()
	data, overflow, err := readBounded(r, op.MaxBytes)
	if err != nil {
		return nil, gws.NewError(gws.CodeProviderFailure, client+": reading client output failed", nil)
	}
	res := &Result{Client: client, Mode: op.Mode, Topic: topic, BytesRead: int64(len(data)), Fidelity: clients[client].Fidelity, Records: []any{}}
	if overflow {
		res.Partial, res.Reason = true, "byte-limit"
	}
	if e := decode(client, res, data, overflow); e != nil {
		return nil, e
	}
	if len(res.Records) > op.MaxMessages {
		res.Records = res.Records[:op.MaxMessages]
		res.Partial, res.Reason = true, "row-limit"
	}
	res.RecordCount = len(res.Records)
	return res, nil
}

func decode(client string, res *Result, data []byte, overflow bool) *gws.Error {
	switch client {
	case "mosquitto":
		parseFramed(res, data, overflow)
		return nil
	default: // kcat -J, rabbitmqadmin get: JSON values with verbatim numbers
		return parseJSONValues(res, data, overflow)
	}
}

// parseFramed decodes mosquitto_sub -F '%t\x1f%p\x1e' output: records end
// at \x1e, topic and payload split at the first \x1f, so payloads may
// contain newlines and separators verbatim.
func parseFramed(res *Result, data []byte, overflow bool) {
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
	for _, rec := range strings.Split(s, "\x1e") {
		topic, payload, _ := strings.Cut(rec, "\x1f")
		res.Records = append(res.Records, map[string]any{"topic": topic, "payload": payload})
	}
}

// parseJSONValues decodes a stream of JSON values (kcat -J objects,
// rabbitmqadmin message objects) with numbers preserved verbatim
// (json.Number), so int64/decimal precision survives. A lone JSON array
// decodes as one value holding many messages and is flattened; on a
// byte-limit cut the incomplete trailing value is dropped, not an error.
func parseJSONValues(res *Result, data []byte, overflow bool) *gws.Error {
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
		if arr, ok := v.([]any); ok {
			res.Records = append(res.Records, arr...)
			continue
		}
		res.Records = append(res.Records, v)
	}
}

// Emit writes a result as a loom-v2 ok envelope and returns the exit code.
func Emit(w io.Writer, command string, res *Result) int {
	return weavecli.EmitOK(w, weavecli.OutputJSON, command, res)
}
