// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package fleet

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/mod/modfile"

	ygit "github.com/qiangli/yoke/git"
	"github.com/qiangli/yoke/pkg/shape/appserver"
)

const (
	shapedBaseBranch = "pristine"
	shapedAuthorName = "bashy"
	shapedAuthorMail = "bashy@localhost"
)

// importShapedApp turns an operator-supplied directory or git URL into the
// managed checkout every shaping path consumes. The registry never points at
// the supplied path: moving or deleting it after registration cannot strand
// the app.
func importShapedApp(a *App) error {
	if !a.IsShaped() {
		return nil
	}
	if err := validName(a.Name); err != nil {
		return err
	}
	source := strings.TrimSpace(a.Source)
	if source == "" {
		return fmt.Errorf("fleet: app %q: a %s app needs --source", a.Name, AppTypeShaped)
	}
	dest := shapedManagedDir(a.Name)
	absDest, err := filepath.Abs(dest)
	if err != nil {
		return fmt.Errorf("fleet: app %q: managed source: %w", a.Name, err)
	}
	dest = absDest

	// A previous successful import is already the authority. Re-registering is
	// a no-op apart from revalidating the base/pristine invariants.
	if st, statErr := os.Stat(dest); statErr == nil && st.IsDir() {
		if err := ensureShapedBase(dest); err != nil {
			return fmt.Errorf("fleet: app %q: managed checkout: %w", a.Name, err)
		}
		a.Source = dest
		return nil
	} else if statErr != nil && !os.IsNotExist(statErr) {
		return fmt.Errorf("fleet: app %q: managed checkout: %w", a.Name, statErr)
	}

	root := filepath.Dir(dest)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("fleet: app %q: create managed source root: %w", a.Name, err)
	}
	tmp, err := os.MkdirTemp(root, "."+a.Name+"-import-*")
	if err != nil {
		return fmt.Errorf("fleet: app %q: create import workspace: %w", a.Name, err)
	}
	defer os.RemoveAll(tmp)

	if st, statErr := os.Stat(source); statErr == nil {
		if !st.IsDir() {
			return fmt.Errorf("fleet: app %q: source %s is not a directory", a.Name, source)
		}
		absSource, absErr := filepath.Abs(source)
		if absErr != nil {
			return fmt.Errorf("fleet: app %q: source: %w", a.Name, absErr)
		}
		if samePath(absSource, dest) {
			if err := ensureShapedBase(dest); err != nil {
				return fmt.Errorf("fleet: app %q: managed checkout: %w", a.Name, err)
			}
			a.Source = dest
			return nil
		}
		if err := copyShapedTree(absSource, tmp); err != nil {
			return fmt.Errorf("fleet: app %q: import %s: %w", a.Name, source, err)
		}
		if err := vendorOutOfTreeReplaces(absSource, tmp); err != nil {
			return fmt.Errorf("fleet: app %q: import %s: %w", a.Name, source, err)
		}
		if _, err := ygit.Init(ygit.InitOptions{Path: tmp}); err != nil {
			return fmt.Errorf("fleet: app %q: initialize managed checkout: %w", a.Name, err)
		}
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("fleet: app %q: source %s: %w", a.Name, source, statErr)
	} else {
		// A non-filesystem source is a git URL. Clone through go-git; there is
		// deliberately no host-git fallback in this path.
		_ = os.Remove(tmp)
		if _, err := ygit.Clone(ygit.CloneOptions{URL: source, Path: tmp}); err != nil {
			return fmt.Errorf("fleet: app %q: clone source: %w", a.Name, err)
		}
	}
	if err := checkShapedManifest(tmp); err != nil {
		return fmt.Errorf("fleet: app %q: %w", a.Name, err)
	}
	if err := ensureShapedBase(tmp); err != nil {
		return fmt.Errorf("fleet: app %q: prepare managed checkout: %w", a.Name, err)
	}
	if err := os.Rename(tmp, dest); err != nil {
		return fmt.Errorf("fleet: app %q: install managed checkout: %w", a.Name, err)
	}
	a.Source = dest
	return nil
}

