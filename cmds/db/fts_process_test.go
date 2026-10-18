package dbcmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Run on a remote gate host: this intentionally starts independent CLI and
// owner processes. It is excluded from Dragon's named in-process test gate.
func TestIndependentFTSClientsBothModes(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BASHY_COORD_DIR", filepath.Join(root, "coord"))
	t.Setenv("BASHY_DB_SESSION_DIR", filepath.Join(root, "sessions"))
	call := func(stdin string, args ...string) (map[string]any, error) {
		cmd := exec.Command(os.Args[0], append([]string{"db"}, args...)...)
		cmd.Env = os.Environ()
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("%v: %w", args, err)
		}
		var doc map[string]any
		if err := json.Unmarshal(out, &doc); err != nil {
			return nil, err
		}
		if doc["ok"] != true {
			return nil, fmt.Errorf("%v: %s", args, out)
		}
		return doc, nil
	}
	owner, err := call("", "session", "start", "--idle-ttl", "2m")
	if err != nil {
		t.Fatal(err)
	}
	session := owner["result"].(map[string]any)["id"].(string)
	defer call("", "session", "stop", "--session", session, "--yes")
	for _, target := range [][]string{
		{"--engine", "bleve", "--mode", "persistent", "--path", filepath.Join(root, "persistent")},
		{"--engine", "bleve", "--mode", "memory", "--session", session, "--name", "shared"},
	} {
		name := target[3]
		t.Run(name, func(t *testing.T) {
			if _, err := call("", append([]string{"create"}, target...)...); err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			errs := make(chan error, 2)
			for i := 0; i < 2; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					input := fmt.Sprintf(`{"id":"client-%d","doc":{"text":"shared lexical"}}`, i)
					_, err := call(input, append([]string{"fts", "put", "--input", "-", "--wait", "10s"}, target...)...)
					errs <- err
				}(i)
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatal(err)
				}
			}
			found, err := call(`{"query":"shared"}`, append([]string{"fts", "search", "--input", "-"}, target...)...)
			if err != nil {
				t.Fatal(err)
			}
			if found["result"].(map[string]any)["total"] != float64(2) {
				t.Fatalf("independent clients: %+v", found)
			}
		})
	}
}
