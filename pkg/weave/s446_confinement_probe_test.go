package weave

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Diagnostic, not a desired security contract: remove/replace this probe when a
// confined executor is available. It deliberately exercises only disposable
// sibling files, a loopback listener, and a harmless host executable. No script
// admission is bypassed: this probes the generic native process boundary that
// a proposed Go execution profile would otherwise trust.
func TestS446NativeConfinementProbe(t *testing.T) {
	if os.Getenv("S446_CONFINEMENT_PROBE") != "1" {
		t.Skip("opt-in diagnostic of unconfined native Go execution")
	}
	if runtime.GOOS != "darwin" {
		t.Skip("run509 diagnostic is for the current Darwin host")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	work := filepath.Join(root, "workspace")
	if err := os.Mkdir(work, 0700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"go.mod": "module confinementprobe\n\ngo 1.27\n",
		"probe_test.go": `package confinementprobe
import ("net"; "os"; "os/exec"; "testing")
func TestEffects(t *testing.T) {
 if err := os.WriteFile("../outside-proof", []byte("outside workspace"), 0600); err != nil { t.Fatal(err) }
 listener, err := net.Listen("tcp", "127.0.0.1:0"); if err != nil { t.Fatal(err) }; listener.Close()
 if err := exec.Command("/usr/bin/true").Run(); err != nil { t.Fatal(err) }
 if err := os.WriteFile("../network-tool-proof", []byte("loopback listen and host tool succeeded"), 0600); err != nil { t.Fatal(err) }
}
`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(work, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(goBin, "test", "-count=1", ".")
	cmd.Dir = work
	cmd.Env = []string{"HOME=" + root, "PATH=" + filepath.Dir(goBin) + ":/usr/bin:/bin", "TMPDIR=" + root,
		"GOCACHE=" + filepath.Join(root, "cache"), "GOMODCACHE=" + filepath.Join(root, "modules"),
		"GOTOOLCHAIN=local", "GOWORK=off", "GOPROXY=off", "GOSUMDB=off", "CGO_ENABLED=0", "GOENV=off", "GOTELEMETRY=off", "GOFLAGS=-p=2", "GOMAXPROCS=4"}
	var output bytes.Buffer
	code, reason, _, _, err := runWeaveToolPTY(cmd, &output, weaveGuards{maxRuntime: time.Minute})
	if err != nil || code != 0 || reason != "" || !strings.Contains(output.String(), "ok") {
		t.Fatalf("probe did not establish effects: code=%d reason=%q err=%v output=%s", code, reason, err, output.String())
	}
	for name, want := range map[string]string{"outside-proof": "outside workspace", "network-tool-proof": "loopback listen and host tool succeeded"} {
		got, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || string(got) != want {
			t.Fatalf("%s: %q %v", name, got, err)
		}
	}
	t.Log("BLOCKER CONFIRMED: literal offline go test in native PTY can write outside cwd, open a loopback socket, and execute a host tool; environment/cwd/watchdog are not confinement")
}
