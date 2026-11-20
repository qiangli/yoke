package agentctl

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/qiangli/coreutils/pkg/lockfile"
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
		workspace = strings.TrimSpace(workspace)
		if workspace == "" {
			return fmt.Errorf("agentctl: empty workspace")
		}
		dir, err := filepath.Abs(workspace)
		if err != nil {
			return err
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		path := filepath.Join(home, preseed)
		if err := mergeClaudeTrust(path, dir, true); err != nil {
			return err
		}
		// claude keys the project by the symlink-resolved cwd (/tmp/x is
		// looked up as /private/tmp/x on macOS), so trust that spelling too.
		if real, err := filepath.EvalSymlinks(dir); err == nil && real != dir {
			return mergeClaudeTrust(path, real, true)
		}
		return nil
	case "opencode.json":
		return preseedOpencodeTrust(workspace)
	case "codex.toml":
		return preseedCodexTrust(workspace)
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

// ApplyLaunchTrustPreseed applies a seed using the vendor's effective launch
// environment. The returned entry describes the exact workspace grant.
func ApplyLaunchTrustPreseed(workspace, preseed string, env []string) (string, error) {
	switch preseed {
	case ".claude.json", ".gemini/antigravity-cli/settings.json", "codex.toml", "opencode.json":
	default:
		return "", nil
	}
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return "", err
	}
	dir, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	homeKey := "HOME"
	if os.PathSeparator == '\\' {
		homeKey = "USERPROFILE"
	}
	home := launchEnv(env, homeKey)
	if home == "" {
		home, err = os.UserHomeDir()
		if err != nil {
			return "", err
		}
	}
	configDir := func(key, fallback string) string {
		if value := launchEnv(env, key); value != "" {
			if !filepath.IsAbs(value) {
				return filepath.Join(dir, value)
			}
			return value
		}
		return fallback
	}
	switch preseed {
	case ".claude.json":
		path := filepath.Join(configDir("CLAUDE_CONFIG_DIR", home), preseed)
		return fmt.Sprintf("%s projects[%q].hasTrustDialogAccepted=true", path, dir), mergeClaudeTrust(path, dir, false)
	case ".gemini/antigravity-cli/settings.json":
		path := filepath.Join(configDir("GEMINI_CLI_HOME", home), preseed)
		return fmt.Sprintf("%s trustedWorkspaces += %q", path, dir), mergeTrustJSON(path, func(doc map[string]any) bool {
			trusted, _ := doc["trustedWorkspaces"].([]any)
			for _, entry := range trusted {
				if entry == dir {
					return false
				}
			}
			doc["trustedWorkspaces"] = append(trusted, dir)
			return true
		})
	case "codex.toml":
		path := filepath.Join(configDir("CODEX_HOME", filepath.Join(home, ".codex")), "config.toml")
		written, err := appendCodexTrust(path, dir)
		if !written || err != nil {
			return "", err
		}
		return fmt.Sprintf("%s projects[%q].trust_level=trusted", path, dir), nil
	default:
		return fmt.Sprintf("%s permissions for %q", filepath.Join(dir, preseed), dir), preseedOpencodeTrust(dir)
	}
}

func launchEnv(env []string, name string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if value, ok := strings.CutPrefix(env[i], name+"="); ok {
			return value
		}
	}
	return ""
}

func mergeClaudeTrust(path, dir string, onboarding bool) error {
	return mergeTrustJSON(path, func(doc map[string]any) bool {
		projects, _ := doc["projects"].(map[string]any)
		if projects == nil {
			projects = map[string]any{}
			doc["projects"] = projects
		}
		project, _ := projects[dir].(map[string]any)
		if project == nil {
			project = map[string]any{}
			projects[dir] = project
		}
		changed := project["hasTrustDialogAccepted"] != true
		project["hasTrustDialogAccepted"] = true
		if onboarding && project["hasCompletedProjectOnboarding"] != true {
			project["hasCompletedProjectOnboarding"] = true
			changed = true
		}
		return changed
	})
}

func mergeTrustJSON(path string, merge func(map[string]any) bool) error {
	// Follow an existing config symlink so replacement preserves the link.
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	} else if info, statErr := os.Lstat(path); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	lock, err := lockfile.AcquireWithin(path+".bashy-trust.lock", 3*time.Second, lockfile.Holder{Intent: "preseed workspace trust"})
	if err != nil {
		return err
	}
	defer lock.Release()
	doc := map[string]any{}
	mode := os.FileMode(0o600)
	file, err := os.Open(path)
	if err == nil {
		info, statErr := file.Stat()
		file.Close()
		if statErr != nil {
			return statErr
		}
		mode = info.Mode().Perm()
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if len(strings.TrimSpace(string(data))) != 0 {
			decoder := json.NewDecoder(bytes.NewReader(data))
			decoder.UseNumber()
			if err := decoder.Decode(&doc); err != nil {
				return fmt.Errorf("parse %s: %w", path, err)
			}
			if doc == nil || !json.Valid(data) {
				return fmt.Errorf("parse %s: expected a JSON object", path)
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if !merge(doc) {
		return nil
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".bashy-trust-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	err = tmp.Chmod(mode)
	if err == nil {
		_, err = tmp.Write(append(data, '\n'))
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// preseedCodexTrust marks a workspace trusted in codex's own config, the way
// codex records the operator's answer to its "Trust this folder?" dialog:
//
//	[projects."<git top-level, symlinks resolved>"]
//	trust_level = "trusted"
//
// (codex-cli 0.156.1, measured 2026-09-24: answering the dialog in a repo's
// subdirectory saved the repo root, under /private/tmp for a /tmp path.)
//
// It APPENDS one table and never rewrites the file. config.toml is the
// operator's — thousands of lines, and codex rewrites it too — and a TOML round
// trip would reorder or drop what it does not model. A project that already has
// a table is left alone, including one the operator marked untrusted.
func preseedCodexTrust(workspace string) error {
	workspace = strings.TrimSpace(workspace)
	if workspace == "" {
		return fmt.Errorf("agentctl: empty workspace")
	}
	key, err := codexTrustKey(workspace)
	if err != nil {
		return err
	}
	dir := strings.TrimSpace(os.Getenv("CODEX_HOME"))
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		dir = filepath.Join(home, ".codex")
	}
	_, err = appendCodexTrust(filepath.Join(dir, "config.toml"), key)
	return err
}

func appendCodexTrust(path, key string) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	lock, err := lockfile.AcquireWithin(path+".bashy-trust.lock", 3*time.Second, lockfile.Holder{Intent: "preseed workspace trust"})
	if err != nil {
		return false, err
	}
	defer lock.Release()
	header := "[projects." + tomlBasicString(key) + "]"
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	for _, line := range strings.Split(string(existing), "\n") {
		if strings.TrimSpace(line) == header {
			return false, nil
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return false, err
	}
	defer f.Close()
	block := header + "\ntrust_level = \"trusted\"\n"
	if len(existing) > 0 {
		block = "\n" + block
		if !strings.HasSuffix(string(existing), "\n") {
			block = "\n" + block
		}
	}
	_, err = f.WriteString(block)
	return err == nil, err
}

// codexTrustKey is the directory codex keys its trust on: the enclosing git
// work tree's top level (a .git directory, or the .git FILE of a submodule or
// linked worktree), else the workspace itself — symlinks resolved either way.
func codexTrustKey(workspace string) (string, error) {
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	for dir := abs; ; {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return abs, nil
		}
		dir = parent
	}
}

// tomlBasicString quotes s as a TOML basic string.
func tomlBasicString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, "\\u%04X", r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
