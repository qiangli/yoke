package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeShareRecord(t *testing.T, home, name string, rec map[string]any) {
	t.Helper()
	dir := filepath.Join(home, "shared")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func doRaw(t *testing.T, method, url string, body []byte, hdr map[string]string) (*http.Response, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, out
}

func TestFilesAuthOwnerOnlyInF1(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	peersFile := filepath.Join(home, "peers.json")
	peerCred, err := MintPeerCredential(peersFile, "boxb", ShareAllow, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	writeShareRecord(t, home, "renders", map[string]any{"name": "renders", "mode": "ro"})

	h := newHarness(t, func(o *Options) { o.PeersFile = peersFile })

	// Unauthenticated and bad bearer are 401.
	resp, _ := h.do("GET", "/files/renders?definition", nil, map[string]string{"Authorization": ""})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing auth: got %d, want 401", resp.StatusCode)
	}
	resp, _ = h.do("GET", "/files/renders?definition", nil, map[string]string{"Authorization": "Bearer wrong"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad auth: got %d, want 401", resp.StatusCode)
	}

	// Shared peer credential is refused on /files/* in F1 (peer/member admission comes in K4).
	resp, _ = h.do("GET", "/files/renders?definition", nil, map[string]string{"Authorization": "Bearer " + peerCred})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("peer auth: got %d, want 403", resp.StatusCode)
	}
	resp, _ = h.do("POST", "/files/renders", map[string]any{"op": "ls"}, map[string]string{"Authorization": "Bearer " + peerCred})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("peer tool call: got %d, want 403", resp.StatusCode)
	}

	// Owner bearer, /k/<token>, and unix socket all pass.
	resp, _ = h.do("GET", "/files/renders?definition", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("owner bearer: got %d, want 200", resp.StatusCode)
	}
	resp, _ = h.do("GET", "/k/"+testToken+"/files/renders?definition", nil, map[string]string{"Authorization": ""})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("owner path token: got %d, want 200", resp.StatusCode)
	}

	sock := filepath.Join(t.TempDir(), "d.sock")
	ln, err := net.Listen("unix", sock)
	if err == nil {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go h.b.Serve(ctx, ln)
		client := http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}}}
		uResp, err := client.Get("http://door/files/renders?definition")
		if err != nil {
			t.Fatal(err)
		}
		uResp.Body.Close()
		if uResp.StatusCode != http.StatusOK {
			t.Fatalf("unix owner socket: got %d, want 200", uResp.StatusCode)
		}
	}
}

func TestFilesDefinitionAndDigest(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	writeShareRecord(t, home, "renders", map[string]any{"name": "renders"})
	h := newHarness(t, nil)

	resp, raw := doRaw(t, "GET", h.server.URL+"/files/renders?definition", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("definition status %d: %s", resp.StatusCode, raw)
	}
	var def FileToolDefinition
	if err := json.Unmarshal(raw, &def); err != nil {
		t.Fatalf("decode definition: %v", err)
	}
	if def.Name != "files-renders" || def.Class != "tool" || def.Unit != "bytes" {
		t.Fatalf("unexpected definition metadata: %+v", def)
	}
	if len(def.Effects) != 1 || def.Effects[0] != "read" {
		t.Fatalf("unexpected effects: %+v", def.Effects)
	}
	if len(def.Digest) != 64 {
		t.Fatalf("expected 64-hex sha256 digest, got %q", def.Digest)
	}
	var schema struct {
		Type       string                       `json:"type"`
		Required   []string                     `json:"required"`
		Properties map[string]map[string]any    `json:"properties"`
	}
	if err := json.Unmarshal(def.InputSchema, &schema); err != nil {
		t.Fatalf("invalid input_schema JSON: %v", err)
	}
	if schema.Type != "object" || len(schema.Required) != 1 || schema.Required[0] != "op" {
		t.Fatalf("unexpected input_schema: %+v", schema)
	}
	for _, field := range []string{"op", "path", "offset", "length"} {
		if _, ok := schema.Properties[field]; !ok {
			t.Fatalf("input_schema missing property %q", field)
		}
	}

	// Deterministic digest across repeated calls; missing share is 404.
	_, raw2 := doRaw(t, "GET", h.server.URL+"/files/renders?definition", nil, nil)
	var def2 FileToolDefinition
	_ = json.Unmarshal(raw2, &def2)
	if def.Digest != def2.Digest {
		t.Fatalf("digest drifted: %q vs %q", def.Digest, def2.Digest)
	}
	miss, _ := h.do("GET", "/files/unknown?definition", nil, nil)
	if miss.StatusCode != http.StatusNotFound {
		t.Fatalf("missing share definition: got %d, want 404", miss.StatusCode)
	}
}

