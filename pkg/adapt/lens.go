package adapt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/qiangli/yoke/pkg/lens"
)

// MaxOutputBytes bounds each buffered conversion/projection input and output.
// After overflow we drain without retaining bytes: a wrapper limit must not
// break the child's stdout pipe and thereby change its exit status.
const MaxOutputBytes = 8 << 20

var errOutput = errors.New("adapt output conversion unavailable, invalid or over limit")

// PrepareJSONLens implements PrepareLens using S3's published route table.
// Only declared cmd: parsers are accepted. No detection, PATH converter or
// private command parser is used. Route failure happens before target exec.
// S3 currently has no cmd: rows; those requests honestly refuse until supplied.
func PrepareJSONLens(ctx context.Context, from string, out io.Writer) (io.WriteCloser, error) {
	if !strings.HasPrefix(from, "cmd:") {
		return nil, errOutput
	}
	if _, err := lens.Route(from, "json"); err != nil {
		return nil, errOutput
	}
	return newBoundedOutput(ctx, out, func(data []byte) ([]byte, error) {
		result, err := lens.Convert(ctx, lens.Request{From: from, To: "json", Data: data})
		if err != nil {
			return nil, errOutput
		}
		return result.Data, nil
	}), nil
}

type boundedOutput struct {
	ctx     context.Context
	dst     io.Writer
	convert func([]byte) ([]byte, error)
	data    bytes.Buffer
	failed  bool
	closed  bool
}

func newBoundedOutput(ctx context.Context, dst io.Writer, convert func([]byte) ([]byte, error)) *boundedOutput {
	return &boundedOutput{ctx: ctx, dst: dst, convert: convert}
}
func (w *boundedOutput) Write(p []byte) (int, error) {
	if w.closed {
		return 0, errOutput
	}
	if w.failed {
		return len(p), nil
	}
	if w.ctx.Err() != nil || len(p) > MaxOutputBytes-w.data.Len() {
		w.failed = true
		w.data.Reset()
		return len(p), nil
	}
	return w.data.Write(p)
}
func (w *boundedOutput) Close() error {
	if w.closed {
		return errOutput
	}
	w.closed = true
	defer w.data.Reset()
	if w.failed || w.ctx.Err() != nil {
		return errOutput
	}
	data, err := w.convert(w.data.Bytes())
	// Never return parser errors, input excerpts, field values or recipe stderr.
	if err != nil || len(data) > MaxOutputBytes || w.ctx.Err() != nil {
		return errOutput
	}
	n, err := w.dst.Write(data)
	if err != nil || n != len(data) {
		return errOutput
	}
	return nil
}

func parseFields(value string) ([]string, error) {
	if len(value) == 0 || len(value) > 4096 {
		return nil, errors.New("invalid --fields selection")
	}
	fields := strings.Split(value, ",")
	if len(fields) > 128 {
		return nil, errors.New("too many --fields selections")
	}
	for _, field := range fields {
		if strings.TrimSpace(field) == "" {
			return nil, errors.New("invalid --fields selection")
		}
	}
	return fields, nil
}

// projectJSON handles a JSON object, an array of records, or consecutive JSON
// records (NDJSON). RawMessage preserves numeric precision and nested values.
// Fields are literal top-level keys; absent keys are omitted. Data is explicit
// stdout, not a log: only wrapper diagnostics are redacted.
func projectJSON(data []byte, fields []string) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	count := 0
	for {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err == io.EOF {
			break
		} else if err != nil {
			return nil, errOutput
		}
		var value any
		if len(raw) > 0 && raw[0] == '[' {
			var records []json.RawMessage
			if json.Unmarshal(raw, &records) != nil {
				return nil, errOutput
			}
			projected := make([]map[string]json.RawMessage, 0, len(records))
			for _, record := range records {
				v, err := projectRecord(record, fields)
				if err != nil {
					return nil, err
				}
				projected = append(projected, v)
			}
			value = projected
		} else {
			v, err := projectRecord(raw, fields)
			if err != nil {
				return nil, err
			}
			value = v
		}
		if err := enc.Encode(value); err != nil {
			return nil, errOutput
		}
		if out.Len() > MaxOutputBytes {
			return nil, errOutput
		}
		count++
	}
	if count == 0 {
		return nil, errOutput
	}
	return out.Bytes(), nil
}
func projectRecord(raw json.RawMessage, fields []string) (map[string]json.RawMessage, error) {
	var record map[string]json.RawMessage
	if json.Unmarshal(raw, &record) != nil || record == nil {
		return nil, errOutput
	}
	result := make(map[string]json.RawMessage, len(fields))
	for _, field := range fields {
		if v, ok := record[field]; ok {
			result[field] = v
		}
	}
	return result, nil
}
