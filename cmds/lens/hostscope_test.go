package lenscmd

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

type fakeHost struct {
	calls []string
	tree  map[string][]hostEntry
	files map[string]string
}

func (f *fakeHost) List(_ context.Context, name, dir string) ([]hostEntry, error) {
	f.calls = append(f.calls, "ls "+name+" "+dir)
	es, ok := f.tree[dir]
	if !ok {
		return nil, fmt.Errorf("no such dir %s", dir)
	}
	return es, nil
}

func (f *fakeHost) Read(_ context.Context, name, file string) ([]byte, error) {
	f.calls = append(f.calls, "get "+name+" "+file)
	return []byte(f.files[file]), nil
}

func hostFixture(t *testing.T, peersJSON string) *fakeHost {
	t.Helper()
	fh := &fakeHost{
		tree: map[string][]hostEntry{
			".":   {{Name: "src", Dir: true}},
			"src": {{Name: "a.txt", Size: 5}},
		},
		files: map[string]string{"src/a.txt": "hello"},
	}
	oldT, oldR, oldTargets := hostTransportImpl, peerRunner, hostTargetsRunner
	hostTransportImpl = fh
	peerRunner = func(context.Context) ([]byte, error) { return []byte(peersJSON), nil }
	hostTargetsRunner = func(context.Context) ([]byte, error) {
		return []byte(`[{"name":"mac-target","host":"mac-1.example"}]`), nil
	}
	t.Cleanup(func() { hostTransportImpl, peerRunner, hostTargetsRunner = oldT, oldR, oldTargets })
	return fh
}

const hostPeers = `[
 {"host":"mac-1.example","alias":"mac","online":true},
 {"host":"box-2.example","alias":"dup","online":true},
 {"host":"box-3.example","alias":"dup","online":true},
 {"host":"gone.example","online":false}]`

func TestHostScopeReadsPeer(t *testing.T) {
	fh := hostFixture(t, hostPeers)
	cwd := t.TempDir()
	for _, scope := range []string{"mac", "mac-1.example"} {
		out, errOut, code := runTool(t, cwd, "", "-t", "graph", "dir:src@"+scope)
		if code != 0 || !strings.Contains(out, "a.txt") || !strings.Contains(out, "@"+scope) {
			t.Fatalf("%s: code %d stderr %q out %q", scope, code, errOut, out)
		}
	}
	for _, c := range fh.calls {
		if !strings.HasPrefix(c, "ls mac-target ") && !strings.HasPrefix(c, "get mac-target ") {
			t.Fatalf("non-read transport call %q", c)
		}
	}
	if len(fh.calls) == 0 {
		t.Fatal("transport never used")
	}
}

func TestHostScopeRefusals(t *testing.T) {
	fh := hostFixture(t, hostPeers)
	cwd := t.TempDir()
	for _, tc := range []struct{ op, want string }{
		{"dir:src@nobody", "unknown peer"},
		{"dir:src@dup", "ambiguous peer"},
		{"dir:src@gone.example", "offline"},
		{"dir:..%2Fetc@mac", "escapes host scope"},
		{"dir:src%2F..%2F..@mac", "escapes host scope"},
		{"dir:%2Fetc@mac", "host-relative"},
		{"dir:~%2Fx@mac", "host-relative"},
		{"git:HEAD:README.md@mac", "does not support @host scope"},
	} {
		out, errOut, code := runTool(t, cwd, "", "-t", "graph", tc.op)
		if code == 0 || out != "" || !strings.Contains(errOut, tc.want) {
			t.Errorf("%s: code %d out %q stderr %q, want %q", tc.op, code, out, errOut, tc.want)
		}
	}
	if len(fh.calls) != 0 {
		t.Fatalf("refused locators reached the transport: %v", fh.calls)
	}
}

func TestHostScopeNotImplied(t *testing.T) {
	fh := hostFixture(t, hostPeers)
	_, errOut, code := runTool(t, t.TempDir(), "", "-t", "graph", "dir:src")
	_ = errOut
	_ = code
	if len(fh.calls) != 0 {
		t.Fatalf("unscoped locator used the host transport: %v", fh.calls)
	}
}

func TestParseHostListingPreservesNamesAndSkipsSymlinks(t *testing.T) {
	es, err := parseHostListing([]byte("-rw-r--r--\t5\ta file\twith tabs.txt\nLrwxrwxrwx\t8\toutside\ndrwxr-xr-x\t0\ta folder\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 2 || es[0].Name != "a file\twith tabs.txt" || es[0].Dir || es[1].Name != "a folder" || !es[1].Dir {
		t.Fatalf("unexpected entries: %+v", es)
	}
	if _, err := parseHostListing([]byte("-rw-r--r--\t5\t../escape\n")); err == nil {
		t.Fatal("unsafe remote name accepted")
	}
}

func TestHostScopeRequiresUniqueSavedTarget(t *testing.T) {
	fh := hostFixture(t, hostPeers)
	hostTargetsRunner = func(context.Context) ([]byte, error) {
		return []byte(`[{"name":"one","host":"mac-1.example"},{"name":"two","host":"mac-1.example"}]`), nil
	}
	out, errOut, code := runTool(t, t.TempDir(), "", "-t", "graph", "dir:src@mac")
	if code == 0 || out != "" || !strings.Contains(errOut, "exactly one saved SSH target") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
	}
	if len(fh.calls) != 0 {
		t.Fatalf("ambiguous target reached transport: %v", fh.calls)
	}
}
