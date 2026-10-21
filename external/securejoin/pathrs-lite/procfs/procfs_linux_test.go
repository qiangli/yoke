//go:build linux

package procfs

import (
	"io"
	"os"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func readHandle(t *testing.T, f *os.File) string {
	t.Helper()
	rf, err := os.OpenFile("/proc/self/fd/"+strconv.Itoa(int(f.Fd())), os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()
	b, err := io.ReadAll(rf)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestProcRootHandles(t *testing.T) {
	proc, err := OpenProcRoot()
	if err != nil {
		t.Fatalf("OpenProcRoot: %v", err)
	}
	defer proc.Close()

	f, err := proc.OpenSelf("status")
	if err != nil {
		t.Fatalf("OpenSelf(status): %v", err)
	}
	if got := readHandle(t, f); !strings.Contains(got, "Pid:\t"+strconv.Itoa(os.Getpid())) {
		t.Errorf("OpenSelf(status) is not this process: %q", got)
	}
	f.Close()

	f, err = proc.OpenPid(os.Getpid(), "stat")
	if err != nil {
		t.Fatalf("OpenPid: %v", err)
	}
	if got := readHandle(t, f); !strings.HasPrefix(got, strconv.Itoa(os.Getpid())+" ") {
		t.Errorf("OpenPid(stat) = %q", got)
	}
	f.Close()

	f, closer, err := proc.OpenThreadSelf("stat")
	if err != nil {
		t.Fatalf("OpenThreadSelf: %v", err)
	}
	if got := readHandle(t, f); !strings.HasPrefix(got, strconv.Itoa(unix.Gettid())+" ") {
		t.Errorf("OpenThreadSelf(stat) not this thread: %q", got)
	}
	f.Close()
	closer()
}

// TestProcSubpathConfined: a procfs subpath must not walk out of the
// pid directory into the host filesystem or through magic links.
func TestProcSubpathConfined(t *testing.T) {
	proc, err := OpenProcRoot()
	if err != nil {
		t.Fatal(err)
	}
	defer proc.Close()
	for _, sub := range []string{"../../etc/passwd", "/etc/passwd", "root/etc/passwd", "cwd/x"} {
		if f, err := proc.OpenSelf(sub); err == nil {
			f.Close()
			t.Errorf("OpenSelf(%q) succeeded, want refusal", sub)
		}
	}
	if err := proc.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}
