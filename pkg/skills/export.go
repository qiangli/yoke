package skills

// L2 of the advertisement ladder: exporting catalog skills into the
// directories agentic tools actually read. Standard skill folders are
// portable by design (agentskills.io); this file only decides WHERE to
// write and owns the etiquette:
//
//   - the vendor-neutral convergence home is `.agents/skills/`
//     (user: ~/.agents/skills — Codex primary, Goose recommended,
//     Gemini/opencode alias); Claude Code and Copilot CLI still read
//     only their own roots, which are written when DETECTED (the
//     Sentry pattern: any detected agent root, never creating a
//     vendor's config dir the user doesn't have);
//   - every export carries an ownership marker (.bashy-export.json);
//     re-exports refresh only marker-carrying dirs — a skill folder we
//     did not write is never clobbered without --force;
//   - repo-scope writes happen ONLY via the explicit --repo flag
//     (writing into a user's repository is scaffold-consent territory);
//     user-scope and --to are the defaults of choice.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

const exportMarker = ".bashy-export.json"

type exportRecord struct {
	Name       string    `json:"name"`
	Identity   string    `json:"identity,omitempty"`
	ExportedAt time.Time `json:"exported_at"`
	By         string    `json:"by"`
}

// ExportTo writes one catalog skill's folder to <dstRoot>/<name>/, with
// the ownership marker. An existing destination is refreshed when it
// carries our marker, refused otherwise unless force.
func ExportTo(sk Skill, src Source, dstRoot string, force bool) (string, error) {
	dst := filepath.Join(dstRoot, sk.Name)
	if _, err := os.Stat(dst); err == nil {
		if _, merr := os.Stat(filepath.Join(dst, exportMarker)); merr != nil && !force {
			return "", fmt.Errorf("skills: %s exists and was not exported by us — use --force to replace", dst)
		}
		if err := os.RemoveAll(dst); err != nil {
			return "", err
		}
	}
	files, err := src.Files(sk.Name)
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return "", fmt.Errorf("skills: %q has no files", sk.Name)
	}
	for _, rel := range files {
		data, ok := src.File(sk.Name, rel)
		if !ok {
			continue
		}
		target := filepath.Join(dst, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			_ = os.RemoveAll(dst)
			return "", err
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			_ = os.RemoveAll(dst)
			return "", err
		}
	}
	rec := exportRecord{Name: sk.Name, ExportedAt: time.Now().UTC(), By: "bashy skill export"}
	if sk.Dhnt.Valid() {
		rec.Identity = sk.Dhnt.Identity
	}
	data, _ := json.MarshalIndent(rec, "", "  ")
	if err := os.WriteFile(filepath.Join(dst, exportMarker), data, 0o644); err != nil {
		_ = os.RemoveAll(dst)
		return "", err
	}
	return dst, nil
}

// userExportRoots returns the user-scope skill directories to write:
// the vendor-neutral ~/.agents/skills always, plus each vendor root
// whose parent config dir is DETECTED on this host.
func userExportRoots(home string) []string {
	roots := []string{filepath.Join(home, ".agents", "skills")}
	for _, vendor := range []string{".claude", ".copilot"} {
		if _, err := os.Stat(filepath.Join(home, vendor)); err == nil {
			roots = append(roots, filepath.Join(home, vendor, "skills"))
		}
	}
	return roots
}

// repoExportRoots returns the repo-scope skill directories: the
// vendor-neutral .agents/skills always, plus .claude/skills when the
// repo already has a .claude dir.
func repoExportRoots(repoRoot string) []string {
	roots := []string{filepath.Join(repoRoot, ".agents", "skills")}
	if _, err := os.Stat(filepath.Join(repoRoot, ".claude")); err == nil {
		roots = append(roots, filepath.Join(repoRoot, ".claude", "skills"))
	}
	return roots
}

// findRepoRoot walks up from dir to the enclosing git repository root,
// falling back to dir itself.
func findRepoRoot(dir string) string {
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		if filepath.Dir(d) == d {
			return dir
		}
	}
}

// Provision writes the named catalog skills into a workspace the caller
// OWNS (a weave clone, a foreman session dir): both the vendor-neutral
// .agents/skills and .claude/skills, so any agent brand launched inside
// finds them. This is the orchestrator channel of the advertisement
// ladder — no consent question arises because the workspace is ours.
func Provision(workspace string, names []string, log io.Writer, opts ...Option) {
	cfg := &config{statics: map[string]string{}, cacheTTL: 24 * time.Hour}
	for _, o := range opts {
		o(cfg)
	}
	if cfg.cfgDir == "" {
		cfg.cfgDir = defaultConfigDir()
	}
	cat := cfg.catalog()
	var done []string
	for _, name := range names {
		sk, src, ok := cat.Get(name)
		if !ok {
			fmt.Fprintf(log, "skills: provision: %q not found (skipped)\n", name)
			continue
		}
		for _, root := range []string{
			filepath.Join(workspace, ".agents", "skills"),
			filepath.Join(workspace, ".claude", "skills"),
		} {
			if _, err := ExportTo(sk, src, root, true); err != nil {
				fmt.Fprintf(log, "skills: provision %s: %v\n", name, err)
			}
		}
		done = append(done, name)
	}
	if len(done) > 0 {
		fmt.Fprintf(log, "skills: workspace provisioned with %v (.agents/skills + .claude/skills)\n", done)
	}
}
