package sharedfiles

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/qiangli/yoke/pkg/toolcmd"
)

func execute(args ...string) (string, error) {
	cmd := NewCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SilenceUsage = true
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestSharedOwner(t *testing.T) {
	home := t.TempDir()
	state := filepath.Join(home, ".bashy")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASHY_HOME", state)
	run := func(args ...string) string {
		t.Helper()
		out, err := execute(args...)
		if err != nil {
			t.Fatal(args, err)
		}
		return out
	}
	if got := run("shares", "--json"); got != "[]\n" {
		t.Fatal(got)
	}
	run("share", "docs", "--circle", "a", "--circle", "b", "--group", "g", "--email", "e", "--note", "hello", "--json")
	var records []Record
	if err := json.Unmarshal([]byte(run("shares", "--json")), &records); err != nil || len(records) != 1 {
		t.Fatal(records, err)
	}
	r := records[0]
	if r.Name != "docs" || r.Mode != "ro" || r.Note != "hello" || r.Created.IsZero() || !slices.Equal(r.Audience.Circles, []string{"a", "b"}) || !slices.Equal(r.Audience.Groups, []string{"g"}) || !slices.Equal(r.Audience.Emails, []string{"e"}) {
		t.Fatal(r)
	}
	folder := filepath.Join(state, "shared", "docs")
	if items, err := os.ReadDir(folder); err != nil || len(items) != 0 {
		t.Fatal(items, err)
	}
	if err := os.WriteFile(filepath.Join(folder, "keep"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	run("unshare", "docs", "--json")
	if _, err := os.Stat(filepath.Join(folder, "keep")); err != nil {
		t.Fatal(err)
	}
	if got := run("shares", "--json"); got != "[]\n" {
		t.Fatal(got)
	}
	link := filepath.Join(home, "alias")
	if err := os.Symlink(state, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{home, filepath.Dir(home), state, folder, link} {
		if _, err := execute("share", "private", "--path", path); err == nil {
			t.Errorf("allowed %q", path)
		}
	}
	for _, args := range [][]string{{"share", "private", "--path", home, "--force"}, {"share", "../escape"}, {"unshare", "../escape"}} {
		if _, err := execute(args...); err == nil {
			t.Error(args)
		}
	}
	run("share", "private", "--path", home, "--force", "--reason", "approved", "--json")
	data, err := os.ReadFile(filepath.Join(state, "shared", "private.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &r); err != nil || !r.Forced || r.Reason != "approved" {
		t.Fatal(r, err)
	}
	if err := os.Symlink(home, filepath.Join(state, "shared", "redirect")); err != nil {
		t.Fatal(err)
	}
	if _, err := execute("share", "redirect"); err == nil {
		t.Error("followed managed folder symlink")
	}
	outside := t.TempDir()
	run("share", "outside", "--path", outside)
}

func TestSharedPeer(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	const path = "a b/%?#.bin"
	const content = "\x00\xff"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("missing peer credential")
		}
		if r.Method == http.MethodGet {
			if r.URL.Path != "/files/docs/"+path || r.URL.RawQuery != "" {
				t.Errorf("unexpected target %s", r.URL)
			}
			io.WriteString(w, content)
			return
		}
		if r.Method != "POST" || r.URL.Path != "/files/docs" || r.Header.Get("Content-Type") != "application/json" {
			t.Error(r.Method, r.URL)
		}
		var c call
		if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
			t.Error(err)
		}
		switch c.Op {
		case "ls", "stat":
			if c.Path != "" {
				t.Error(c)
			}
			fmt.Fprint(w, `{"ok":true}`)
		case "read":
			if c.Offset != 1 || c.Length != 2 || c.Path != path {
				t.Error(c)
			}
			fmt.Fprint(w, `{"data":"AP8="}`)
		case "link":
			fmt.Fprint(w, `{"uri":"bashy://peer/files-docs"}`)
		default:
			t.Error(c)
		}
	}))
	defer srv.Close()
	if err := toolcmd.SavePeer("peer", toolcmd.PeerDoor{URL: srv.URL, Credential: "test-token"}); err != nil {
		t.Fatal(err)
	}
	for _, verb := range []string{"ls", "stat"} {
		out, err := execute(verb, "peer:docs", "--json")
		if err != nil || out != "{\"ok\":true}\n" {
			t.Fatal(verb, out, err)
		}
	}
	out, err := execute("link", "bashy://peer/files-docs")
	if err != nil || out != "bashy://peer/files-docs\n" {
		t.Fatal(out, err)
	}
	const uri = "bashy://peer/files-docs/a%20b/%25%3F%23.bin"
	out, err = execute("cat", uri, "--offset", "1", "--length", "2")
	if err != nil || out != content {
		t.Fatal(out, err)
	}
	dest := filepath.Join(t.TempDir(), "saved")
	out, err = execute("get", "peer:docs/"+path, dest, "--json")
	if err != nil || !json.Valid([]byte(out)) {
		t.Fatal(out, err)
	}
	data, err := os.ReadFile(dest)
	if err != nil || string(data) != content {
		t.Fatal(data, err)
	}
}

func TestSharedInvalidTargets(t *testing.T) {
	for _, s := range []string{"", "peer:../dir", "peer:docs//outside", "peer:docs/x/../y", "bashy://user@peer/files-x", "bashy://peer/files-x/%2e%2e/y", "bashy://peer/files-x/a%2fb", "bashy://peer/other-x", "bashy://peer/files-docs?query"} {
		if _, err := ParseTarget(s); err == nil {
			t.Errorf("accepted %q", s)
		}
	}
}

func TestSharedFailedDownloadKeepsDestination(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	var redirected bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/files/docs/partial":
			w.Header().Set("Content-Length", "100")
			io.WriteString(w, "partial")
		case "/files/docs/redirect":
			http.Redirect(w, r, "/next", http.StatusFound)
		case "/next":
			redirected = true
		default:
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer srv.Close()
	if err := toolcmd.SavePeer("peer", toolcmd.PeerDoor{URL: srv.URL, Credential: "test-token"}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	dest := filepath.Join(dir, "original")
	if err := os.WriteFile(dest, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"partial", "redirect", "forbidden"} {
		if _, err := execute("get", "peer:docs/"+file, dest); err == nil {
			t.Errorf("get %s succeeded", file)
		}
	}
	data, err := os.ReadFile(dest)
	if err != nil || string(data) != "original" {
		t.Fatal(data, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || redirected {
		t.Fatal(entries, err, redirected)
	}
}

func TestSharedRelocatedState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	outer := t.TempDir()
	state := filepath.Join(outer, "state")
	t.Setenv("BASHY_HOME", state)
	legacy := filepath.Join(home, ".bashy", "private")
	if err := os.MkdirAll(legacy, 0700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{outer, legacy} {
		if _, err := execute("share", "test", "--path", path); err == nil {
			t.Errorf("allowed %q", path)
		}
	}
}
