package atlas_test

import (
	"os/exec"
	"strings"
	"sync"
	"testing"
)

var (
	coreutilsDirOnce sync.Once
	coreutilsDirPath string
	coreutilsDirErr  error
)

// coreutilsDir is the certified coreutils module the census spans: the live
// tree inside the dhnt go.work, the pinned module in a standalone clone.
func coreutilsDir(t *testing.T) string {
	t.Helper()
	coreutilsDirOnce.Do(func() {
		var out []byte
		out, coreutilsDirErr = exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/qiangli/coreutils").Output()
		coreutilsDirPath = strings.TrimSpace(string(out))
	})
	if coreutilsDirErr != nil || coreutilsDirPath == "" {
		t.Fatalf("locate the coreutils module (go list -m): %v", coreutilsDirErr)
	}
	return coreutilsDirPath
}
