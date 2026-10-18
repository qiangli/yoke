package lens

import (
	"bytes"
	"strings"

	htmltomarkdown "github.com/JohannesKaufmann/html-to-markdown/v2"
	xhtml "golang.org/x/net/html"
)

// HTMLToMarkdown is the one html→md conversion: `lens -f html -t md` and
// `fetch --md` both call it, so the two never drift.
func HTMLToMarkdown(html string) (string, error) {
	return htmltomarkdown.ConvertString(html)
}

// htmlText renders HTML as plain text: the <title> on its own line, then the
// visible text with script/style/noscript removed.
//
// Ported from kg internal/extract/html (findTitle, stripText).
func htmlText(body []byte) (string, error) {
	root, err := xhtml.Parse(bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if title := findTitle(root); title != "" {
		b.WriteString(title)
		b.WriteString("\n\n")
	}
	if text := strings.TrimSpace(stripText(root)); text != "" {
		b.WriteString(text)
		b.WriteByte('\n')
	}
	return b.String(), nil
}

func findTitle(n *xhtml.Node) string {
	if n.Type == xhtml.ElementNode && n.Data == "title" && n.FirstChild != nil {
		return strings.TrimSpace(n.FirstChild.Data)
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if s := findTitle(c); s != "" {
			return s
		}
	}
	return ""
}

func stripText(n *xhtml.Node) string {
	var b strings.Builder
	var walk func(*xhtml.Node)
	walk = func(n *xhtml.Node) {
		if n.Type == xhtml.ElementNode {
			switch n.Data {
			case "script", "style", "noscript", "title":
				return
			}
		}
		if n.Type == xhtml.TextNode {
			s := strings.TrimSpace(n.Data)
			if s != "" {
				b.WriteString(s)
				b.WriteByte(' ')
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.TrimSpace(b.String())
}
