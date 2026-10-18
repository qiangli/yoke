package lens

import (
	"bytes"
	"fmt"
	"strings"

	pdflib "github.com/ledongthuc/pdf"
)

// The pdf fallback is text-only: no OCR (an image-only PDF yields no text),
// no column or table reconstruction. Ported from kg internal/extract/pdf.

// pdfMarkdown emits one "## Page N" section per page that has text.
func pdfMarkdown(body []byte) (string, error) {
	pages, err := pdfTextByPage(body)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for i, page := range pages {
		if strings.TrimSpace(page) == "" {
			continue
		}
		fmt.Fprintf(&b, "## Page %d\n\n", i+1)
		b.WriteString(page)
		b.WriteString("\n\n")
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}

// pdfPlainText joins every page's text with newlines.
func pdfPlainText(body []byte) (string, error) {
	pages, err := pdfTextByPage(body)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, page := range pages {
		if page == "" {
			continue
		}
		b.WriteString(page)
		b.WriteByte('\n')
	}
	return b.String(), nil
}

func pdfTextByPage(body []byte) (pages []string, err error) {
	// ledongthuc/pdf panics on some malformed inputs; that is invalid input,
	// not a crash.
	defer func() {
		if r := recover(); r != nil {
			pages, err = nil, fmt.Errorf("pdf: %v", r)
		}
	}()
	r, err := pdflib.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return nil, fmt.Errorf("pdf: %w", err)
	}
	total := r.NumPage()
	out := make([]string, 0, total)
	for i := 1; i <= total; i++ {
		p := r.Page(i)
		if p.V.IsNull() {
			out = append(out, "")
			continue
		}
		text, err := p.GetPlainText(nil)
		if err != nil {
			out = append(out, "")
			continue
		}
		out = append(out, strings.TrimSpace(text))
	}
	return out, nil
}
