package dbsql

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/qiangli/yoke/pkg/dbstore"
)

// tagged is the contract §8.3 lossless value encoding. Plain JSON strings
// and null pass through untagged; everything plain JSON would coerce is
// tagged.
type tagged struct {
	T    string  `json:"t"`
	V    any     `json:"v,omitempty"`
	B64  *string `json:"b64,omitempty"`
	Unit string  `json:"unit,omitempty"`
}

// encodeValue maps one SQLite storage-class value to its wire form.
// Text that is not valid UTF-8 cannot be a JSON string without silent
// replacement, so it is refused with a hint rather than coerced.
func encodeValue(v any) (any, bool, error) {
	switch x := v.(type) {
	case nil:
		return nil, false, nil
	case int64:
		return tagged{T: "int", V: strconv.FormatInt(x, 10)}, false, nil
	case float64:
		switch {
		case math.IsNaN(x):
			return tagged{T: "f64", V: "NaN"}, false, nil
		case math.IsInf(x, 1):
			return tagged{T: "f64", V: "Inf"}, false, nil
		case math.IsInf(x, -1):
			return tagged{T: "f64", V: "-Inf"}, false, nil
		}
		return tagged{T: "f64", V: x}, false, nil
	case string:
		if !utf8.ValidString(x) {
			return nil, false, fmt.Errorf("%w: text value is not valid UTF-8; select CAST(col AS BLOB) to read its bytes", dbstore.ErrUnsupported)
		}
		return x, false, nil
	case []byte:
		b64 := base64.StdEncoding.EncodeToString(x)
		return tagged{T: "bytes", B64: &b64}, false, nil
	case time.Time:
		// The driver parses TEXT in DATE/DATETIME/TIMESTAMP columns; the
		// stored spelling is not recoverable, so the caller adds a warning.
		return tagged{T: "ts", V: x.Format(time.RFC3339Nano), Unit: "ns"}, true, nil
	}
	return nil, false, fmt.Errorf("%w: unexpected driver value %T", dbstore.ErrBackend, v)
}

// decodeValue is the strict inverse for parameters and import records:
// untagged numbers and booleans are refused because no target type says how
// to read them exactly.
func decodeValue(raw json.RawMessage) (any, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, fmt.Errorf("%w: empty value", dbstore.ErrInvalidInput)
	}
	switch raw[0] {
	case 'n':
		if string(raw) == "null" {
			return nil, nil
		}
	case '"':
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("%w: %v", dbstore.ErrInvalidInput, err)
		}
		return s, nil
	case '{':
		var t struct {
			T    string          `json:"t"`
			V    json.RawMessage `json:"v"`
			B64  *string         `json:"b64"`
			Unit string          `json:"unit"`
			// Scale and Dtype belong to the dec and vec tags.
			Scale *int   `json:"scale"`
			Dtype string `json:"dtype"`
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&t); err != nil {
			return nil, fmt.Errorf("%w: tagged value: %v", dbstore.ErrInvalidInput, err)
		}
		return decodeTagged(t.T, t.V, t.B64)
	}
	return nil, fmt.Errorf("%w: untagged value %s; use {\"t\":\"int\",\"v\":\"N\"} or {\"t\":\"f64\",\"v\":N}", dbstore.ErrInvalidInput, clip(string(raw)))
}

