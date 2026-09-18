package agentctl

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The first thing several agent CLIs do in a directory they have not seen before
// is stop and ask whether they may read it. On a human's terminal that is a
// reasonable question. To an unattended launcher it is a wall: the agent produces
// nothing, exits nothing, and eventually trips an idle timeout that reports the
// wrong cause entirely.
//
// There are two ways past it, and a fleet wants both.
//
// PREVENTION (this file): write the tool's own config so the prompt never
// appears. Quiet, instant, and it works even without a terminal.
//
// CURE (pkg/agentpty's gate classifier): watch the live output, recognise the
// prompt, and press the key. Needed because a preseed cannot cover every prompt
// every tool will ever invent, and because a config file can be stale.
//
// Prevention is tried first because a prompt that never appears cannot be
// mis-answered.

// ApplyTrustPreseed writes whatever config suppresses a tool's first-run trust
// prompt for this workspace. An unknown preseed is a no-op, not an error: a tool
// we do not have a recipe for is one whose prompt we will clear reactively
// instead, and failing the launch would be strictly worse than trying.
func ApplyTrustPreseed(workspace, preseed string) error {
	switch strings.TrimSpace(preseed) {
	case "", "none":
		return nil
	case ".claude.json":
		return preseedClaudeTrust(workspace)
	case "opencode.json":
		return preseedOpencodeTrust(workspace)
	default:
		return nil
	}
}

// preseedOpencodeTrust grants opencode its permissions WITHOUT destroying the
// project's own config.
//
// This used to be a blind os.WriteFile of a permissions-only blob, which
// overwrote any existing opencode.json — taking the project's provider settings
// with it. That matters more than it sounds: opencode reads its model endpoints
// from that file (a Moonshot baseURL, say — the API has separate international
// and China hosts, and the wrong one fails opaquely). So bashy would silently
// delete the very configuration the agent needed, and the agent would fail with
// an "Unexpected server error" that pointed nowhere near the cause.
//
// The claude preseed has always merged. This one did not, and nobody noticed
// because the failure surfaced as somebody else's bug.
func preseedOpencodeTrust(workspace string) error {
	workspace = strings.TrimSpace(workspace)
	if workspace == "" {
		return fmt.Errorf("agentctl: empty workspace")
	}
	path := filepath.Join(workspace, "opencode.json")

	doc := map[string]any{}
	if b, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(b))) > 0 {
		if err := json.Unmarshal(b, &doc); err != nil {
			// A config we cannot parse is a config we must not replace. Leave it
			// alone and let opencode complain about its own file — that error at
			// least points at the truth.
			return fmt.Errorf("parse %s: %w", path, err)
		}
	}

	// Only touch `permission`, and only the keys we actually need. Anything else
	// in the file — provider, model, mcp, agent — is the project's business.
	perm, _ := doc["permission"].(map[string]any)
	if perm == nil {
		perm = map[string]any{}
	}
	for _, k := range []string{"edit", "bash", "webfetch", "external_directory"} {
		if _, set := perm[k]; !set {
			perm[k] = "allow"
		}
	}
	doc["permission"] = perm

	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		return err
	}
	// And hide it from git. We just wrote a file into a repository the agent is
	// about to commit from -- so without this, BASHY'S OWN preseed shows up as an
	// untracked file in the workspace, and an agent told to "commit your work"
	// dutifully commits our plumbing into the artifact. The tool that set up the
	// run would be polluting the diff it is judging.
	//
	// .git/info/exclude, not .gitignore: it is local to the clone, so we never
	// modify a file the project tracks.
	excludeFromGit(workspace, "opencode.json")
	return nil
}

// excludeFromGit adds a path to the repo's LOCAL exclude list (.git/info/exclude),
// which is not tracked and so cannot leak into a commit. Best-effort: a workspace
// that is not a git repo needs no exclusion, and a failure here must never stop a
// launch.
func excludeFromGit(workspace, name string) {
	gitDir := filepath.Join(workspace, ".git")
	info, err := os.Stat(gitDir)
	if err != nil {
		return
	}
	if !info.IsDir() {
		// A worktree: .git is a FILE pointing at the real gitdir. weave uses these,
		// so this is the common case, not the exotic one.
		b, err := os.ReadFile(gitDir)
		if err != nil {
			return
		}
		line := strings.TrimSpace(string(b))
		if !strings.HasPrefix(line, "gitdir:") {
			return
		}
		gitDir = strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
		if !filepath.IsAbs(gitDir) {
			gitDir = filepath.Join(workspace, gitDir)
		}
	}
	path := filepath.Join(gitDir, "info", "exclude")
	if b, err := os.ReadFile(path); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if strings.TrimSpace(l) == name {
				return // already excluded
			}
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString("\n# written by bashy: agent trust preseed, not project config\n" + name + "\n")
}

// preseedClaudeTrust marks one workspace as already-trusted in ~/.claude.json.
//
// It MERGES into the existing document rather than writing a fresh one. That file
// is the user's real claude config — their projects, their onboarding state — and
// clobbering it to skip a prompt would be a spectacular trade.
func preseedClaudeTrust(workspace string) error {
	workspace = strings.TrimSpace(workspace)
	if workspace == "" {
		return fmt.Errorf("agentctl: empty workspace")
	}
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	path := filepath.Join(home, ".claude.json")

	var doc map[string]any
	if b, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(b))) > 0 {
		if err := json.Unmarshal(b, &doc); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
	} else {
		doc = map[string]any{}
	}
	projects, ok := doc["projects"].(map[string]any)
	if !ok {
		projects = map[string]any{}
		doc["projects"] = projects
	}
	project, ok := projects[abs].(map[string]any)
	if !ok {
		project = map[string]any{}
		projects[abs] = project
	}
	project["hasTrustDialogAccepted"] = true
	project["hasCompletedProjectOnboarding"] = true

	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}