func TestFilesDefaultFolderLiveReloadAndReadOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	h := newHarness(t, nil)

	// Before share record exists -> 404.
	resp, _ := h.do("POST", "/files/inbox", map[string]any{"op": "ls"}, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("before share created: got %d, want 404", resp.StatusCode)
	}

	// Write record with missing path -> defaults to <bashy-home>/shared/inbox/, empty.
	writeShareRecord(t, home, "inbox", map[string]any{"name": "inbox"})
	resp, out := h.do("POST", "/files/inbox", map[string]any{"op": "ls"}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("default folder ls: got %d, body %v", resp.StatusCode, out)
	}
	entries, ok := out["entries"].([]any)
	if !ok || len(entries) != 0 {
		t.Fatalf("expected empty entries array, got %v", out["entries"])
	}

	// Repoint record at an external designated directory -> live reload with no restart.
	extDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(extDir, "note.txt"), []byte("designated"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeShareRecord(t, home, "inbox", map[string]any{"name": "inbox", "path": extDir, "mode": "ro"})
	resp, raw := doRaw(t, "POST", h.server.URL+"/files/inbox", []byte(`{"op":"read","path":"note.txt"}`), nil)
	var reloadRead FileReadResult
	_ = json.Unmarshal(raw, &reloadRead)
	if resp.StatusCode != http.StatusOK || string(reloadRead.Data) != "designated" {
		t.Fatalf("reloaded share read: got %d %s", resp.StatusCode, raw)
	}

	// Non-ro mode is refused.
	writeShareRecord(t, home, "rwshare", map[string]any{"name": "rwshare", "path": extDir, "mode": "rw"})
	resp, _ = h.do("POST", "/files/rwshare", map[string]any{"op": "ls"}, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("rw mode: got %d, want 403", resp.StatusCode)
	}

	// Removing the record unshares immediately -> 404.
	if err := os.Remove(filepath.Join(home, "shared", "inbox.json")); err != nil {
		t.Fatal(err)
	}
	resp, _ = h.do("POST", "/files/inbox", map[string]any{"op": "ls"}, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("after unshare: got %d, want 404", resp.StatusCode)
	}

	// Mutating HTTP verbs are refused (read-only).
	writeShareRecord(t, home, "ro", map[string]any{"name": "ro", "path": extDir})
	for _, m := range []string{http.MethodPut, http.MethodDelete, http.MethodPatch} {
		r, _ := h.do(m, "/files/ro/note.txt", nil, nil)
		if r.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("%s on byte route: got %d, want 405", m, r.StatusCode)
		}
	}
	rPostSub, _ := h.do("POST", "/files/ro/note.txt", map[string]any{"op": "ls"}, nil)
	if rPostSub.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST on subpath: got %d, want 405", rPostSub.StatusCode)
	}
}

