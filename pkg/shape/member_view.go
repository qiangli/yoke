package shape

import (
	"os"
	"path/filepath"
	"strings"
)

// MemberView is the candidate as a caller other than the owner may see it:
// the same record without the host's paths and identities. The workspace,
// tracking store, artifact and preview locations, the owner's board item, the
// host process actor, the owner's activation identity and the agent receipt
// are dropped; the verifier record is named by its public reference
// (ShapeEvidenceReference) instead of its path; failure text keeps its
// meaning with the known host roots removed. The stored candidate is not
// changed.
func MemberView(c Candidate) Candidate {
	roots := []string{c.Workspace, c.TrackingRoot}
	if c.Verdict != nil && c.Verdict.Evidence != "" {
		roots = append(roots, filepath.Dir(c.Verdict.Evidence))
	}
	home, _ := os.UserHomeDir()
	scrub := func(text string) string {
		for _, root := range roots {
			text = RelativizeWorkspace(text, root)
		}
		for _, h := range workspaceSpellings(home) {
			text = strings.ReplaceAll(text, h, "~")
		}
		return text
	}
	c.Error = scrub(c.Error)
	if c.Verdict != nil {
		v := *c.Verdict
		v.Detail, v.ArtifactPath = scrub(v.Detail), ""
		if v.Evidence != "" {
			v.Evidence, _ = ShapeEvidenceReference(c.Rev)
		}
		c.Verdict = &v
	}
	c.Workspace, c.TrackingRoot, c.TrackingItem, c.ArtifactPath = "", "", "", ""
	c.Actor, c.ActivationPrincipal, c.Identity, c.Receipt = "", "", "", ""
	c.ProgramPreview = nil
	return c
}
