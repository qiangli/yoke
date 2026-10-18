package lenscmd

import (
	"context"
	"errors"
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

// authFailHost fails every transport call with a canned outpost-style error,
// standing in for a live elevation refusal without contacting any peer.
type authFailHost struct{ err error }

func (a authFailHost) List(_ context.Context, name, dir string) ([]hostEntry, error) {
	return nil, a.err
}

func (a authFailHost) Read(_ context.Context, name, file string) ([]byte, error) {
	return nil, a.err
}

// TestHostRunErrorIncludesBoundedStderr pins the @host transport contract:
// a failed outpost invocation carries its bounded, trimmed stderr (the
// actionable guidance, as @sandbox does via runRecipe), stays wrapped for
// errors.Is, never grows past hostStderrLimit, and strips NULs.
func TestHostRunErrorIncludesBoundedStderr(t *testing.T) {
	runErr := errors.New("exit status 1")
	args := []string{"ssh", "sftp", "ls", "dogtest", "."}

	var withMsg hostStderr
	_, _ = withMsg.Write([]byte("Error: elevation required for novidesign \u2014 run outpost connect novidesign\n"))
	err := hostRunError(args, runErr, &withMsg)
	if !strings.Contains(err.Error(), "elevation required for novidesign") ||
		!strings.Contains(err.Error(), "run outpost connect novidesign") {
		t.Fatalf("guidance lost: %q", err)
	}
	if !errors.Is(err, runErr) {
		t.Fatalf("run error unwrapped: %q", err)
	}

	if err := hostRunError(args, runErr, &hostStderr{}); strings.HasSuffix(err.Error(), ": ") ||
		!errors.Is(err, runErr) {
		t.Fatalf("empty stderr should fall back to the bare error: %q", err)
	}

	big := &hostStderr{}
	if _, err := big.Write([]byte(strings.Repeat("x", 2*hostStderrLimit))); err != nil {
		t.Fatal(err)
	}
	if len(big.buf) != hostStderrLimit {
		t.Fatalf("stderr capture=%d bytes, want bound %d", len(big.buf), hostStderrLimit)
	}
	if got := hostRunError(args, runErr, big).Error(); !strings.HasSuffix(got, string(big.buf)) {
		t.Fatalf("bounded stderr not carried verbatim, got %d chars", len(got))
	}

	var nul hostStderr
	_, _ = nul.Write([]byte("re-elevate novidesign:\x00 no TTY for password prompt\n"))
	if got := hostRunError(args, runErr, &nul).Error(); strings.Contains(got, "\x00") ||
		!strings.Contains(got, "no TTY for password prompt") {
		t.Fatalf("stderr not sanitized: %q", got)
	}
}

// TestHostScopeSurfacesAuthGuidance proves a readable auth failure reaches
// the user: when the sftp transport reports an elevation refusal, the lens
// CLI surfaces that guidance on stderr instead of a bare exit status. No
// live peer is contacted: peers, targets and the transport are all canned,
// and no credential is acquired or modified.
func TestHostScopeSurfacesAuthGuidance(t *testing.T) {
	hostFixture(t, hostPeers)
	oldT := hostTransportImpl
	hostTransportImpl = authFailHost{err: errors.New("host: outpost ssh sftp ls mac-target src: exit status 1: Error: elevation required for novidesign \u2014 run outpost connect novidesign")}
	defer func() { hostTransportImpl = oldT }()
	out, errOut, code := runTool(t, t.TempDir(), "", "-t", "graph", "dir:src@mac")
	if code == 0 || out != "" {
		t.Fatalf("code=%d out=%q stderr=%q, want a surfaced failure", code, out, errOut)
	}
	if !strings.Contains(errOut, "elevation required") || !strings.Contains(errOut, "outpost connect") {
		t.Fatalf("auth guidance lost from stderr: %q", errOut)
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
