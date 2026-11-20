package agentctl

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLaunchTrustPreseedUsesVendorHomeAndCanonicalCwd(t *testing.T) {
	for _, seed := range []struct {
		file, env, before, after string
	}{
		{".claude.json", "CLAUDE_CONFIG_DIR",
			`{"projects":{"other":{"hasTrustDialogAccepted":false},"WORKSPACE":{"existing":true}},"vendor":42}`,
			`{"projects":{"other":{"hasTrustDialogAccepted":false},"WORKSPACE":{"existing":true,"hasTrustDialogAccepted":true}},"vendor":42}`},
		{".gemini/antigravity-cli/settings.json", "GEMINI_CLI_HOME",
			`{"trustedWorkspaces":["other"],"vendor":42}`,
			`{"trustedWorkspaces":["other","WORKSPACE"],"vendor":42}`},
	} {
		t.Run(seed.env, func(t *testing.T) {
			vendorHome := t.TempDir()
			workspace, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			alias := filepath.Join(t.TempDir(), "alias")
			if err := os.Symlink(workspace, alias); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			path := filepath.Join(vendorHome, seed.file)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			quoted, err := json.Marshal(workspace)
			if err != nil {
				t.Fatal(err)
			}
			setWorkspace := func(s string) string {
				return strings.ReplaceAll(s, `"WORKSPACE"`, string(quoted))
			}
			if err := os.WriteFile(path, []byte(setWorkspace(seed.before)), 0o600); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				entry, err := ApplyLaunchTrustPreseed(alias, seed.file, []string{seed.env + "=" + vendorHome})
				if err != nil || !strings.Contains(entry, string(quoted)) {
					t.Fatalf("entry=%q err=%v", entry, err)
				}
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var got, want any
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(setWorkspace(seed.after)), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("config=%s, want %s", data, setWorkspace(seed.after))
			}
		})
	}
}