// checkShapedManifest refuses an import whose server manifest (shaped.json)
// the console would refuse to serve, so the mistake surfaces at `app add`
// rather than as a 5xx on first use. No manifest is the common case.
func checkShapedManifest(dir string) error {
	b, err := os.ReadFile(filepath.Join(dir, appserver.File))
	if os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	_, err = appserver.Parse(b)
	return err
}

// vendorOutOfTreeReplaces makes a declared server buildable from its own
// tree. A revision is built from that commit alone, so a go.mod replace
// pointing outside the app (`replace desk-kit => ../kit`, the desk tools'
// shape) would break every build. When the source has such a replace and no
// vendor/ of its own, `go mod vendor` runs where the replace resolves and
// writes into the managed checkout: the outside module is captured into the
// base revision like any other source. An app without a server manifest, or
// whose go.mod stays inside the tree, is copied untouched.
func vendorOutOfTreeReplaces(source, dest string) error {
	b, err := os.ReadFile(filepath.Join(source, appserver.File))
	if err != nil {
		return nil
	}
	m, err := appserver.Parse(b)
	if err != nil || m.Server == nil {
		return err
	}
	modDir := ""
	for d := filepath.Join(source, filepath.FromSlash(m.Server.Main)); ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			modDir = d
			break
		}
		if samePath(d, source) || filepath.Dir(d) == d {
			return nil
		}
	}
	if _, err := os.Stat(filepath.Join(modDir, "vendor")); err == nil {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(modDir, "go.mod"))
	if err != nil {
		return err
	}
	mf, err := modfile.Parse("go.mod", data, nil)
	if err != nil {
		return fmt.Errorf("go.mod: %w", err)
	}
	outside := ""
	for _, r := range mf.Replace {
		if r.New.Version != "" {
			continue
		}
		target := r.New.Path
		if !filepath.IsAbs(target) {
			target = filepath.Join(modDir, target)
		}
		if rel, err := filepath.Rel(source, target); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			outside = r.New.Path
			break
		}
	}
	if outside == "" {
		return nil
	}
	rel, err := filepath.Rel(source, modDir)
	if err != nil {
		return err
	}
	cmd := exec.Command("go", "mod", "vendor", "-o", filepath.Join(dest, rel, "vendor"))
	cmd.Dir = modDir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("capture out-of-tree replace %s with go mod vendor: %v\n%s", outside, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func shapedManagedDir(name string) string {
	home := strings.TrimSpace(os.Getenv("BASHY_HOME"))
	if home == "" {
		if userHome, err := os.UserHomeDir(); err == nil && userHome != "" {
			home = filepath.Join(userHome, ".bashy")
		} else {
			home = ".bashy"
		}
	}
	return filepath.Join(home, "src", name)
}

func ensureShapedBase(dir string) error {
	if _, err := os.Stat(filepath.Join(dir, ".git")); os.IsNotExist(err) {
		if _, err := ygit.Init(ygit.InitOptions{Path: dir}); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if _, err := ygit.ResolveCommit(dir, "HEAD"); err != nil {
		if err := commitShapedBase(dir, "Import shaped app"); err != nil {
			return err
		}
	}
	if _, err := ygit.ResolveCommit(dir, shapedBaseBranch); err == nil {
		return nil
	}
	if _, _, err := ygit.Branch(ygit.BranchOptions{RepoPath: dir, Name: shapedBaseBranch}); err != nil {
		return err
	}
	return nil
}

func copyShapedTree(source, dest string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if rel == ".git" || strings.HasPrefix(rel, ".git"+string(filepath.Separator)) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dest, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case entry.IsDir():
			return os.MkdirAll(target, info.Mode().Perm())
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case info.Mode().IsRegular():
			return copyShapedFile(path, target, info.Mode().Perm())
		default:
			return fmt.Errorf("unsupported file type at %s", rel)
		}
	})
}

func copyShapedFile(source, dest string, mode fs.FileMode) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func samePath(a, b string) bool {
	if aa, err := filepath.EvalSymlinks(a); err == nil {
		a = aa
	}
	if bb, err := filepath.EvalSymlinks(b); err == nil {
		b = bb
	}
	return filepath.Clean(a) == filepath.Clean(b)
}
