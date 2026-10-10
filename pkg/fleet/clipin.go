package fleet

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// KnownToolHelpers maps a tool's canonical name to helper executables that ship
// beside its primary binary and must be present for full functionality
// (for example, codex requires codex-code-mode-host).
var KnownToolHelpers = map[string][]string{
	"codex": {"codex-code-mode-host"},
}

// RegisterKnownHelper registers an additional helper executable for tool.
func RegisterKnownHelper(tool, helper string) {
	for _, h := range KnownToolHelpers[tool] {
		if h == helper {
			return
		}
	}
	KnownToolHelpers[tool] = append(KnownToolHelpers[tool], helper)
}

// DefaultToolsDir returns the root directory where CLI pins are installed
// (~/.bashy/tools or $BASHY_TOOLS_CACHE or $BASHY_HOME/tools).
func DefaultToolsDir() string {
	if d := os.Getenv("BASHY_TOOLS_CACHE"); d != "" {
		return d
	}
	if d := os.Getenv("BASHY_HOME"); d != "" {
		return filepath.Join(d, "tools")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".bashy", "tools")
	}
	return filepath.Join(home, ".bashy", "tools")
}

// ToolsDir returns the catalog's tools cache directory. If an explicit root was
// set (such as in tests), <root>/tools is used; otherwise DefaultToolsDir().
func (c *Catalog) ToolsDir() string {
	if c != nil && c.cfg.rootSet {
		return filepath.Join(c.cfg.root, "tools")
	}
	return DefaultToolsDir()
}

// CopyToolPin copies sourceBinary into destDir (as destBinary), and copies any
// known helper executables that exist in sourceBinary's directory (or its symlink
// target's directory) beside destBinary. It returns the destination binary path
// and the list of helper basenames that were copied.
func CopyToolPin(toolName, sourceBinary, destDir string) (string, []string, error) {
	sourceInfo, err := os.Stat(sourceBinary)
	if err != nil {
		return "", nil, fmt.Errorf("fleet: source binary %q: %w", sourceBinary, err)
	}
	if sourceInfo.IsDir() {
		return "", nil, fmt.Errorf("fleet: source binary %q is a directory", sourceBinary)
	}

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", nil, fmt.Errorf("fleet: create pin directory %q: %w", destDir, err)
	}

	binName := filepath.Base(sourceBinary)
	destBinary := filepath.Join(destDir, binName)
	mode := sourceInfo.Mode().Perm()
	if mode == 0 {
		mode = 0o755
	}
	if err := copyFile(sourceBinary, destBinary, mode); err != nil {
		return "", nil, fmt.Errorf("fleet: copy binary to %q: %w", destBinary, err)
	}

	var copied []string
	helpers := KnownToolHelpers[toolName]
	if len(helpers) > 0 {
		candidateDirs := []string{filepath.Dir(sourceBinary)}
		if realSource, err := filepath.EvalSymlinks(sourceBinary); err == nil {
			realDir := filepath.Dir(realSource)
			if realDir != candidateDirs[0] {
				candidateDirs = append(candidateDirs, realDir)
			}
		}

		for _, h := range helpers {
			srcHelper := findHelperInDirs(h, candidateDirs)
			if srcHelper == "" {
				continue
			}
			hInfo, err := os.Stat(srcHelper)
			if err != nil {
				continue
			}
			hMode := hInfo.Mode().Perm()
			if hMode == 0 {
				hMode = 0o755
			}
			destHelper := filepath.Join(destDir, filepath.Base(srcHelper))
			if err := copyFile(srcHelper, destHelper, hMode); err != nil {
				return "", nil, fmt.Errorf("fleet: copy helper %s to %q: %w", h, destHelper, err)
			}
			copied = append(copied, filepath.Base(srcHelper))
		}
	}

	return destBinary, copied, nil
}

