package lens

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// The data rows share one order-preserving value model so a lossless row can
// promise A→B→A byte identity: map key order and number literals survive every
// hop. A value is one of:
//
//	nil            null
//	bool
//	json.Number    a number literal, kept verbatim
//	string
//	[]any          array
//	*Object        mapping, keys in document order
type Object struct {
	Keys []string
	Vals map[string]any
}

func newObject() *Object { return &Object{Vals: map[string]any{}} }

// set appends key (or replaces its value in place); it reports whether key was
// already present.
func (o *Object) set(key string, v any) bool {
	if _, dup := o.Vals[key]; dup {
		o.Vals[key] = v
		return true
	}
	o.Keys = append(o.Keys, key)
	o.Vals[key] = v
	return false
}

// losses accumulates the runtime reasons a conversion was not exact. A row's
// declared fidelity says what the route can do in general; losses say what
// this particular input lost.
type losses struct{ notes []string }

func (l *losses) add(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	for _, n := range l.notes {
		if n == msg {
			return
		}
	}
	l.notes = append(l.notes, msg)
}

// decodeJSON parses exactly one JSON value; trailing non-space content is an
// error.
func decodeJSON(data []byte, l *losses) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := decodeJSONValue(dec, l)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data after the JSON value")
	}
	return v, nil
}

// decodeJSONL parses one JSON value per non-blank line.
func decodeJSONL(data []byte, l *losses) ([]any, error) {
	var docs []any
	for i, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		v, err := decodeJSON([]byte(line), l)
		if err != nil {
			return nil, fmt.Errorf("line %d: %v", i+1, err)
		}
		docs = append(docs, v)
	}
	return docs, nil
}

func decodeJSONValue(dec *json.Decoder, l *losses) (any, error) {
	tok, err := dec.Token()
	if err == io.EOF {
		return nil, errors.New("empty input")
	}
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			obj := newObject()
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, _ := kt.(string)
				v, err := decodeJSONValue(dec, l)
				if err != nil {
					return nil, err
				}
				if obj.set(key, v) {
					l.add("duplicate key %q: last value kept", key)
				}
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return obj, nil
		case '[':
			arr := []any{}
			for dec.More() {
				v, err := decodeJSONValue(dec, l)
				if err != nil {
					return nil, err
				}
				arr = append(arr, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return arr, nil
		}
		return nil, fmt.Errorf("unexpected %v", t)
	default:
		return t, nil // nil, bool, json.Number, string
	}
}

// encodeJSON writes v plus a trailing newline: 2-space indented, or on one line
// when compact.
func encodeJSON(w *bytes.Buffer, v any, compact bool) {
	writeJSON(w, v, compact, "")
	w.WriteByte('\n')
}

func writeJSON(w *bytes.Buffer, v any, compact bool, indent string) {
	next := indent + "  "
	switch t := v.(type) {
	case nil:
		w.WriteString("null")
	case bool:
		if t {
			w.WriteString("true")
		} else {
			w.WriteString("false")
		}
	case json.Number:
		w.WriteString(string(t))
	case string:
		w.WriteString(jsonString(t))
	case []any:
		if len(t) == 0 {
			w.WriteString("[]")
			return
		}
		w.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				w.WriteByte(',')
			}
			if !compact {
				w.WriteString("\n" + next)
			}
			writeJSON(w, e, compact, next)
		}
		if !compact {
			w.WriteString("\n" + indent)
		}
		w.WriteByte(']')
	case *Object:
		if len(t.Keys) == 0 {
			w.WriteString("{}")
			return
		}
		w.WriteByte('{')
		for i, k := range t.Keys {
			if i > 0 {
				w.WriteByte(',')
			}
			if !compact {
				w.WriteString("\n" + next)
			}
			w.WriteString(jsonString(k))
			w.WriteByte(':')
			if !compact {
				w.WriteByte(' ')
			}
			writeJSON(w, t.Vals[k], compact, next)
		}
		if !compact {
			w.WriteString("\n" + indent)
		}
		w.WriteByte('}')
	default:
		w.WriteString(jsonString(fmt.Sprint(t)))
	}
}

func jsonString(s string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimSuffix(b.String(), "\n")
}

// compactJSON renders v on one line (used when a nested value has to land in
// a flat cell).
func compactJSON(v any) string {
	var b bytes.Buffer
	writeJSON(&b, v, true, "")
	return b.String()
}
