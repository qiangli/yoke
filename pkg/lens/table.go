package lens

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"strings"
)

func delimiter(format string) rune {
	if format == "tsv" {
		return '\t'
	}
	return ','
}

// readRecords parses csv or tsv. Rows may be ragged; callers decide whether
// that loses anything.
func readRecords(data []byte, format string) ([][]string, error) {
	r := csv.NewReader(bytes.NewReader(data))
	r.Comma = delimiter(format)
	r.FieldsPerRecord = -1
	r.LazyQuotes = format == "tsv"
	return r.ReadAll()
}

func writeRecords(w *bytes.Buffer, records [][]string, format string) error {
	cw := csv.NewWriter(w)
	cw.Comma = delimiter(format)
	if err := cw.WriteAll(records); err != nil {
		return err
	}
	return cw.Error()
}

// recordsToJSON maps a header + rows to an array of objects whose values are
// all strings, in header order.
func recordsToJSON(records [][]string, l *losses) any {
	arr := []any{}
	if len(records) == 0 {
		return arr
	}
	header := records[0]
	seen := map[string]bool{}
	for _, h := range header {
		if seen[h] {
			l.add("duplicate column %q: last value kept", h)
		}
		seen[h] = true
	}
	for i, row := range records[1:] {
		if len(row) != len(header) {
			l.add("row %d has %d fields, header has %d", i+2, len(row), len(header))
		}
		obj := newObject()
		for j, h := range header {
			cell := ""
			if j < len(row) {
				cell = row[j]
			}
			obj.set(h, cell)
		}
		arr = append(arr, obj)
	}
	return arr
}

// jsonToRecords flattens an array of objects to a header (union of keys in
// first-seen order) plus rows. Anything that is not a string cell is a loss:
// csv cannot tell the reader it was a number, null or nested value.
func jsonToRecords(v any, l *losses) ([][]string, error) {
	arr, ok := v.([]any)
	if !ok {
		return nil, errors.New("csv/tsv needs a JSON array of objects")
	}
	var header []string
	index := map[string]int{}
	for _, e := range arr {
		obj, ok := e.(*Object)
		if !ok {
			return nil, errors.New("csv/tsv needs a JSON array of objects")
		}
		for _, k := range obj.Keys {
			if _, ok := index[k]; !ok {
				index[k] = len(header)
				header = append(header, k)
			}
		}
	}
	if len(arr) == 0 {
		return nil, nil
	}
	records := [][]string{header}
	for _, e := range arr {
		obj := e.(*Object)
		row := make([]string, len(header))
		if len(obj.Keys) != len(header) {
			l.add("missing keys written as empty cells")
		}
		for _, k := range obj.Keys {
			row[index[k]] = cellString(obj.Vals[k], l)
		}
		records = append(records, row)
	}
	return records, nil
}

func cellString(v any, l *losses) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		l.add("null written as an empty cell")
		return ""
	case bool:
		l.add("non-string values written as text")
		if t {
			return "true"
		}
		return "false"
	case json.Number:
		l.add("non-string values written as text")
		return string(t)
	}
	l.add("nested values written as compact JSON text")
	return compactJSON(v)
}

// recordsToMarkdown renders a GitHub-flavored pipe table; the first record is
// the header.
func recordsToMarkdown(w *bytes.Buffer, records [][]string, l *losses) {
	if len(records) == 0 {
		return
	}
	width := 0
	for _, r := range records {
		width = max(width, len(r))
	}
	row := func(cells []string) {
		w.WriteString("|")
		for i := 0; i < width; i++ {
			c := ""
			if i < len(cells) {
				c = cells[i]
			}
			w.WriteString(" " + mdCell(c, l) + " |")
		}
		w.WriteString("\n")
	}
	row(records[0])
	w.WriteString("|")
	for i := 0; i < width; i++ {
		w.WriteString(" --- |")
	}
	w.WriteString("\n")
	for _, r := range records[1:] {
		row(r)
	}
}

func mdCell(s string, l *losses) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "|", `\|`)
	if strings.ContainsAny(s, "\r\n") {
		l.add("line breaks inside cells written as <br>")
		s = strings.ReplaceAll(s, "\r\n", "<br>")
		s = strings.ReplaceAll(s, "\n", "<br>")
		s = strings.ReplaceAll(s, "\r", "<br>")
	}
	return s
}
