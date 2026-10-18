package lens

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"time"
)

// Entry is one row of the uniform `-t listing` projection: the same five
// fields whatever the container, so an agent never has to remember `tar -t`
// versus `unzip -l`. Entries keep archive order.
type Entry struct {
	Path  string `json:"path"`
	Type  string `json:"type"` // file | dir | symlink | hardlink | other
	Size  int64  `json:"size"`
	Mode  string `json:"mode"`  // permission bits, octal
	Mtime string `json:"mtime"` // RFC 3339, UTC
}

// tarStream unwraps an optional gzip or bzip2 layer. pax archives are tar
// streams with pax headers, so they read the same way.
func tarStream(body []byte) (io.Reader, error) {
	switch {
	case bytes.HasPrefix(body, []byte{0x1f, 0x8b}):
		return gzip.NewReader(bytes.NewReader(body))
	case bytes.HasPrefix(body, []byte("BZh")):
		return bzip2.NewReader(bytes.NewReader(body)), nil
	}
	return bytes.NewReader(body), nil
}

func tarEntries(body []byte) ([]Entry, error) {
	r, err := tarStream(body)
	if err != nil {
		return nil, fmt.Errorf("tar: %w", err)
	}
	tr := tar.NewReader(r)
	var out []Entry
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("tar: %w", err)
		}
		typ := "other"
		switch hdr.Typeflag {
		case tar.TypeReg:
			typ = "file"
		case tar.TypeDir:
			typ = "dir"
		case tar.TypeSymlink:
			typ = "symlink"
		case tar.TypeLink:
			typ = "hardlink"
		}
		out = append(out, Entry{
			Path:  hdr.Name,
			Type:  typ,
			Size:  hdr.Size,
			Mode:  fmt.Sprintf("%04o", fs.FileMode(hdr.Mode).Perm()),
			Mtime: hdr.ModTime.UTC().Format(time.RFC3339),
		})
	}
	return out, nil
}

func zipEntries(body []byte) ([]Entry, error) {
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return nil, fmt.Errorf("zip: %w", err)
	}
	out := make([]Entry, 0, len(zr.File))
	for _, f := range zr.File {
		mode := f.Mode()
		typ := "file"
		switch {
		case mode.IsDir() || strings.HasSuffix(f.Name, "/"):
			typ = "dir"
		case mode&fs.ModeSymlink != 0:
			typ = "symlink"
		case !mode.IsRegular():
			typ = "other"
		}
		out = append(out, Entry{
			Path:  f.Name,
			Type:  typ,
			Size:  int64(f.UncompressedSize64),
			Mode:  fmt.Sprintf("%04o", mode.Perm()),
			Mtime: f.Modified.UTC().Format(time.RFC3339),
		})
	}
	return out, nil
}

func writeListing(w *bytes.Buffer, entries []Entry) {
	for _, e := range entries {
		b, _ := json.Marshal(e)
		w.Write(b)
		w.WriteByte('\n')
	}
}

// writeListingMarkdown is the `| path | bytes |` table from kg
// internal/extract/archive.
func writeListingMarkdown(w *bytes.Buffer, entries []Entry) {
	w.WriteString("| path | bytes |\n|---|---:|\n")
	for _, e := range entries {
		fmt.Fprintf(w, "| %s | %d |\n", strings.ReplaceAll(e.Path, "|", `\|`), e.Size)
	}
}
