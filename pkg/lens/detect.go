package lens

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/qiangli/yoke/pkg/lens/stream"
)

// aliases maps every accepted -f/-t spelling to its canonical format name.
var aliases = map[string]string{
	"json": "json", "jsonl": "jsonl", "ndjson": "jsonl",
	"yaml": "yaml", "yml": "yaml", "toml": "toml",
	"csv": "csv", "tsv": "tsv",
	"md": "md", "markdown": "md", "txt": "txt", "text": "txt",
	"html": "html", "htm": "html",
	"pdf": "pdf", "docx": "docx", "xlsx": "xlsx", "pptx": "pptx",
	"tar": "tar", "pax": "tar", "tgz": "tar", "zip": "zip",
	"listing": "listing",
	"cmd:ps":  "cmd:ps", "cmd:df": "cmd:df", "cmd:ls-l": "cmd:ls-l",
	"cmd:du": "cmd:du", "cmd:wc": "cmd:wc", "cmd:stat": "cmd:stat",
	"cmd:id": "cmd:id", "cmd:uname": "cmd:uname", "cmd:tar-tv": "cmd:tar-tv",
	// command lines (argv.go); json and yaml above double as the record rows
	"bash": "bash", "posix": "posix", "sh": "posix", "bsh": "bsh", "bashpp": "bsh",
	"cmd": "cmd", "pwsh": "pwsh", "powershell": "pwsh",
	"mcp-tool": "mcp-tool", "mcp-call": "mcp-call", "skill": "skill",
	"graph": "graph", "bashy-graph-v1": "graph",
	"edges": "edges", "paths": "paths", "tree": "tree", "outline": "outline",
	"dot": "dot", "mermaid": "mermaid",
}

// Canonical returns the canonical format name for a -f/-t spelling, or "".
func Canonical(name string) string { return aliases[strings.ToLower(name)] }

// byExtension resolves a file name to a format (the first half of the FROM
// rule: extension, then magic — as filebrowser/files/file.go detectType does).
func byExtension(name string) string {
	lower := strings.ToLower(filepath.Base(name))
	for _, suffix := range []string{".tar.gz", ".tar.bz2", ".tbz2", ".tbz"} {
		if strings.HasSuffix(lower, suffix) {
			return "tar"
		}
	}
	ext := strings.TrimPrefix(filepath.Ext(lower), ".")
	switch ext {
	case "tab":
		return "tsv"
	case "xhtml":
		return "html"
	}
	f := aliases[ext]
	if f == "listing" || strings.HasPrefix(f, "cmd:") || argvOnly[f] {
		return "" // command lines and command output are never guessed: they need -f
	}
	switch f {
	case "graph", "edges", "paths", "tree", "outline", "dot", "mermaid":
		return "" // graph streams are declared by their header; views are outputs
	}
	return f
}

// byMagic sniffs content. Only signatures that cannot be mistaken are used;
// yaml, toml, csv and tsv are never guessed from content.
func byMagic(data []byte) string {
	switch {
	case bytes.HasPrefix(data, []byte("%PDF-")):
		return "pdf"
	case bytes.HasPrefix(data, []byte("PK\x03\x04")), bytes.HasPrefix(data, []byte("PK\x05\x06")):
		return zipKind(data)
	case bytes.HasPrefix(data, []byte{0x1f, 0x8b}), bytes.HasPrefix(data, []byte("BZh")):
		if r, err := tarStream(data); err == nil {
			head := make([]byte, 512)
			if n, _ := io.ReadFull(r, head); n == 512 && isTarHeader(head) {
				return "tar"
			}
		}
		return ""
	case isTarHeader(data):
		return "tar"
	}
	text := bytes.TrimLeft(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), " \t\r\n")
	if len(text) == 0 {
		return ""
	}
	if text[0] == '{' || text[0] == '[' {
		if json.Valid(text) {
			return "json"
		}
		if _, err := decodeJSONL(text, &losses{}); err == nil {
			return "jsonl"
		}
		return ""
	}
	if strings.HasPrefix(http.DetectContentType(text), "text/html") {
		return "html"
	}
	return ""
}

func isTarHeader(b []byte) bool {
	return len(b) >= 263 && bytes.Equal(b[257:262], []byte("ustar"))
}

// zipKind tells the Office Open XML containers apart from a plain zip.
func zipKind(data []byte) string {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "zip"
	}
	for _, f := range zr.File {
		switch f.Name {
		case "word/document.xml":
			return "docx"
		case "xl/workbook.xml":
			return "xlsx"
		case "ppt/presentation.xml":
			return "pptx"
		}
	}
	return "zip"
}

// Detect resolves FROM for an input. A declared bashy-graph-v1 header wins
// (it is the stream's own statement of schema, like a fence info string);
// then the file name's extension, then content magic. It returns "" when
// none is conclusive.
func Detect(name string, data []byte) string {
	if text := bytes.TrimLeft(data, " \t\r\n"); len(text) > 0 && text[0] == '{' && stream.IsFramed(text) {
		return "graph"
	}
	if f := byExtension(name); f != "" {
		return f
	}
	return byMagic(data)
}