func findHelperInDirs(helper string, dirs []string) string {
	names := []string{helper}
	if !strings.HasSuffix(strings.ToLower(helper), ".exe") {
		names = append(names, helper+".exe")
	}
	for _, dir := range dirs {
		for _, name := range names {
			p := filepath.Join(dir, name)
			if fileExists(p) {
				return p
			}
		}
	}
	return ""
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

// InstallToolPin installs a pinned copy of toolName into toolsDir (defaulting
// to c.ToolsDir()), copying any known helpers beside it, and updates the local
// catalog definition pointing cli.binary at the pinned copy.
func (c *Catalog) InstallToolPin(toolName, sourceBinary, version, toolsDir string) (string, []string, error) {
	t, ok := c.Tool(toolName)
	if !ok {
		return "", nil, fmt.Errorf("fleet: no tool %q", toolName)
	}

	if sourceBinary == "" {
		if isPathLike(t.CLI.Binary) && fileExists(t.CLI.Binary) {
			sourceBinary = t.CLI.Binary
		} else {
			binName := t.managedBinaryName()
			if p, err := exec.LookPath(binName); err == nil {
				sourceBinary = p
			} else if p, err := exec.LookPath(t.Name); err == nil {
				sourceBinary = p
			} else {
				return "", nil, fmt.Errorf("fleet: cannot find source binary for tool %q on PATH; specify --source", toolName)
			}
		}
	}

	if version == "" {
		if len(t.CLI.Versions) > 0 && t.CLI.Versions[0].Version != "" {
			version = t.CLI.Versions[0].Version
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			out, err := exec.CommandContext(ctx, sourceBinary, "--version").Output()
			cancel()
			if err == nil {
				words := strings.Fields(string(out))
				for _, w := range words {
					clean := strings.Trim(w, "v,()[]")
					if clean != "" && (clean[0] >= '0' && clean[0] <= '9') {
						version = clean
						break
					}
				}
			}
			if version == "" {
				version = "pinned"
			}
		}
	}

	if toolsDir == "" {
		toolsDir = c.ToolsDir()
	}

	destDir := filepath.Join(toolsDir, t.Name, version)
	destBinary, copied, err := CopyToolPin(t.Name, sourceBinary, destDir)
	if err != nil {
		return "", nil, err
	}

	t.CLI.Binary = destBinary
	instCmd := fmt.Sprintf("cp %s %s/", sourceBinary, destDir)
	if len(copied) > 0 {
		var all []string
		all = append(all, sourceBinary)
		srcDir := filepath.Dir(sourceBinary)
		if realSrc, err := filepath.EvalSymlinks(sourceBinary); err == nil {
			srcDir = filepath.Dir(realSrc)
		}
		for _, h := range copied {
			all = append(all, filepath.Join(srcDir, h))
		}
		instCmd = fmt.Sprintf("cp %s %s/", strings.Join(all, " "), destDir)
	}

	foundVer := false
	for i, v := range t.CLI.Versions {
		if v.Version == version {
			t.CLI.Versions[i].Install = instCmd
			foundVer = true
			break
		}
	}
	if !foundVer {
		t.CLI.Versions = append([]ToolVersion{{
			Version: version,
			Install: instCmd,
		}}, t.CLI.Versions...)
	}

	if err := c.SaveTool(t); err != nil {
		return "", nil, err
	}

	return destBinary, copied, nil
}

// ToolMissingHelpers returns any known helper executables that are missing from
// a pinned tool's directory, along with a fix command.
// If the tool is not a pinned binary or no known helpers are missing, it returns nil, "".
func ToolMissingHelpers(t Tool) (missing []string, fixCmd string) {
	bin := strings.TrimSpace(t.CLI.Binary)
	if bin == "" || !isPathLike(bin) {
		return nil, ""
	}

	helpers, ok := KnownToolHelpers[t.Name]
	if !ok || len(helpers) == 0 {
		return nil, ""
	}

	binDir := filepath.Dir(bin)
	for _, h := range helpers {
		target := filepath.Join(binDir, h)
		if !fileExists(target) && !fileExists(target+".exe") {
			missing = append(missing, h)
		}
	}
	if len(missing) == 0 {
		return nil, ""
	}

	var fixParts []string
	for _, h := range missing {
		src := findHostHelper(t, h)
		if src != "" {
			fixParts = append(fixParts, fmt.Sprintf("cp %s %s/", src, binDir))
		} else {
			fixParts = append(fixParts, fmt.Sprintf("cp <source-dir>/%s %s/", h, binDir))
		}
	}
	fixCmd = strings.Join(fixParts, " && ")
	return missing, fixCmd
}

func findHostHelper(t Tool, helper string) string {
	names := []string{helper}
	if !strings.HasSuffix(strings.ToLower(helper), ".exe") {
		names = append(names, helper+".exe")
	}

	// 1. Check if unpinned binary on PATH has the helper in its directory
	for _, binName := range []string{t.managedBinaryName(), t.Name} {
		if p, err := exec.LookPath(binName); err == nil {
			dirs := []string{filepath.Dir(p)}
			if real, err := filepath.EvalSymlinks(p); err == nil {
				dirs = append(dirs, filepath.Dir(real))
			}
			for _, d := range dirs {
				for _, n := range names {
					candidate := filepath.Join(d, n)
					if fileExists(candidate) {
						return candidate
					}
				}
			}
		}
	}

	// 2. Check if helper is on PATH directly
	for _, n := range names {
		if p, err := exec.LookPath(n); err == nil {
			return p
		}
	}

	// 3. Check tool's cli.versions install command for hints
	for _, v := range t.CLI.Versions {
		fields := strings.Fields(v.Install)
		for _, f := range fields {
			if strings.Contains(f, helper) && fileExists(f) {
				return f
			}
			if fileExists(filepath.Join(f, helper)) {
				return filepath.Join(f, helper)
			}
		}
	}

	// 4. Common macOS Homebrew Caskroom paths for codex
	if t.Name == "codex" {
		pattern := "/opt/homebrew/Caskroom/codex/*/bin/" + helper
		if matches, _ := filepath.Glob(pattern); len(matches) > 0 && fileExists(matches[len(matches)-1]) {
			return matches[len(matches)-1]
		}
		pattern2 := "/usr/local/Caskroom/codex/*/bin/" + helper
		if matches, _ := filepath.Glob(pattern2); len(matches) > 0 && fileExists(matches[len(matches)-1]) {
			return matches[len(matches)-1]
		}
	}

	return ""
}

func checkToolHelpersWarning(cmd *cobra.Command, t Tool) {
	missing, fixCmd := ToolMissingHelpers(t)
	if len(missing) == 0 {
		return
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "warning: pinned binary %s is missing known helper %s; fix: %s\n",
		t.CLI.Binary, strings.Join(missing, ", "), fixCmd)
}

func newToolInstall(opts []Option) *cobra.Command {
	var source string
	var version string
	var toolsDir string
	c := &cobra.Command{
		Use:     "install <name>",
		Aliases: []string{"pin"},
		Short:   "Install a pinned CLI version and its helper executables",
		Long: "Install a pinned CLI version and its helper executables.\n\n" +
			"Copies the executable into ~/.bashy/tools/<tool>/<version>/ (or --tools-dir),\n" +
			"copies any known helper executables (such as codex-code-mode-host) beside it,\n" +
			"and points cli.binary at the pinned copy.",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cat := New(opts...)
			name := args[0]
			dest, copied, err := cat.InstallToolPin(name, source, version, toolsDir)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "installed %s -> %s\n", name, dest)
			if len(copied) > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "copied helpers: %s\n", strings.Join(copied, ", "))
			}
			return nil
		},
	}
	c.Flags().StringVar(&source, "source", "", "source executable to copy (default: resolved on PATH)")
	c.Flags().StringVar(&version, "version", "", "pinned version (default: from tool definition or binary)")
	c.Flags().StringVar(&toolsDir, "tools-dir", "", "target tools cache directory (default: ~/.bashy/tools)")
	return c
}