func TestFilesToolOpsAndInlineCap(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)

	prevHost := doorHostName
	doorHostName = func() string { return "door-c" }
	t.Cleanup(func() { doorHostName = prevHost })

	shareDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(shareDir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shareDir, "hello.txt"), []byte("hello world\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	binBytes := []byte{0xff, 0xfe, 0x00, 0x80, 0x42}
	if err := os.WriteFile(filepath.Join(shareDir, "sub", "raw.bin"), binBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	big := bytes.Repeat([]byte("abcd"), (MaxInlineReadBytes/4)+256) // > 256 KiB
	if err := os.WriteFile(filepath.Join(shareDir, "sub", "movie.mp4"), big, 0o600); err != nil {
		t.Fatal(err)
	}
	writeShareRecord(t, home, "media", map[string]any{"name": "media", "path": shareDir, "mode": "ro"})

	h := newHarness(t, nil)

	// ls root
	resp, raw := doRaw(t, "POST", h.server.URL+"/files/media", []byte(`{"op":"ls"}`), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ls status %d: %s", resp.StatusCode, raw)
	}
	var ls FileListResult
	if err := json.Unmarshal(raw, &ls); err != nil {
		t.Fatal(err)
	}
	if len(ls.Entries) != 2 || ls.Entries[0].Name != "hello.txt" || ls.Entries[0].IsDir || ls.Entries[1].Name != "sub" || !ls.Entries[1].IsDir {
		t.Fatalf("unexpected ls entries: %+v", ls.Entries)
	}

	// stat file
	resp, raw = doRaw(t, "POST", h.server.URL+"/files/media", []byte(`{"op":"stat","path":"hello.txt"}`), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stat status %d: %s", resp.StatusCode, raw)
	}
	var st FileStatResult
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	if st.Size != 12 || st.IsDir || !strings.HasPrefix(st.ETag, `W/"`) {
		t.Fatalf("unexpected stat: %+v", st)
	}

	// read UTF-8 text inline (base64 in JSON key "data") + check Record.Bytes
	before := len(h.b.audit.snapshot())
	resp, raw = doRaw(t, "POST", h.server.URL+"/files/media", []byte(`{"op":"read","path":"hello.txt"}`), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("read text status %d: %s", resp.StatusCode, raw)
	}
	var rd FileReadResult
	if err := json.Unmarshal(raw, &rd); err != nil {
		t.Fatal(err)
	}
	if string(rd.Data) != "hello world\n" || rd.Length != 12 || !rd.EOF {
		t.Fatalf("unexpected text read: %+v", rd)
	}
	rec := h.lastRecord(before + 1)
	if rec.Bytes != 12 || rec.Principal != "owner" || rec.Status != http.StatusOK {
		t.Fatalf("unexpected read run record: %+v", rec)
	}

	// read binary inline -> lossless []byte via "data"
	resp, raw = doRaw(t, "POST", h.server.URL+"/files/media", []byte(`{"op":"read","path":"sub/raw.bin"}`), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("read bin status %d: %s", resp.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, &rd); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rd.Data, binBytes) {
		t.Fatalf("binary read mismatch: got=%x want=%x", rd.Data, binBytes)
	}

	// read > 256 KiB without length -> 413 asking for link
	before = len(h.b.audit.snapshot())
	resp, out := h.do("POST", "/files/media", map[string]any{"op": "read", "path": "sub/movie.mp4"}, nil)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("large inline read: got %d, want 413 (%v)", resp.StatusCode, out)
	}
	if out["uri"] != "bashy://door-c/files-media/sub/movie.mp4" {
		t.Fatalf("expected uri hint in 413 response, got %v", out)
	}
	rec = h.lastRecord(before + 1)
	if rec.Bytes != 0 || rec.Status != http.StatusRequestEntityTooLarge || !strings.Contains(rec.Error, "link") {
		t.Fatalf("unexpected 413 record: %+v", rec)
	}

	// bounded slice read (offset + length <= 256 KiB) on a large file works!
	before = len(h.b.audit.snapshot())
	resp, raw = doRaw(t, "POST", h.server.URL+"/files/media", []byte(`{"op":"read","path":"sub/movie.mp4","offset":2,"length":6}`), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("windowed read: %d %s", resp.StatusCode, raw)
	}
	_ = json.Unmarshal(raw, &rd)
	if string(rd.Data) != "cdabcd" || rd.Offset != 2 || rd.Length != 6 || rd.EOF {
		t.Fatalf("unexpected windowed read: %+v", rd)
	}
	if got := h.lastRecord(before + 1); got.Bytes != 6 {
		t.Fatalf("windowed read record bytes=%d, want 6", got.Bytes)
	}

	// link op
	resp, raw = doRaw(t, "POST", h.server.URL+"/files/media", []byte(`{"op":"link","path":"sub/movie.mp4"}`), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("link status %d: %s", resp.StatusCode, raw)
	}
	var lnk FileLinkResult
	if err := json.Unmarshal(raw, &lnk); err != nil {
		t.Fatal(err)
	}
	if lnk.URI != "bashy://door-c/files-media/sub/movie.mp4" || lnk.Size != int64(len(big)) {
		t.Fatalf("unexpected link result: %+v", lnk)
	}
}

