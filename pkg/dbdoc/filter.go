package dbdoc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/qiangli/yoke/pkg/dbstore"
)

// decodeJSON strictly decodes one JSON value: numbers stay json.Number (their
// text verbatim), a duplicate object key is malformed rather than silently
// last-wins, and trailing data is refused.
func decodeJSON(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	v, err := decodeValue(dec, 0)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data after the JSON value")
	}
	return v, nil
}

const maxDepth = 256

func decodeValue(dec *json.Decoder, depth int) (any, error) {
	if depth > maxDepth {
		return nil, fmt.Errorf("nesting deeper than %d", maxDepth)
	}
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch d := tok.(type) {
	case json.Delim:
		if d == '[' {
			arr := []any{}
			for dec.More() {
				v, err := decodeValue(dec, depth+1)
				if err != nil {
					return nil, err
				}
				arr = append(arr, v)
			}
			_, err := dec.Token()
			return arr, err
		}
		obj := map[string]any{}
		for dec.More() {
			k, err := dec.Token()
			if err != nil {
				return nil, err
			}
			key := k.(string)
			if _, dup := obj[key]; dup {
				return nil, fmt.Errorf("duplicate object key %q", key)
			}
			if obj[key], err = decodeValue(dec, depth+1); err != nil {
				return nil, err
			}
		}
		_, err := dec.Token()
		return obj, err
	default:
		return tok, nil
	}
}

// encodeJSON is the stored form: compact, numbers verbatim, no HTML escaping.
func encodeJSON(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// object decodes a document or patch that must be a JSON object.
func object(what string, raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("%w: %s is required", dbstore.ErrInvalidInput, what)
	}
	if len(raw) > MaxDocBytes {
		return nil, fmt.Errorf("%w: %s is over %d bytes", dbstore.ErrLimitExceeded, what, MaxDocBytes)
	}
	v, err := decodeJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", dbstore.ErrInvalidInput, what, err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: %s must be a JSON object", dbstore.ErrInvalidInput, what)
	}
	return m, nil
}

// mergePatch applies an RFC 7386 JSON merge patch.
func mergePatch(target any, patch any) any {
	pm, ok := patch.(map[string]any)
	if !ok {
		return patch
	}
	tm, ok := target.(map[string]any)
	if !ok {
		tm = map[string]any{}
	}
	for k, v := range pm {
		if v == nil {
			delete(tm, k)
		} else {
			tm[k] = mergePatch(tm[k], v)
		}
	}
	return tm
}

