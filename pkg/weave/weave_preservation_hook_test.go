//go:build !windows

package weave

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Run the actual sprint command from Git's installed commit-msg hook, in a
// separate test process. No dependency on the operator's installed bashy build.
func TestPreservationHookProcess(t *testing.T) {
	if os.Getenv("WEAVE_PRESERVATION_HOOK_HELPER") != "1" {
		return
	}
	args := os.Args
	for i, arg := range args {
		if arg == "--" {
			cmd := NewSprintCmd()
			cmd.SetArgs(args[i+2:])
			if err := cmd.Execute(); err != nil {
				os.Exit(1)
			}
			os.Exit(0)
		}
	}
	os.Exit(2)
}

func TestForcedAbandonWithSprintHook(t *testing.T) {
	for _, valid := range []bool{true, false} {
		t.Run(fmt.Sprint(valid), func(t *testing.T) {
			root, qdir, it := workerStoryFixture(t)
			t.Chdir(root)
			// Reproduce the ad-hoc brief form: no Register and no local auto-claim record.
			it.State = "failed"
			if !valid {
				it.Body = "No explicit story"
			}
			if err := saveWeaveQueue(qdir, &weaveQueue{Root: root, Items: []*weaveItem{it}}); err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			shim := "#!/bin/sh\nexec '" + strings.ReplaceAll(exe, "'", "'\\''") + "' -test.run=^TestPreservationHookProcess$ -- \"$@\"\n"
			if err := os.WriteFile(filepath.Join(bin, "bashy"), []byte(shim), 0755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("WEAVE_PRESERVATION_HOOK_HELPER", "1")
			if _, err := installSprintCommitHook(it.Workspace); err != nil {
				t.Fatal(err)
			}
			mustWrite(t, filepath.Join(it.Workspace, "seed.txt"), "recoverable tracked work\n")
			mustWrite(t, filepath.Join(it.Workspace, "new.txt"), "recoverable new work\n")
			out, code := runWeave(t, "abandon", "1", "--force", "--json")
			if valid {
				if code != 0 {
					t.Fatalf("forced abandon with real sprint hook: exit=%d\n%s", code, out)
				}
				for _, path := range []string{"seed.txt", "new.txt"} {
					got := gitT(t, root, "show", "refs/salvage/abandoned-1:"+path)
					if !strings.Contains(got, "recoverable") {
						t.Fatalf("lost %s: %s", path, got)
					}
				}
				msg := gitT(t, root, "log", "-1", "--format=%B", "refs/salvage/abandoned-1")
				if _, err := parseCommitTrace(msg); err != nil {
					t.Fatalf("invalid preserved trailers: %v\n%s", err, msg)
				}
				if !strings.Contains(msg, "NOT submitted") {
					t.Fatal("preservation misrepresented as delivery")
				}
			} else {
				if code == 0 || !strings.Contains(out, "refusing to destroy") {
					t.Fatalf("unattributed hook refusal: exit=%d %s", code, out)
				}
				for _, path := range []string{"seed.txt", "new.txt"} {
					data, err := os.ReadFile(filepath.Join(it.Workspace, path))
					if err != nil || !strings.Contains(string(data), "recoverable") {
						t.Fatalf("refusal lost %s: %v %s", path, err, data)
					}
				}
			}
		})
	}
}
