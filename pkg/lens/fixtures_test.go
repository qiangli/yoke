package lens

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"fmt"
	"testing"
	"time"
)

// Binary fixtures are generated, not committed, so every byte of input is
// reviewable here and the goldens stay deterministic (fixed mtimes).

var fixtureTime = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

func zipOf(t *testing.T, files [][2]string) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for _, f := range files {
		h := &zip.FileHeader{Name: f[0], Method: zip.Deflate, Modified: fixtureTime}
		if f[0][len(f[0])-1] == '/' {
			h.SetMode(0o755 | 1<<31) // fs.ModeDir
		} else {
			h.SetMode(0o644)
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(f[1]))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func tarOf(t *testing.T, gz bool, format tar.Format) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, h := range []*tar.Header{
		{Name: "pkg/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "pkg/a.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 5},
		{Name: "pkg/link", Typeflag: tar.TypeSymlink, Linkname: "a.txt", Mode: 0o777},
		{Name: "pkg/hard", Typeflag: tar.TypeLink, Linkname: "pkg/a.txt", Mode: 0o644},
	} {
		h.ModTime = fixtureTime
		h.Format = format
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			tw.Write([]byte("hello"))
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if !gz {
		return b.Bytes()
	}
	var z bytes.Buffer
	zw := gzip.NewWriter(&z)
	zw.ModTime = fixtureTime
	zw.Write(b.Bytes())
	zw.Close()
	return z.Bytes()
}

func docxFixture(t *testing.T) []byte {
	return zipOf(t, [][2]string{
		{"[Content_Types].xml", `<?xml version="1.0"?><Types/>`},
		{"word/document.xml", `<?xml version="1.0" encoding="UTF-8"?>` +
			`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>` +
			`<w:p><w:r><w:t>Quarterly report</w:t></w:r></w:p>` +
			`<w:p><w:r><w:t xml:space="preserve">Revenue grew </w:t></w:r><w:r><w:t>12%.</w:t></w:r></w:p>` +
			`<w:p></w:p>` +
			`<w:p><w:r><w:t>Costs &amp; risks</w:t></w:r></w:p>` +
			`</w:body></w:document>`},
	})
}

func xlsxFixture(t *testing.T) []byte {
	return zipOf(t, [][2]string{
		{"[Content_Types].xml", `<?xml version="1.0"?><Types/>`},
		{"xl/workbook.xml", `<?xml version="1.0"?><workbook/>`},
		{"xl/sharedStrings.xml", `<?xml version="1.0"?><sst><si><t>region</t></si><si><t>revenue</t></si><si><t>north</t></si><si><r><t>so</t></r><r><t>uth</t></r></si></sst>`},
		// sheet10 sorts before sheet2 in zip order; output must follow N.
		{"xl/worksheets/sheet10.xml", `<?xml version="1.0"?><worksheet><sheetData><row r="1"><c r="A1" t="inlineStr"><is><t>tenth</t></is></c></row></sheetData></worksheet>`},
		{"xl/worksheets/sheet1.xml", `<?xml version="1.0"?><worksheet><sheetData>` +
			`<row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1" t="s"><v>1</v></c></row>` +
			`<row r="2"><c r="A2" t="s"><v>2</v></c><c r="B2"><v>100</v></c></row>` +
			`<row r="3"><c r="A3" t="s"><v>3</v></c><c r="C3"><f>B2*2</f><v>200</v></c></row>` +
			`</sheetData></worksheet>`},
		{"xl/worksheets/sheet2.xml", `<?xml version="1.0"?><worksheet><sheetData><row r="1"><c r="A1" t="inlineStr"><is><t>second</t></is></c></row></sheetData></worksheet>`},
	})
}

// pdfFixture writes a minimal two-page PDF 1.4 with a correct xref table.
func pdfFixture() []byte {
	page := func(text string) string {
		return fmt.Sprintf("BT /F1 12 Tf 72 720 Td (%s) Tj ET", text)
	}
	c1, c2 := page("Hello lens"), page("Second page")
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R 4 0 R] /Count 2 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 5 0 R >> >> /Contents 6 0 R >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 5 0 R >> >> /Contents 7 0 R >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(c1), c1),
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(c2), c2),
	}
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs))
	for i, o := range objs {
		offsets[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return b.Bytes()
}
