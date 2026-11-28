package broker

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// MaxInlineReadBytes caps inline bytes returned by a files-NAME "read" call;
// larger reads ask the caller to mint a link and stream GET /files/NAME/PATH.
const MaxInlineReadBytes = 256 << 10

// FilesInputSchemaJSON is the typed JSON Schema for a files-NAME tool call.
// K2 folds this into the generic offer definition.
const FilesInputSchemaJSON = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["op"],
  "properties": {
    "op": {
      "type": "string",
      "enum": ["ls", "stat", "read", "link"],
      "description": "Operation: list a directory (ls), stat one path (stat), read inline bytes up to 256 KiB (read), or return a bashy:// stream URI (link)."
    },
    "path": {
      "type": "string",
      "description": "Relative path inside the shared folder (empty or '.' for the folder root)."
    },
    "offset": {
      "type": "integer",
      "minimum": 0,
      "description": "Byte offset for read (default 0)."
    },
    "length": {
      "type": "integer",
      "minimum": 0,
      "description": "Maximum byte count for read (up to 262144; 0 reads up to the cap)."
    }
  }
}`

// FilesOutputSchemaJSON describes the structured result of a files-NAME call.
const FilesOutputSchemaJSON = `{
  "type": "object",
  "required": ["op", "share"],
  "properties": {
    "op": {"type": "string", "enum": ["ls", "stat", "read", "link"]},
    "share": {"type": "string"},
    "path": {"type": "string"},
    "entries": {"type": "array", "items": {"type": "object"}},
    "name": {"type": "string"},
    "type": {"type": "string", "enum": ["file", "dir"]},
    "is_dir": {"type": "boolean"},
    "size": {"type": "integer"},
    "mtime": {"type": "string"},
    "etag": {"type": "string"},
    "offset": {"type": "integer"},
    "length": {"type": "integer"},
    "data": {"type": "string", "contentEncoding": "base64"},
    "eof": {"type": "boolean"},
    "uri": {"type": "string"}
  }
}`

// FileShareAudience records the intended audience of a shared folder.
type FileShareAudience struct {
	Circles []string `json:"circles,omitempty"`
	Groups  []string `json:"groups,omitempty"`
	Emails  []string `json:"emails,omitempty"`
}

// FileShare is one <bashy-home>/shared/NAME.json record.
type FileShare struct {
	Name     string            `json:"name"`
	Path     string            `json:"path,omitempty"`
	Mode     string            `json:"mode,omitempty"`
	Audience FileShareAudience `json:"audience,omitempty"`
	Created  string            `json:"created,omitempty"`
	Note     string            `json:"note,omitempty"`
	Forced   bool              `json:"forced,omitempty"`
	Reason   string            `json:"reason,omitempty"`
}

// FileToolDefinition is the typed tool definition served at GET /files/NAME?definition.
type FileToolDefinition struct {
	Name         string          `json:"name"`
	Class        string          `json:"class"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"input_schema"`
	OutputSchema json.RawMessage `json:"output_schema"`
	Effects      []string        `json:"effects"`
	Unit         string          `json:"unit"`
	Digest       string          `json:"digest,omitempty"`
}

// FileToolCall is the JSON request body of POST /files/NAME.
type FileToolCall struct {
	Op     string `json:"op"`
	Path   string `json:"path,omitempty"`
	Offset int64  `json:"offset,omitempty"`
	Length int64  `json:"length,omitempty"`
}

// FileEntry describes one file or directory inside a shared folder.
type FileEntry struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Type    string `json:"type"`
	IsDir   bool   `json:"is_dir"`
	Size    int64  `json:"size"`
	ModTime string `json:"mtime"`
	ETag    string `json:"etag,omitempty"`
}

// FileListResult is returned by op "ls" and by GET on a directory path.
type FileListResult struct {
	Op      string      `json:"op"`
	Share   string      `json:"share"`
	Path    string      `json:"path"`
	Entries []FileEntry `json:"entries"`
}

// FileStatResult is returned by op "stat".
type FileStatResult struct {
	Op    string `json:"op"`
	Share string `json:"share"`
	FileEntry
}

// FileReadResult is returned by op "read". Data holds raw bytes (base64 in JSON).
type FileReadResult struct {
	Op     string `json:"op"`
	Share  string `json:"share"`
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Offset int64  `json:"offset"`
	Length int    `json:"length"`
	Data   []byte `json:"data"`
	EOF    bool   `json:"eof"`
	ETag   string `json:"etag"`
}

// FileLinkResult is returned by op "link".
type FileLinkResult struct {
	Op    string `json:"op"`
	Share string `json:"share"`
	Path  string `json:"path"`
	URI   string `json:"uri"`
	Size  int64  `json:"size"`
	IsDir bool   `json:"is_dir"`
	ETag  string `json:"etag,omitempty"`
}

var shareNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// ValidShareName reports whether name may identify a shared folder record.
func ValidShareName(name string) bool {
	return shareNameRE.MatchString(name) && !strings.Contains(name, "..")
}

// doorHostName returns this door's host name for bashy:// URIs (a seam for tests).
var doorHostName = thisHostName

// filesBashyHome resolves $BASHY_HOME or ~/.bashy, matching door.StateDir.
func filesBashyHome() (string, error) {
	if h := strings.TrimSpace(os.Getenv("BASHY_HOME")); h != "" {
		return h, nil
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, ".bashy"), nil
}

type shareErr struct {
	status int
	msg    string
}

func (e *shareErr) Error() string { return e.msg }

// loadFileShare reads <bashy-home>/shared/NAME.json fresh per request and
// returns the record and its backing directory.
func loadFileShare(name string) (FileShare, string, error) {
	if !ValidShareName(name) {
		return FileShare{}, "", &shareErr{status: http.StatusBadRequest, msg: fmt.Sprintf("invalid share name %q", name)}
	}
	home, err := filesBashyHome()
	if err != nil {
		return FileShare{}, "", &shareErr{status: http.StatusInternalServerError, msg: err.Error()}
	}
	sharedDir := filepath.Join(home, "shared")
	f, err := os.OpenInRoot(sharedDir, name+".json")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return FileShare{}, "", &shareErr{status: http.StatusNotFound, msg: fmt.Sprintf("share %q not found", name)}
		}
		return FileShare{}, "", &shareErr{status: http.StatusForbidden, msg: fmt.Sprintf("share %q unavailable: %v", name, err)}
	}
	data, rerr := io.ReadAll(io.LimitReader(f, 1<<20))
	_ = f.Close()
	if rerr != nil {
		return FileShare{}, "", &shareErr{status: http.StatusInternalServerError, msg: rerr.Error()}
	}
	var rec FileShare
	if err := json.Unmarshal(data, &rec); err != nil {
		return FileShare{}, "", &shareErr{status: http.StatusInternalServerError, msg: fmt.Sprintf("invalid share record %q: %v", name, err)}
	}
	if rec.Name != "" && rec.Name != name {
		return FileShare{}, "", &shareErr{status: http.StatusForbidden, msg: fmt.Sprintf("share record name %q does not match %q", rec.Name, name)}
	}
	rec.Name = name
	mode := strings.TrimSpace(rec.Mode)
	if mode != "" && mode != "ro" {
		return FileShare{}, "", &shareErr{status: http.StatusForbidden, msg: fmt.Sprintf("share %q mode %q is unsupported (read-only only)", name, mode)}
	}
	rec.Mode = "ro"

	rootDir := strings.TrimSpace(rec.Path)
	if rootDir == "" {
		rootDir = filepath.Join(sharedDir, name)
		if _, err := os.Stat(rootDir); errors.Is(err, os.ErrNotExist) {
			_ = os.MkdirAll(rootDir, 0o700)
		}
	}
	st, err := os.Stat(rootDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return FileShare{}, "", &shareErr{status: http.StatusNotFound, msg: fmt.Sprintf("share %q folder does not exist", name)}
		}
		return FileShare{}, "", &shareErr{status: http.StatusForbidden, msg: fmt.Sprintf("share %q folder unavailable: %v", name, err)}
	}
	if !st.IsDir() {
		return FileShare{}, "", &shareErr{status: http.StatusForbidden, msg: fmt.Sprintf("share %q path is not a directory", name)}
	}
	return rec, rootDir, nil
}

// FileShareDefinition builds the typed definition and SHA-256 digest for files-NAME.
func FileShareDefinition(name string) (FileToolDefinition, error) {
	def := FileToolDefinition{
		Name:         "files-" + name,
		Class:        "tool",
		Description:  fmt.Sprintf("Read-only access to designated shared folder %s (ls, stat, read inline up to 256 KiB, or mint a bashy:// stream link).", name),
		InputSchema:  json.RawMessage(FilesInputSchemaJSON),
		OutputSchema: json.RawMessage(FilesOutputSchemaJSON),
		Effects:      []string{"read"},
		Unit:         "bytes",
	}
	raw, err := json.Marshal(def)
	if err != nil {
		return FileToolDefinition{}, err
	}
	var canonical any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&canonical); err != nil {
		return FileToolDefinition{}, err
	}
	canonBytes, err := json.Marshal(canonical)
	if err != nil {
		return FileToolDefinition{}, err
	}
	sum := sha256.Sum256(canonBytes)
	def.Digest = hex.EncodeToString(sum[:])
	return def, nil
}

func weakFileETag(info os.FileInfo) string {
	return fmt.Sprintf(`W/"%x-%x"`, info.Size(), info.ModTime().UTC().UnixNano())
}

func fileEntryFromInfo(name, relPath string, info os.FileInfo) FileEntry {
	e := FileEntry{
		Name:    name,
		Path:    relPath,
		Type:    "file",
		IsDir:   info.IsDir(),
		Size:    info.Size(),
		ModTime: info.ModTime().UTC().Format(time.RFC3339),
	}
	if info.IsDir() {
		e.Type = "dir"
		e.Size = 0
	} else {
		e.ETag = weakFileETag(info)
	}
	return e
}

func fileShareURI(host, share, relPath string) string {
	var b strings.Builder
	b.WriteString("bashy://")
	b.WriteString(url.PathEscape(host))
	b.WriteString("/files-")
	b.WriteString(url.PathEscape(share))
	b.WriteByte('/')
	if relPath != "" && relPath != "." {
		parts := strings.Split(relPath, "/")
		for i, p := range parts {
			if i > 0 {
				b.WriteByte('/')
			}
			b.WriteString(url.PathEscape(p))
		}
	}
	return b.String()
}

// prepareRelPath validates raw and returns the name to pass to os.OpenInRoot
// and the clean slash-separated relative path for responses.
func prepareRelPath(raw string) (openName, display string, err error) {
	if strings.ContainsRune(raw, 0) || strings.ContainsRune(raw, '\\') {
		return "", "", &shareErr{status: http.StatusForbidden, msg: "invalid path"}
	}
	if raw == "" || raw == "." {
		return ".", "", nil
	}
	if strings.HasPrefix(raw, "/") {
		return "", "", &shareErr{status: http.StatusForbidden, msg: "absolute path is not allowed"}
	}
	trimmed := strings.TrimSuffix(raw, "/")
	if trimmed == "" || trimmed == "." {
		return ".", "", nil
	}
	for _, seg := range strings.Split(trimmed, "/") {
		if seg == "" {
			return "", "", &shareErr{status: http.StatusForbidden, msg: "empty path segment"}
		}
	}
	cleaned := path.Clean(trimmed)
	if cleaned == "." {
		cleaned = ""
	}
	return filepath.FromSlash(trimmed), cleaned, nil
}

func openInShareRoot(rootDir, rawPath string) (*os.File, os.FileInfo, string, error) {
	openName, display, err := prepareRelPath(rawPath)
	if err != nil {
		return nil, nil, "", err
	}
	f, err := os.OpenInRoot(rootDir, openName)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, "", &shareErr{status: http.StatusNotFound, msg: fmt.Sprintf("path %q not found", rawPath)}
		}
		return nil, nil, "", &shareErr{status: http.StatusForbidden, msg: fmt.Sprintf("path %q refused: %v", rawPath, err)}
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, "", &shareErr{status: http.StatusForbidden, msg: fmt.Sprintf("stat %q refused: %v", rawPath, err)}
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, nil, "", &shareErr{status: http.StatusForbidden, msg: fmt.Sprintf("path %q is not a regular file or directory", rawPath)}
	}
	return f, info, display, nil
}

func listShareDir(shareName, rootDir, relDisplay string, dirFile *os.File) (FileListResult, error) {
	dirents, err := dirFile.ReadDir(-1)
	if err != nil {
		return FileListResult{}, &shareErr{status: http.StatusInternalServerError, msg: err.Error()}
	}
	sort.Slice(dirents, func(i, j int) bool { return dirents[i].Name() < dirents[j].Name() })
	entries := make([]FileEntry, 0, len(dirents))
	for _, d := range dirents {
		childRel := d.Name()
		if relDisplay != "" {
			childRel = relDisplay + "/" + d.Name()
		}
		var info os.FileInfo
		if d.Type()&os.ModeSymlink != 0 {
			cf, cerr := os.OpenInRoot(rootDir, filepath.FromSlash(childRel))
			if cerr != nil {
				// Hide symlinks that escape the root or dangle.
				continue
			}
			info, err = cf.Stat()
			_ = cf.Close()
			if err != nil {
				continue
			}
		} else {
			info, err = d.Info()
			if err != nil {
				continue
			}
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			continue
		}
		entries = append(entries, fileEntryFromInfo(d.Name(), childRel, info))
	}
	return FileListResult{
		Op:      "ls",
		Share:   shareName,
		Path:    relDisplay,
		Entries: entries,
	}, nil
}

type byteCountWriter struct {
	http.ResponseWriter
	status  int
	written int64
}

func (w *byteCountWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *byteCountWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.written += int64(n)
	return n, err
}

func (w *byteCountWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *byteCountWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func unwrapResponseWriter(w http.ResponseWriter) http.ResponseWriter {
	if cw, ok := w.(*captureWriter); ok {
		return cw.Unwrap()
	}
	return w
}

func (b *Broker) finishFilesErr(w http.ResponseWriter, ri *reqInfo, path string, err error) {
	status := http.StatusInternalServerError
	msg := err.Error()
	var se *shareErr
	if errors.As(err, &se) {
		status = se.status
		msg = se.msg
	}
	b.finish(ri, status, 0, msg)
	writeErr(w, path, status, msg)
}

func (b *Broker) serveFiles(w http.ResponseWriter, r *http.Request, ri *reqInfo) {
	rawW := unwrapResponseWriter(w)
	if r.URL.Path == "/files" || r.URL.Path == "/files/" {
		b.finishFilesErr(rawW, ri, r.URL.Path, &shareErr{status: http.StatusNotFound, msg: "missing share name"})
		return
	}
	tail := strings.TrimPrefix(r.URL.Path, "/files/")
	name, sub, hasSlash := strings.Cut(tail, "/")

	_, rootDir, err := loadFileShare(name)
	if err != nil {
		b.finishFilesErr(rawW, ri, r.URL.Path, err)
		return
	}

	if !hasSlash && r.Method == http.MethodGet && r.URL.Query().Has("definition") {
		def, err := FileShareDefinition(name)
		if err != nil {
			b.finishFilesErr(rawW, ri, r.URL.Path, err)
			return
		}
		b.finish(ri, http.StatusOK, 0, "")
		writeJSON(rawW, http.StatusOK, def)
		return
	}

	if r.Method == http.MethodPost {
		if hasSlash {
			b.finishFilesErr(rawW, ri, r.URL.Path, &shareErr{status: http.StatusMethodNotAllowed, msg: "POST is supported only on /files/" + name})
			return
		}
		b.serveFileToolCall(rawW, r, ri, name, rootDir)
		return
	}

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		b.finishFilesErr(rawW, ri, r.URL.Path, &shareErr{status: http.StatusMethodNotAllowed, msg: "method not allowed (shared folders are read-only)"})
		return
	}

	b.serveFileBytes(rawW, r, ri, name, rootDir, sub)
}

func (b *Broker) serveFileToolCall(w http.ResponseWriter, r *http.Request, ri *reqInfo, name, rootDir string) {
	body, err := readBody(r)
	if err != nil {
		b.finishFilesErr(w, ri, r.URL.Path, &shareErr{status: http.StatusBadRequest, msg: err.Error()})
		return
	}
	var call FileToolCall
	if err := json.Unmarshal(body, &call); err != nil {
		b.finishFilesErr(w, ri, r.URL.Path, &shareErr{status: http.StatusBadRequest, msg: "invalid JSON body: " + err.Error()})
		return
	}
	if call.Offset < 0 || call.Length < 0 {
		b.finishFilesErr(w, ri, r.URL.Path, &shareErr{status: http.StatusBadRequest, msg: "offset and length must not be negative"})
		return
	}

	switch call.Op {
	case "ls":
		f, info, display, err := openInShareRoot(rootDir, call.Path)
		if err != nil {
			b.finishFilesErr(w, ri, r.URL.Path, err)
			return
		}
		defer f.Close()
		if !info.IsDir() {
			b.finishFilesErr(w, ri, r.URL.Path, &shareErr{status: http.StatusBadRequest, msg: fmt.Sprintf("path %q is not a directory", call.Path)})
			return
		}
		res, err := listShareDir(name, rootDir, display, f)
		if err != nil {
			b.finishFilesErr(w, ri, r.URL.Path, err)
			return
		}
		b.finish(ri, http.StatusOK, 0, "")
		writeJSON(w, http.StatusOK, res)

	case "stat":
		f, info, display, err := openInShareRoot(rootDir, call.Path)
		if err != nil {
			b.finishFilesErr(w, ri, r.URL.Path, err)
			return
		}
		_ = f.Close()
		entryName := info.Name()
		if display == "" {
			entryName = name
		}
		b.finish(ri, http.StatusOK, 0, "")
		writeJSON(w, http.StatusOK, FileStatResult{
			Op:        "stat",
			Share:     name,
			FileEntry: fileEntryFromInfo(entryName, display, info),
		})

	case "read":
		f, info, display, err := openInShareRoot(rootDir, call.Path)
		if err != nil {
			b.finishFilesErr(w, ri, r.URL.Path, err)
			return
		}
		defer f.Close()
		if info.IsDir() {
			b.finishFilesErr(w, ri, r.URL.Path, &shareErr{status: http.StatusBadRequest, msg: fmt.Sprintf("path %q is a directory; use op \"ls\"", call.Path)})
			return
		}
		uri := fileShareURI(doorHostName(), name, display)
		if call.Length > MaxInlineReadBytes {
			msg := fmt.Sprintf("requested length %d exceeds 256 KiB inline read cap (%d bytes); use op \"link\" (%s)", call.Length, MaxInlineReadBytes, uri)
			b.finish(ri, http.StatusRequestEntityTooLarge, 0, msg)
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
				"error": map[string]any{"message": msg, "type": "bashy_broker", "code": http.StatusRequestEntityTooLarge},
				"uri":   uri,
				"link":  uri,
				"size":  info.Size(),
			})
			return
		}
		remaining := info.Size() - call.Offset
		if remaining < 0 {
			remaining = 0
		}
		want := remaining
		if call.Length > 0 && call.Length < want {
			want = call.Length
		}
		if call.Length == 0 && want > MaxInlineReadBytes {
			msg := fmt.Sprintf("file %q (%d bytes from offset %d) exceeds 256 KiB inline read cap; use op \"link\" (%s)", display, want, call.Offset, uri)
			b.finish(ri, http.StatusRequestEntityTooLarge, 0, msg)
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
				"error": map[string]any{"message": msg, "type": "bashy_broker", "code": http.StatusRequestEntityTooLarge},
				"uri":   uri,
				"link":  uri,
				"size":  info.Size(),
			})
			return
		}
		chunk := []byte{}
		if want > 0 {
			sr := io.NewSectionReader(f, call.Offset, want)
			chunk, err = io.ReadAll(sr)
			if err != nil {
				b.finishFilesErr(w, ri, r.URL.Path, &shareErr{status: http.StatusInternalServerError, msg: err.Error()})
				return
			}
		}
		ri.rec.Bytes = int64(len(chunk))
		eof := call.Offset+int64(len(chunk)) >= info.Size()
		b.finish(ri, http.StatusOK, 0, "")
		writeJSON(w, http.StatusOK, FileReadResult{
			Op:     "read",
			Share:  name,
			Path:   display,
			Size:   info.Size(),
			Offset: call.Offset,
			Length: len(chunk),
			Data:   chunk,
			EOF:    eof,
			ETag:   weakFileETag(info),
		})

	case "link":
		f, info, display, err := openInShareRoot(rootDir, call.Path)
		if err != nil {
			b.finishFilesErr(w, ri, r.URL.Path, err)
			return
		}
		_ = f.Close()
		out := FileLinkResult{
			Op:    "link",
			Share: name,
			Path:  display,
			URI:   fileShareURI(doorHostName(), name, display),
			Size:  info.Size(),
			IsDir: info.IsDir(),
		}
		if out.IsDir {
			out.Size = 0
		} else {
			out.ETag = weakFileETag(info)
		}
		b.finish(ri, http.StatusOK, 0, "")
		writeJSON(w, http.StatusOK, out)

	default:
		b.finishFilesErr(w, ri, r.URL.Path, &shareErr{status: http.StatusBadRequest, msg: fmt.Sprintf("unknown op %q (want ls, stat, read or link)", call.Op)})
	}
}

func (b *Broker) serveFileBytes(w http.ResponseWriter, r *http.Request, ri *reqInfo, name, rootDir, sub string) {
	f, info, display, err := openInShareRoot(rootDir, sub)
	if err != nil {
		b.finishFilesErr(w, ri, r.URL.Path, err)
		return
	}
	defer f.Close()

	if info.IsDir() {
		res, err := listShareDir(name, rootDir, display, f)
		if err != nil {
			b.finishFilesErr(w, ri, r.URL.Path, err)
			return
		}
		data, err := json.Marshal(res)
		if err != nil {
			b.finishFilesErr(w, ri, r.URL.Path, err)
			return
		}
		data = append(data, '\n')
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(data)
		}
		b.finish(ri, http.StatusOK, 0, "")
		return
	}

	w.Header().Set("ETag", weakFileETag(info))
	cw := &byteCountWriter{ResponseWriter: w}
	http.ServeContent(cw, r, info.Name(), info.ModTime(), f)
	status := cw.status
	if status == 0 {
		status = http.StatusOK
	}
	if r.Method == http.MethodGet && (status == http.StatusOK || status == http.StatusPartialContent) {
		ri.rec.Bytes = cw.written
	}
	b.finish(ri, status, 0, "")
}
