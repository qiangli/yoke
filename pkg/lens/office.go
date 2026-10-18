package lens

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// Office Open XML fallbacks (docx, xlsx) over stdlib archive/zip +
// encoding/xml: read-only "give me the text", namespace-agnostic by local
// element name. Ported from kg internal/extract/office.

// docxMarkdown emits each non-empty paragraph as its own block.
func docxMarkdown(body []byte) (string, error) {
	text, err := docxPlainText(body)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String(), nil
}

func docxText(body []byte) (string, error) {
	text, err := docxPlainText(body)
	if err != nil || text == "" {
		return "", err
	}
	return text + "\n", nil
}

// docxPlainText walks every <w:t> of word/document.xml, one line per <w:p>.
// Headers, footers, footnotes and embedded objects are not read.
func docxPlainText(body []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return "", fmt.Errorf("docx: zip: %w", err)
	}
	var f *zip.File
	for _, zf := range zr.File {
		if zf.Name == "word/document.xml" {
			f = zf
			break
		}
	}
	if f == nil {
		return "", fmt.Errorf("docx: word/document.xml not found")
	}
	rc, err := f.Open()
	if err != nil {
		return "", fmt.Errorf("docx: open document.xml: %w", err)
	}
	defer rc.Close()
	return xmlStreamText(rc)
}

func xmlStreamText(r io.Reader) (string, error) {
	dec := xml.NewDecoder(r)
	var b strings.Builder
	inText := false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("docx: xml: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "t" {
				inText = true
			}
		case xml.EndElement:
			if t.Name.Local == "t" {
				inText = false
			}
			if t.Name.Local == "p" {
				b.WriteByte('\n')
			}
		case xml.CharData:
			if inText {
				b.Write(t)
			}
		}
	}
	return strings.TrimSpace(b.String()), nil
}

// xlsxMarkdown wraps the sheet text in a ```tsv fence.
func xlsxMarkdown(body []byte) (string, error) {
	text, err := xlsxPlainText(body)
	if err != nil || text == "" {
		return "", err
	}
	return "```tsv\n" + text + "\n```\n", nil
}

func xlsxText(body []byte) (string, error) {
	text, err := xlsxPlainText(body)
	if err != nil || text == "" {
		return "", err
	}
	return text + "\n", nil
}

// xlsxPlainText reads the shared-string pool, then every
// xl/worksheets/sheetN.xml in N order: cells tab-separated, one line per row,
// a blank line between sheets. Formulas yield their cached value.
func xlsxPlainText(body []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return "", fmt.Errorf("xlsx: zip: %w", err)
	}
	shared, err := xlsxReadSharedStrings(zr)
	if err != nil {
		return "", err
	}
	var sheets []*zip.File
	for _, zf := range zr.File {
		if strings.HasPrefix(zf.Name, "xl/worksheets/sheet") && strings.HasSuffix(zf.Name, ".xml") {
			sheets = append(sheets, zf)
		}
	}
	sort.SliceStable(sheets, func(i, j int) bool { return sheetNum(sheets[i].Name) < sheetNum(sheets[j].Name) })
	var parts []string
	for _, zf := range sheets {
		rc, err := zf.Open()
		if err != nil {
			return "", fmt.Errorf("xlsx: open %s: %w", zf.Name, err)
		}
		text, err := xlsxSheetText(rc, shared)
		rc.Close()
		if err != nil {
			return "", fmt.Errorf("xlsx: read %s: %w", zf.Name, err)
		}
		if text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n\n"), nil
}

func sheetNum(name string) int {
	n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "xl/worksheets/sheet"), ".xml"))
	if err != nil {
		return 1 << 30
	}
	return n
}

func xlsxReadSharedStrings(zr *zip.Reader) ([]string, error) {
	for _, zf := range zr.File {
		if zf.Name != "xl/sharedStrings.xml" {
			continue
		}
		rc, err := zf.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		dec := xml.NewDecoder(rc)
		var out []string
		var cur strings.Builder
		inSI, inT := false, false
		for {
			tok, err := dec.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("xlsx: sharedStrings: %w", err)
			}
			switch t := tok.(type) {
			case xml.StartElement:
				if t.Name.Local == "si" {
					inSI = true
					cur.Reset()
				}
				if t.Name.Local == "t" && inSI {
					inT = true
				}
			case xml.EndElement:
				if t.Name.Local == "t" {
					inT = false
				}
				if t.Name.Local == "si" {
					inSI = false
					out = append(out, cur.String())
				}
			case xml.CharData:
				if inT {
					cur.Write(t)
				}
			}
		}
		return out, nil
	}
	return nil, nil
}

// xlsxSheetText streams one worksheet. Shared-string cells (t="s") resolve
// through the pool; inline strings and numbers are emitted verbatim. Cells
// are placed by their r="B3" column so empty cells keep their position.
func xlsxSheetText(r io.Reader, shared []string) (string, error) {
	dec := xml.NewDecoder(r)
	var rows []string
	var row []string
	var cellType, cellRef string
	inV, inIS := false, false
	var cur strings.Builder
	flush := func() {
		raw := cur.String()
		cur.Reset()
		if cellType == "s" {
			if idx, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil && idx >= 0 && idx < len(shared) {
				raw = shared[idx]
			}
		}
		col := len(row)
		if c, ok := columnIndex(cellRef); ok && c >= col {
			col = c
		}
		for len(row) < col {
			row = append(row, "")
		}
		row = append(row, cellEscape(raw))
	}
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "row":
				row = nil
			case "c":
				cellType, cellRef = "", ""
				for _, a := range t.Attr {
					switch a.Name.Local {
					case "t":
						cellType = a.Value
					case "r":
						cellRef = a.Value
					}
				}
			case "v":
				inV = true
			case "is":
				inIS = true
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "v":
				inV = false
				flush()
			case "is":
				inIS = false
				flush()
			case "row":
				if len(row) > 0 {
					rows = append(rows, strings.Join(row, "\t"))
				}
			}
		case xml.CharData:
			if inV || inIS {
				cur.Write(t)
			}
		}
	}
	return strings.Join(rows, "\n"), nil
}

// columnIndex maps an A1-style reference to a 0-based column.
func columnIndex(ref string) (int, bool) {
	n := 0
	i := 0
	for ; i < len(ref) && ref[i] >= 'A' && ref[i] <= 'Z'; i++ {
		n = n*26 + int(ref[i]-'A'+1)
	}
	if i == 0 {
		return 0, false
	}
	return n - 1, true
}

func cellEscape(s string) string {
	return strings.NewReplacer("\t", " ", "\r\n", " ", "\n", " ", "\r", " ").Replace(s)
}