// jsonPath turns a field path (a.b[0].c) into a SQLite JSON path. The
// grammar admits only [A-Za-z_][A-Za-z0-9_]* keys and decimal array
// indexes, so the result is safe to embed as a SQL string literal — which an
// expression index needs to match a query.
func jsonPath(field string) (string, error) {
	bad := fmt.Errorf("%w: field path %q (want key(.key|[N])*, keys [A-Za-z_][A-Za-z0-9_]*)", dbstore.ErrInvalidInput, field)
	if field == "" || len(field) > 256 {
		return "", bad
	}
	var b strings.Builder
	b.WriteString("$")
	i, wantKey := 0, true
	for i < len(field) {
		c := field[i]
		switch {
		case c == '[':
			j := strings.IndexByte(field[i:], ']')
			if wantKey || j < 2 || j > 10 {
				return "", bad
			}
			if _, err := strconv.ParseUint(field[i+1:i+j], 10, 32); err != nil {
				return "", bad
			}
			b.WriteString(field[i : i+j+1])
			i += j + 1
			wantKey = false
		case c == '.' && !wantKey:
			i++
			wantKey = true
			if i == len(field) {
				return "", bad
			}
		case wantKey && (c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'):
			j := i + 1
			for j < len(field) && (field[j] == '_' || field[j] >= 'a' && field[j] <= 'z' || field[j] >= 'A' && field[j] <= 'Z' || field[j] >= '0' && field[j] <= '9') {
				j++
			}
			b.WriteString("." + field[i:j])
			i = j
			wantKey = false
		default:
			return "", bad
		}
	}
	return b.String(), nil
}

func extract(path string) string { return "json_extract(doc, '" + path + "')" }
func jtype(path string) string   { return "json_type(doc, '" + path + "')" }

// filterNode is one node of the structured filter: a boolean combination or
// a field comparison whose value is a literal or a named parameter. Values
// are always bound, never interpolated.
type filterNode struct {
	And   []json.RawMessage `json:"and,omitempty"`
	Or    []json.RawMessage `json:"or,omitempty"`
	Not   json.RawMessage   `json:"not,omitempty"`
	Field string            `json:"field,omitempty"`
	Op    string            `json:"op,omitempty"`
	Value json.RawMessage   `json:"value,omitempty"`
	Param string            `json:"param,omitempty"`
}

type compiler struct {
	params map[string]json.RawMessage
	used   map[string]bool
	args   []any
	nodes  int
}

// compileFilter returns a SQL boolean expression over doc and its bound
// arguments. An empty filter matches every document.
func compileFilter(filter, params json.RawMessage) (string, []any, error) {
	c := &compiler{params: map[string]json.RawMessage{}, used: map[string]bool{}}
	if len(params) != 0 {
		dec := json.NewDecoder(bytes.NewReader(params))
		if err := dec.Decode(&c.params); err != nil {
			return "", nil, fmt.Errorf("%w: params must be a JSON object of values: %v", dbstore.ErrInvalidInput, err)
		}
	}
	expr := "1"
	if len(filter) != 0 && string(filter) != "null" {
		var err error
		if expr, err = c.node(filter); err != nil {
			return "", nil, err
		}
	}
	for name := range c.params {
		if !c.used[name] {
			return "", nil, fmt.Errorf("%w: param %q is not used by the filter", dbstore.ErrInvalidInput, name)
		}
	}
	return expr, c.args, nil
}

func (c *compiler) node(raw json.RawMessage) (string, error) {
	if c.nodes++; c.nodes > maxFilterNodes {
		return "", fmt.Errorf("%w: filter has more than %d nodes", dbstore.ErrInvalidInput, maxFilterNodes)
	}
	var n filterNode
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&n); err != nil {
		return "", fmt.Errorf("%w: filter: %v", dbstore.ErrInvalidInput, err)
	}
	kinds := 0
	for _, set := range []bool{n.And != nil, n.Or != nil, len(n.Not) != 0, n.Field != ""} {
		if set {
			kinds++
		}
	}
	if kinds != 1 {
		return "", fmt.Errorf("%w: a filter node is exactly one of and, or, not, or field/op/value", dbstore.ErrInvalidInput)
	}
	switch {
	case n.And != nil || n.Or != nil:
		parts, join := n.And, " and "
		if n.Or != nil {
			parts, join = n.Or, " or "
		}
		if len(parts) == 0 {
			return "", fmt.Errorf("%w: empty and/or", dbstore.ErrInvalidInput)
		}
		out := make([]string, len(parts))
		for i, p := range parts {
			s, err := c.node(p)
			if err != nil {
				return "", err
			}
			out[i] = s
		}
		return "(" + strings.Join(out, join) + ")", nil
	case len(n.Not) != 0:
		s, err := c.node(n.Not)
		if err != nil {
			return "", err
		}
		return "(not coalesce(" + s + ", 0))", nil
	}
	path, err := jsonPath(n.Field)
	if err != nil {
		return "", err
	}
	value := n.Value
	if n.Param != "" {
		if len(value) != 0 {
			return "", fmt.Errorf("%w: give value or param, not both", dbstore.ErrInvalidInput)
		}
		v, ok := c.params[n.Param]
		if !ok {
			return "", fmt.Errorf("%w: param %q is not given", dbstore.ErrInvalidInput, n.Param)
		}
		value, c.used[n.Param] = v, true
	}
	if n.Op == "exists" {
		var want bool
		if err := json.Unmarshal(value, &want); err != nil {
			return "", fmt.Errorf("%w: exists wants true or false", dbstore.ErrInvalidInput)
		}
		if want {
			return "(" + jtype(path) + " is not null)", nil
		}
		return "(" + jtype(path) + " is null)", nil
	}
	if len(value) == 0 {
		return "", fmt.Errorf("%w: %s on %q needs value or param", dbstore.ErrInvalidInput, n.Op, n.Field)
	}
	v, err := decodeJSON(value)
	if err != nil {
		return "", fmt.Errorf("%w: filter value: %v", dbstore.ErrInvalidInput, err)
	}
	switch n.Op {
	case "eq", "ne":
		s, err := c.compare(path, "=", v)
		if err != nil {
			return "", err
		}
		if n.Op == "ne" {
			s = "(not coalesce(" + s + ", 0))"
		}
		return s, nil
	case "lt", "lte", "gt", "gte":
		sym := map[string]string{"lt": "<", "lte": "<=", "gt": ">", "gte": ">="}[n.Op]
		switch v.(type) {
		case string, json.Number:
			return c.compare(path, sym, v)
		}
		return "", fmt.Errorf("%w: %s compares a string or a number", dbstore.ErrInvalidInput, n.Op)
	case "in":
		arr, ok := v.([]any)
		if !ok || len(arr) == 0 || len(arr) > 256 {
			return "", fmt.Errorf("%w: in wants an array of 1..256 scalar values", dbstore.ErrInvalidInput)
		}
		out := make([]string, len(arr))
		for i, item := range arr {
			if out[i], err = c.compare(path, "=", item); err != nil {
				return "", err
			}
		}
		return "(" + strings.Join(out, " or ") + ")", nil
	}
	return "", fmt.Errorf("%w: filter op %q (want eq, ne, lt, lte, gt, gte, in, exists)", dbstore.ErrInvalidInput, n.Op)
}

// compare matches the JSON type as well as the value, so the string "1",
// the number 1 and true never compare equal.
func (c *compiler) compare(path, sym string, v any) (string, error) {
	switch x := v.(type) {
	case nil:
		return "(" + jtype(path) + " = 'null')", nil
	case bool:
		if x {
			return "(" + jtype(path) + " = 'true')", nil
		}
		return "(" + jtype(path) + " = 'false')", nil
	case string:
		c.args = append(c.args, x)
		return "(" + jtype(path) + " = 'text' and " + extract(path) + " " + sym + " ?)", nil
	case json.Number:
		arg, err := number(x)
		if err != nil {
			return "", err
		}
		c.args = append(c.args, arg)
		return "(" + jtype(path) + " in ('integer', 'real') and " + extract(path) + " " + sym + " ?)", nil
	}
	return "", fmt.Errorf("%w: filters compare scalar values (string, number, boolean, null)", dbstore.ErrInvalidInput)
}

// number binds an integer as int64 and anything else as float64; an integer
// outside int64 is refused rather than rounded.
func number(n json.Number) (any, error) {
	s := n.String()
	if !strings.ContainsAny(s, ".eE") {
		i, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: integer %s is outside int64 and cannot be compared exactly", dbstore.ErrInvalidInput, s)
		}
		return i, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil, fmt.Errorf("%w: number %s", dbstore.ErrInvalidInput, s)
	}
	return f, nil
}
