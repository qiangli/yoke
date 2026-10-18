package lens

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// decodeTOML parses a TOML document. BurntSushi decodes into Go maps, so key
// order is rebuilt from MetaData.Keys(), which lists every key in document
// order.
func decodeTOML(data []byte, l *losses) (any, error) {
	var m map[string]any
	md, err := toml.Decode(string(data), &m)
	if err != nil {
		return nil, err
	}
	order := map[string][]string{}
	seen := map[string]bool{}
	for _, k := range md.Keys() {
		parent := strings.Join(k[:len(k)-1], "\x00")
		id := parent + "\x01" + k[len(k)-1]
		if !seen[id] {
			seen[id] = true
			order[parent] = append(order[parent], k[len(k)-1])
		}
	}
	return fromTOML(m, nil, order, l), nil
}

func fromTOML(v any, path []string, order map[string][]string, l *losses) any {
	switch t := v.(type) {
	case map[string]any:
		obj := newObject()
		for _, k := range order[strings.Join(path, "\x00")] {
			if cv, ok := t[k]; ok {
				obj.set(k, fromTOML(cv, append(path[:len(path):len(path)], k), order, l))
			}
		}
		for k, cv := range t { // keys MetaData did not report (defensive)
			if _, ok := obj.Vals[k]; !ok {
				obj.set(k, fromTOML(cv, append(path[:len(path):len(path)], k), order, l))
			}
		}
		return obj
	case []map[string]any:
		arr := make([]any, 0, len(t))
		for _, e := range t {
			arr = append(arr, fromTOML(e, path, order, l))
		}
		return arr
	case []any:
		arr := make([]any, 0, len(t))
		for _, e := range t {
			arr = append(arr, fromTOML(e, path, order, l))
		}
		return arr
	case string, bool:
		return t
	case int64:
		return json.Number(strconv.FormatInt(t, 10))
	case float64:
		if math.IsInf(t, 0) || math.IsNaN(t) {
			l.add("toml %v has no JSON number form: kept as a string", t)
			return strconv.FormatFloat(t, 'g', -1, 64)
		}
		return json.Number(formatFloat(t))
	case time.Time:
		l.add("toml datetime converted to a string")
		switch t.Location().String() {
		case "datetime-local":
			return t.Format("2006-01-02T15:04:05.999999999")
		case "date-local":
			return t.Format("2006-01-02")
		case "time-local":
			return t.Format("15:04:05.999999999")
		}
		return t.Format(time.RFC3339Nano)
	}
	l.add("toml value %v converted to a string", v)
	return fmt.Sprint(v)
}

// encodeTOML renders a top-level table: plain keys first, then [tables] and
// [[arrays of tables]] in key order. TOML has no null: null members and
// elements are omitted and recorded as a loss.
func encodeTOML(w *bytes.Buffer, v any, l *losses) error {
	obj, ok := v.(*Object)
	if !ok {
		return errors.New("toml needs a table (JSON object) at the top level")
	}
	writeTOMLTable(w, nil, obj, l)
	return nil
}

func writeTOMLTable(w *bytes.Buffer, path []string, obj *Object, l *losses) {
	sawTable := false
	for _, k := range obj.Keys {
		if v := obj.Vals[k]; isTOMLTable(v) || isTOMLTableArray(v) {
			sawTable = true
		} else if sawTable && v != nil {
			l.add("toml writes tables after plain keys: key order changed")
			break
		}
	}
	for _, k := range obj.Keys {
		v := obj.Vals[k]
		if v == nil {
			l.add("toml has no null: key %q omitted", k)
			continue
		}
		if isTOMLTable(v) || isTOMLTableArray(v) {
			continue
		}
		w.WriteString(tomlKey(k) + " = " + tomlInline(v, l) + "\n")
	}
	for _, k := range obj.Keys {
		v := obj.Vals[k]
		sub := append(path[:len(path):len(path)], k)
		switch {
		case isTOMLTable(v):
			if w.Len() > 0 {
				w.WriteByte('\n')
			}
			w.WriteString("[" + tomlPath(sub) + "]\n")
			writeTOMLTable(w, sub, v.(*Object), l)
		case isTOMLTableArray(v):
			for _, e := range v.([]any) {
				if w.Len() > 0 {
					w.WriteByte('\n')
				}
				w.WriteString("[[" + tomlPath(sub) + "]]\n")
				writeTOMLTable(w, sub, e.(*Object), l)
			}
		}
	}
}

func isTOMLTable(v any) bool { _, ok := v.(*Object); return ok }

func isTOMLTableArray(v any) bool {
	arr, ok := v.([]any)
	if !ok || len(arr) == 0 {
		return false
	}
	for _, e := range arr {
		if _, ok := e.(*Object); !ok {
			return false
		}
	}
	return true
}

func tomlInline(v any, l *losses) string {
	switch t := v.(type) {
	case bool:
		return strconv.FormatBool(t)
	case json.Number:
		return string(t) // every JSON number literal is a valid TOML integer or float
	case string:
		return tomlString(t)
	case []any:
		var parts []string
		for _, e := range t {
			if e == nil {
				l.add("toml has no null: array element omitted")
				continue
			}
			parts = append(parts, tomlInline(e, l))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case *Object:
		var parts []string
		for _, k := range t.Keys {
			if t.Vals[k] == nil {
				l.add("toml has no null: key %q omitted", k)
				continue
			}
			parts = append(parts, tomlKey(k)+" = "+tomlInline(t.Vals[k], l))
		}
		if len(parts) == 0 {
			return "{}"
		}
		return "{ " + strings.Join(parts, ", ") + " }"
	}
	return tomlString(fmt.Sprint(v))
}

var bareKey = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func tomlKey(k string) string {
	if bareKey.MatchString(k) {
		return k
	}
	return tomlString(k)
}

func tomlPath(p []string) string {
	parts := make([]string, len(p))
	for i, k := range p {
		parts[i] = tomlKey(k)
	}
	return strings.Join(parts, ".")
}

func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}