func TestFilesByteRouteServeContentETagRangeAndDirListing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)

	shareDir := t.TempDir()
	// Include a fake usage JSON line in the file content to verify the byte
	// route never triggers inference token parsing or pricing on the run record.
	payload := []byte("0123456789\n{\"usage\":{\"prompt_tokens\":999,\"completion_tokens\":888}}\n")
	if err := os.WriteFile(filepath.Join(shareDir, "data.jsonl"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	writeShareRecord(t, home, "out", map[string]any{"name": "out", "path": shareDir})

	h := newHarness(t, nil)

	// Full GET
	before := len(h.b.audit.snapshot())
	resp, body := doRaw(t, "GET", h.server.URL+"/files/out/data.jsonl", nil, nil)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, payload) {
		t.Fatalf("GET status %d len %d", resp.StatusCode, len(body))
	}
	etag := resp.Header.Get("ETag")
	if !strings.HasPrefix(etag, `W/"`) || resp.Header.Get("Last-Modified") == "" {
		t.Fatalf("missing weak ETag or Last-Modified: etag=%q lm=%q", etag, resp.Header.Get("Last-Modified"))
	}
	rec := h.lastRecord(before + 1)
	if rec.Bytes != int64(len(payload)) || rec.PromptTok != 0 || rec.OutputTok != 0 || rec.Unpriced || rec.CostUSD != 0 {
		t.Fatalf("unexpected full GET record: %+v", rec)
	}

	// Range GET
	before = len(h.b.audit.snapshot())
	resp, part := doRaw(t, "GET", h.server.URL+"/files/out/data.jsonl", nil, map[string]string{"Range": "bytes=2-6"})
	if resp.StatusCode != http.StatusPartialContent || string(part) != "23456" {
		t.Fatalf("Range GET: status %d body %q", resp.StatusCode, part)
	}
	rec = h.lastRecord(before + 1)
	if rec.Bytes != 5 || rec.Status != http.StatusPartialContent {
		t.Fatalf("Range GET record: %+v", rec)
	}

	// HEAD
	before = len(h.b.audit.snapshot())
	resp, headBody := doRaw(t, "HEAD", h.server.URL+"/files/out/data.jsonl", nil, nil)
	if resp.StatusCode != http.StatusOK || len(headBody) != 0 || resp.Header.Get("ETag") != etag {
		t.Fatalf("HEAD: status %d bodyLen %d etag %q", resp.StatusCode, len(headBody), resp.Header.Get("ETag"))
	}
	rec = h.lastRecord(before + 1)
	if rec.Bytes != 0 || rec.Status != http.StatusOK {
		t.Fatalf("HEAD record: %+v", rec)
	}

	// Conditional GET with weak ETag -> 304 Not Modified, 0 bytes
	before = len(h.b.audit.snapshot())
	resp, condBody := doRaw(t, "GET", h.server.URL+"/files/out/data.jsonl", nil, map[string]string{"If-None-Match": etag})
	if resp.StatusCode != http.StatusNotModified || len(condBody) != 0 {
		t.Fatalf("If-None-Match: status %d bodyLen %d", resp.StatusCode, len(condBody))
	}
	rec = h.lastRecord(before + 1)
	if rec.Bytes != 0 || rec.Status != http.StatusNotModified {
		t.Fatalf("304 record: %+v", rec)
	}

	// Directory GET returns JSON listing.
	resp, dirRaw := doRaw(t, "GET", h.server.URL+"/files/out/", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("dir GET: %d %s", resp.StatusCode, dirRaw)
	}
	var listing FileListResult
	if err := json.Unmarshal(dirRaw, &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Entries) != 1 || listing.Entries[0].Name != "data.jsonl" {
		t.Fatalf("unexpected dir listing: %+v", listing)
	}
}

