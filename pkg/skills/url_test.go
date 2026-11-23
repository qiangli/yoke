package skills

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAddURLSkill(t *testing.T) {
	const skill = "---\nname: sample-site\ndescription: A sample skill for a site\n---\n\n# Sample skill\n"
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(skill)))
	for _, route := range []string{"/guide", skillIndexPath, "/skill.md", "/SKILL.md", "fallback", "bad-digest", "invalid-skill"} {
		t.Run(route, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case skillIndexPath:
					if route == "fallback" {
						http.NotFound(w, r)
						return
					}
					sha := digest
					if route == "bad-digest" {
						sha = strings.Repeat("0", 64)
					}
					if route == "invalid-skill" {
						fmt.Fprint(w, `{"version":"0.2.0","skills":[{"name":"sample-site","type":"skill-md","url":"/skill.md"}]}`)
						return
					}
					fmt.Fprintf(w, `{"version":"0.2.0","skills":[{"name":"sample-site","type":"skill-md","url":"/skill.md","sha256":"%s"}]}`, sha)
				case "/skill.md", "/SKILL.md":
					if route == "invalid-skill" {
						fmt.Fprint(w, "---\nname: sample-site\n---\n")
						return
					}
					fmt.Fprint(w, skill)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			store := t.TempDir()
			cmd := NewSkillsCmd(WithConfigDir(store))
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			arg := route
			if !strings.HasPrefix(arg, "/") {
				arg = "/"
			}
			cmd.SetArgs([]string{"add", "--json", server.URL + arg})
			err := cmd.Execute()
			dest := filepath.Join(store, "sample-site", "SKILL.md")
			if route == "bad-digest" || route == "invalid-skill" {
				if err == nil {
					t.Fatal("admission unexpectedly succeeded")
				}
				if route == "bad-digest" && !strings.Contains(err.Error(), "SHA-256 mismatch") {
					t.Fatalf("digest: %v", err)
				}
				if route == "invalid-skill" && !strings.Contains(out.String(), "description missing") {
					t.Fatalf("admission report: %s", out.String())
				}
				if _, err := os.Stat(dest); !os.IsNotExist(err) {
					t.Fatalf("rejected skill left installed: %v", err)
				}
				return
			}
			got, readErr := os.ReadFile(dest)
			if err != nil || readErr != nil || string(got) != skill {
				t.Fatalf("add: %s %v; read: %q %v", out.String(), err, got, readErr)
			}
		})
	}
}