func decodeTagged(tag string, v json.RawMessage, b64 *string) (any, error) {
	switch tag {
	case "int":
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return nil, fmt.Errorf("%w: int value must be a decimal string", dbstore.ErrInvalidInput)
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: int %q does not fit a SQLite INTEGER", dbstore.ErrInvalidInput, s)
		}
		return n, nil
	case "f64":
		var s string
		if json.Unmarshal(v, &s) == nil {
			switch s {
			case "NaN":
				return math.NaN(), nil
			case "Inf":
				return math.Inf(1), nil
			case "-Inf":
				return math.Inf(-1), nil
			}
			return nil, fmt.Errorf("%w: f64 string must be NaN, Inf or -Inf", dbstore.ErrInvalidInput)
		}
		var n json.Number
		dec := json.NewDecoder(bytes.NewReader(v))
		dec.UseNumber()
		if err := dec.Decode(&n); err != nil {
			return nil, fmt.Errorf("%w: f64 value must be a JSON number", dbstore.ErrInvalidInput)
		}
		f, err := strconv.ParseFloat(n.String(), 64)
		if err != nil {
			return nil, fmt.Errorf("%w: f64 %s: %v", dbstore.ErrInvalidInput, n, err)
		}
		return f, nil
	case "bytes":
		if b64 == nil {
			return nil, fmt.Errorf("%w: bytes value needs b64", dbstore.ErrInvalidInput)
		}
		b, err := base64.StdEncoding.DecodeString(*b64)
		if err != nil {
			return nil, fmt.Errorf("%w: bytes b64: %v", dbstore.ErrInvalidInput, err)
		}
		if b == nil {
			b = []byte{} // an empty blob is not NULL
		}
		return b, nil
	case "dec", "ts", "vec":
		return nil, fmt.Errorf("%w: value tag %q has no exact SQLite binding; bind text or int explicitly", dbstore.ErrUnsupported, tag)
	}
	return nil, fmt.Errorf("%w: unknown value tag %q", dbstore.ErrInvalidInput, tag)
}

func clip(s string) string {
	if len(s) > 40 {
		return s[:40] + "…"
	}
	return s
}

// decodeParams accepts a JSON array (positional) or object (named) of
// tagged values. Values are always bound, never interpolated.
func decodeParams(raw json.RawMessage) ([]any, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	switch raw[0] {
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, fmt.Errorf("%w: params: %v", dbstore.ErrInvalidInput, err)
		}
		args := make([]any, 0, len(items))
		for i, item := range items {
			v, err := decodeValue(item)
			if err != nil {
				return nil, fmt.Errorf("param %d: %w", i+1, err)
			}
			args = append(args, v)
		}
		return args, nil
	case '{':
		var items map[string]json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, fmt.Errorf("%w: params: %v", dbstore.ErrInvalidInput, err)
		}
		names := make([]string, 0, len(items))
		for name := range items {
			names = append(names, name)
		}
		sort.Strings(names)
		args := make([]any, 0, len(items))
		for _, name := range names {
			bare := strings.TrimLeft(name, ":@$")
			if !paramName.MatchString(bare) {
				return nil, fmt.Errorf("%w: param name %q", dbstore.ErrInvalidInput, name)
			}
			v, err := decodeValue(items[name])
			if err != nil {
				return nil, fmt.Errorf("param %s: %w", name, err)
			}
			args = append(args, sql.Named(bare, v))
		}
		return args, nil
	}
	return nil, fmt.Errorf("%w: params must be a JSON array or object", dbstore.ErrInvalidInput)
}

var paramName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ParseParam turns one CLI --param [NAME=]TYPE:VALUE into its tagged JSON
// value. TYPE is int, f64, text, bytes (base64) or null.
func ParseParam(s string) (name string, value json.RawMessage, err error) {
	if i := strings.IndexByte(s, '='); i > 0 && paramName.MatchString(s[:i]) {
		name, s = s[:i], s[i+1:]
	}
	typ, val, ok := strings.Cut(s, ":")
	if !ok && typ != "null" {
		return "", nil, fmt.Errorf("%w: --param %q wants TYPE:VALUE (int, f64, text, bytes, null)", dbstore.ErrInvalidInput, s)
	}
	var v any
	switch typ {
	case "int":
		v = tagged{T: "int", V: val}
	case "f64":
		if val == "NaN" || val == "Inf" || val == "-Inf" {
			v = tagged{T: "f64", V: val}
		} else {
			if _, perr := strconv.ParseFloat(val, 64); perr != nil {
				return "", nil, fmt.Errorf("%w: f64 %q", dbstore.ErrInvalidInput, val)
			}
			v = tagged{T: "f64", V: json.Number(val)}
		}
	case "text":
		v = val
	case "bytes":
		v = tagged{T: "bytes", B64: &val}
	case "null":
		if val != "" {
			return "", nil, fmt.Errorf("%w: null takes no value", dbstore.ErrInvalidInput)
		}
	default:
		return "", nil, fmt.Errorf("%w: --param type %q (int, f64, text, bytes, null)", dbstore.ErrInvalidInput, typ)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", nil, err
	}
	if _, err := decodeValue(b); err != nil {
		return "", nil, err
	}
	return name, b, nil
}