func TestFilesOpenInRootBlocksTraversalAndSymlinkEscape(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)

	parent := t.TempDir()
	shareDir := filepath.Join(parent, "shared-root")
	if err := os.MkdirAll(filepath.Join(shareDir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	secretFile := filepath.Join(parent, "secret.txt")
	if err := os.WriteFile(secretFile, []byte("TOP-SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	insideFile := filepath.Join(shareDir, "sub", "ok.txt")
	if err := os.WriteFile(insideFile, []byte("inside-ok"), 0o600); err != nil {
		t.Fatal(err)
	}

	hasSymlinks := true
	if err := os.Symlink("../secret.txt", filepath.Join(shareDir, "escape-rel.txt")); err != nil {
		hasSymlinks = false
	} else {
		_ = os.Symlink(secretFile, filepath.Join(shareDir, "escape-abs.txt"))
		_ = os.Symlink(filepath.Join("sub", "ok.txt"), filepath.Join(shareDir, "inside-link.txt"))
	}

	writeShareRecord(t, home, "safe", map[string]any{"name": "safe", "path": shareDir})
	h := newHarness(t, nil)

	// Dot-dot escapes in tool call and URL path are refused with 403.
	for _, p := range []string{"../secret.txt", "sub/../../secret.txt", "/etc/passwd"} {
		for _, op := range []string{"read", "stat", "link", "ls"} {
			resp, raw := doRaw(t, "POST", h.server.URL+"/files/safe", mustJSON(t, map[string]any{"op": op, "path": p}), nil)
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("op=%s path=%q: got %d (%s), want 403", op, p, resp.StatusCode, raw)
			}
			if bytes.Contains(raw, []byte("TOP-SECRET")) {
				t.Fatalf("leaked outside secret on op=%s path=%q", op, p)
			}
		}
	}
	for _, urlPath := range []string{
		"/files/safe/%2e%2e/secret.txt",
		"/files/safe/sub/%2e%2e/%2e%2e/secret.txt",
		"/files/..%2fcligw/token",
	} {
		resp, raw := doRaw(t, "GET", h.server.URL+urlPath, nil, nil)
		if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("GET %s: got %d (%s), want 400/403", urlPath, resp.StatusCode, raw)
		}
		if bytes.Contains(raw, []byte("TOP-SECRET")) {
			t.Fatalf("leaked outside secret on GET %s", urlPath)
		}
	}

	if hasSymlinks {
		// Escaping relative and absolute symlinks are refused across every op and GET.
		for _, bad := range []string{"escape-rel.txt", "escape-abs.txt"} {
			for _, op := range []string{"read", "stat", "link"} {
				resp, raw := doRaw(t, "POST", h.server.URL+"/files/safe", mustJSON(t, map[string]any{"op": op, "path": bad}), nil)
				if resp.StatusCode != http.StatusForbidden {
					t.Fatalf("symlink %s op=%s: got %d (%s), want 403", bad, op, resp.StatusCode, raw)
				}
			}
			resp, raw := doRaw(t, "GET", h.server.URL+"/files/safe/"+bad, nil, nil)
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("symlink GET %s: got %d (%s), want 403", bad, resp.StatusCode, raw)
			}
			if bytes.Contains(raw, []byte("TOP-SECRET")) {
				t.Fatalf("symlink GET %s leaked secret", bad)
			}
		}

		// A symlink staying inside the shared root works, and ls hides escaping symlinks.
		resp, raw := doRaw(t, "POST", h.server.URL+"/files/safe", []byte(`{"op":"read","path":"inside-link.txt"}`), nil)
		var linkRead FileReadResult
		_ = json.Unmarshal(raw, &linkRead)
		if resp.StatusCode != http.StatusOK || string(linkRead.Data) != "inside-ok" {
			t.Fatalf("inside symlink read: %d %s", resp.StatusCode, raw)
		}
		resp, raw = doRaw(t, "POST", h.server.URL+"/files/safe", []byte(`{"op":"ls"}`), nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatal(resp.StatusCode)
		}
		var ls FileListResult
		_ = json.Unmarshal(raw, &ls)
		for _, e := range ls.Entries {
			if strings.HasPrefix(e.Name, "escape-") {
				t.Fatalf("ls exposed escaping symlink %+v", e)
			}
		}
		if len(ls.Entries) != 2 || ls.Entries[0].Name != "inside-link.txt" || ls.Entries[1].Name != "sub" {
			t.Fatalf("unexpected ls entries with symlinks: %+v", ls.Entries)
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
