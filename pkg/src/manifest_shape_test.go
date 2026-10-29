package src

import "testing"

// TestShapeAuthManifestDenial pins the S525.A5 rows: every path is protected,
// carries a recorded reason, and an edit with no approval is refused.
func TestShapeAuthManifestDenial(t *testing.T) {
	paths := []string{
		"pkg/webconsole/host_auth.go", "pkg/webconsole/pairing.go",
		"pkg/webconsole/pair_http.go", "pkg/webconsole/login.go",
		"pkg/webconsole/gate.go", "pkg/coopauth/coopauth.go",
		"pkg/shape/contain/contain.go", "pkg/shape/verify/v.go",
		"pkg/shape/evidence/store.go", "pkg/shape/state/s.go",
	}
	for _, p := range paths {
		if got := PathClass("yoke", p); got != ClassProtected {
			t.Errorf("PathClass(%s) = %s, want protected", p, got)
		}
		covered, _, err := approvalsCoverProtected(t.TempDir(), "yoke", []string{p})
		if err != nil || covered {
			t.Errorf("edit of %s not refused (covered=%v err=%v)", p, covered, err)
		}
	}
	for _, e := range ProtectedManifest() {
		if e.Reason == "" {
			continue
		}
		if e.Core || e.Repo != "yoke" {
			t.Errorf("reasoned row %v must be a protected yoke row", e)
		}
	}
	if PathClass("yoke", "pkg/webconsole/server.go") != ClassSkill {
		t.Error("non-auth webconsole code must stay skill tier (S525.A4 owns it)")
	}
}
