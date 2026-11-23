package gomod

// The local module proxy: a directory in GOPROXY layout
// (<module>/@v/{list,V.info,V.mod,V.zip}) that `mod proxy publish` writes
// from a module directory and every local build reads with
// GOPROXY=file://DIR. No daemon: the go command serves itself from the tree.
// Zips and hashes come from golang.org/x/mod, so a published version is the
// same bytes the go command would produce from the same files.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
	"golang.org/x/mod/sumdb/dirhash"
	modzip "golang.org/x/mod/zip"

	"github.com/qiangli/coreutils/pkg/weavecli"

	coregit "github.com/qiangli/yoke/git"
)

// ProxyEnv names the local proxy directory; unset, it is
// $BASHY_HOME/gomod-proxy (BASHY_HOME defaulting to ~/.bashy).
const ProxyEnv = "BASHY_GOMOD_PROXY"

// LocalModulePrefix is the module path prefix published only to the local
// proxy: the checksum database has never seen these versions.
const LocalModulePrefix = "github.com/qiangli/bashy/examples"

// ProxyDir is the local proxy directory.
func ProxyDir() string {
	if d := strings.TrimSpace(os.Getenv(ProxyEnv)); d != "" {
		return d
	}
	home := strings.TrimSpace(os.Getenv("BASHY_HOME"))
	if home == "" {
		if u, err := os.UserHomeDir(); err == nil && u != "" {
			home = filepath.Join(u, ".bashy")
		} else {
			home = ".bashy"
		}
	}
	return filepath.Join(home, "gomod-proxy")
}

// FileURL is dir as a GOPROXY file:// entry (file:///C:/x on Windows).
func FileURL(dir string) string {
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	p := filepath.ToSlash(dir)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return "file://" + p
}

// BuildEnv is the environment a local build of a published-module consumer
// needs: the local proxy first, then the inherited GOPROXY (default
// https://proxy.golang.org,direct), and the local paths exempt from the
// checksum database. getenv reads the inherited values (os.Getenv when nil);
// an inherited GONOSUMDB is extended, not replaced. GOFLAGS is left alone:
// a consumer's go.sum is complete (-mod=readonly holds), and -mod=mod would
// make a vendored tree ignore its vendor directory.
func BuildEnv(proxyDir string, getenv func(string) string) []string {
	if getenv == nil {
		getenv = os.Getenv
	}
	upstream := strings.TrimSpace(getenv("GOPROXY"))
	if upstream == "" {
		upstream = "https://proxy.golang.org,direct"
	}
	nosum := LocalModulePrefix
	if inherited := strings.TrimSpace(getenv("GONOSUMDB")); inherited != "" {
		nosum = inherited + "," + nosum
	}
	return []string{"GOPROXY=" + FileURL(proxyDir) + "," + upstream, "GONOSUMDB=" + nosum}
}

// Publication is the result of one publish.
type Publication struct {
	Module    string `json:"module"`
	Version   string `json:"version"`
	Commit    string `json:"commit,omitempty"` // the commit a pseudo-version names
	Hash      string `json:"hash"`             // go.sum h1: hash of the zip
	Proxy     string `json:"proxy"`
	Published bool   `json:"published"` // false: the identical version was already there
}

// Publish writes the module at dir into proxyDir as version. An empty
// version is the pseudo-version of the commit at HEAD, and the zip holds
// that commit's tree under dir: a pseudo-version names a commit, so its
// content is that commit's. An explicit version publishes the working copy
// of the files git tracks under dir (every file, outside a git repository).
// A published version is immutable: the identical zip again is a no-op,
// different content is refused.
func Publish(dir, version, proxyDir string) (*Publication, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	gomod, err := os.ReadFile(filepath.Join(abs, "go.mod"))
	if err != nil {
		return nil, &Error{Code: weavecli.ExitInvalidArg, Msg: dir + " is not a module directory: " + err.Error()}
	}
	modPath := modfile.ModulePath(gomod)
	if modPath == "" {
		return nil, &Error{Code: weavecli.ExitInvalidArg, Msg: dir + "/go.mod declares no module path"}
	}
	if err := module.CheckPath(modPath); err != nil {
		return nil, &Error{Code: weavecli.ExitInvalidArg, Msg: fmt.Sprintf("module path %q cannot be served by a proxy (%v); give it a domain-rooted path such as github.com/OWNER/REPO/SUBDIR", modPath, err)}
	}

	pub := &Publication{Module: modPath, Proxy: proxyDir}
	var files []modzip.File
	var when time.Time
	if version == "" {
		c, rel, err := headCommit(abs)
		if err != nil {
			return nil, &Error{Code: weavecli.ExitInvalidArg, Msg: "a pseudo-version needs a git commit (" + err.Error() + "); pass --version"}
		}
		when = c.Committer.When.UTC()
		_, pathMajor, _ := module.SplitPathVersion(modPath)
		version = module.PseudoVersion(module.PathMajorPrefix(pathMajor), "", when, c.Hash.String()[:12])
		pub.Commit = c.Hash.String()
		if gomod, files, err = commitFiles(c, rel); err != nil {
			return nil, err
		}
	} else {
		when = time.Now().UTC().Truncate(time.Second)
		if files, err = worktreeFiles(abs); err != nil {
			return nil, err
		}
	}
	if !semver.IsValid(version) || semver.Canonical(version) != version {
		return nil, &Error{Code: weavecli.ExitInvalidArg, Msg: fmt.Sprintf("version %q is not a canonical semantic version (vX.Y.Z)", version)}
	}
	m := module.Version{Path: modPath, Version: version}
	if err := module.Check(modPath, version); err != nil {
		return nil, &Error{Code: weavecli.ExitInvalidArg, Msg: err.Error()}
	}
	pub.Version = version

	var zbuf bytes.Buffer
	if files == nil {
		err = modzip.CreateFromDir(&zbuf, m, abs)
	} else {
		err = modzip.Create(&zbuf, m, files)
	}
	if err != nil {
		return nil, fmt.Errorf("zip %s: %w", m, err)
	}
	if pub.Hash, err = hashZipBytes(zbuf.Bytes()); err != nil {
		return nil, err
	}

	escPath, err := module.EscapePath(modPath)
	if err != nil {
		return nil, err
	}
	escVer, err := module.EscapeVersion(version)
	if err != nil {
		return nil, err
	}
	vdir := filepath.Join(proxyDir, filepath.FromSlash(escPath), "@v")
	zipFile := filepath.Join(vdir, escVer+".zip")
	if _, err := os.Stat(zipFile); err == nil {
		have, err := dirhash.HashZip(zipFile, dirhash.DefaultHash)
		if err != nil {
			return nil, err
		}
		if have != pub.Hash {
			return nil, &Error{Code: weavecli.ExitPrecondFail, Msg: fmt.Sprintf("%s is already published with different content (%s, now %s); a version is immutable: publish a new version", m, have, pub.Hash)}
		}
		return pub, nil
	}
	if err := os.MkdirAll(vdir, 0o755); err != nil {
		return nil, err
	}
	info, _ := json.Marshal(struct {
		Version string
		Time    string
	}{version, when.Format(time.RFC3339)})
	// The zip lands last: its presence is what makes the version published.
	for _, f := range []struct {
		name string
		data []byte
	}{{escVer + ".info", info}, {escVer + ".mod", gomod}, {escVer + ".zip", zbuf.Bytes()}} {
		if err := writeAtomic(filepath.Join(vdir, f.name), f.data); err != nil {
			return nil, err
		}
	}
	if err := addToList(filepath.Join(vdir, "list"), version); err != nil {
		return nil, err
	}
	pub.Published = true
	return pub, nil
}

// ProxyModule is one module in the proxy and its published versions.
type ProxyModule struct {
	Module   string   `json:"module"`
	Versions []string `json:"versions"`
}

// ProxyStatus lists every module in proxyDir with its versions in semver
// order. A missing directory is an empty proxy.
func ProxyStatus(proxyDir string) ([]ProxyModule, error) {
	var mods []ProxyModule
	err := filepath.WalkDir(proxyDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && p == proxyDir {
				return fs.SkipAll
			}
			return err
		}
		if !d.IsDir() || d.Name() != "@v" {
			return nil
		}
		rel, err := filepath.Rel(proxyDir, filepath.Dir(p))
		if err != nil {
			return err
		}
		modPath, err := module.UnescapePath(filepath.ToSlash(rel))
		if err != nil {
			return nil
		}
		data, err := os.ReadFile(filepath.Join(p, "list"))
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		vers := strings.Fields(string(data))
		semver.Sort(vers)
		mods = append(mods, ProxyModule{Module: modPath, Versions: vers})
		return fs.SkipDir
	})
	sort.Slice(mods, func(i, j int) bool { return mods[i].Module < mods[j].Module })
	return mods, err
}

// headCommit is HEAD of the repository holding dir and dir's path in it.
func headCommit(dir string) (*object.Commit, string, error) {
	repo, root, err := openWorktree(dir)
	if err != nil {
		return nil, "", err
	}
	ref, err := repo.Head()
	if err != nil {
		return nil, "", err
	}
	c, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return nil, "", err
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return nil, "", err
	}
	return c, filepath.ToSlash(rel), nil
}

func openWorktree(dir string) (*gogit.Repository, string, error) {
	repo, err := coregit.OpenRepository(dir)
	if err != nil {
		return nil, "", err
	}
	w, err := repo.Worktree()
	if err != nil {
		return nil, "", err
	}
	root := w.Filesystem.Root()
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real
	}
	return repo, root, nil
}

// commitFiles is the tree of c under rel, with its go.mod.
func commitFiles(c *object.Commit, rel string) ([]byte, []modzip.File, error) {
	tree, err := c.Tree()
	if err != nil {
		return nil, nil, err
	}
	if rel != "." {
		if tree, err = tree.Tree(rel); err != nil {
			return nil, nil, fmt.Errorf("%s at %s: %w", rel, c.Hash.String()[:12], err)
		}
	}
	var gomod []byte
	var files []modzip.File
	err = tree.Files().ForEach(func(f *object.File) error {
		if f.Mode != filemode.Regular && f.Mode != filemode.Executable && f.Mode != filemode.Deprecated {
			return nil
		}
		if f.Name == "go.mod" {
			r, err := f.Reader()
			if err != nil {
				return err
			}
			defer r.Close()
			if gomod, err = io.ReadAll(r); err != nil {
				return err
			}
		}
		files = append(files, treeFile{f})
		return nil
	})
	if err == nil && gomod == nil {
		err = fmt.Errorf("%s has no go.mod at %s", rel, c.Hash.String()[:12])
	}
	return gomod, files, err
}

// worktreeFiles is the working copy of the files git tracks under dir, or
// nil (every file) when dir is not in a git repository.
func worktreeFiles(dir string) ([]modzip.File, error) {
	repo, root, err := openWorktree(dir)
	if err != nil {
		return nil, nil
	}
	idx, err := repo.Storer.Index()
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return nil, err
	}
	prefix := filepath.ToSlash(rel) + "/"
	if rel == "." {
		prefix = ""
	}
	files := []modzip.File{}
	for _, e := range idx.Entries {
		if !strings.HasPrefix(e.Name, prefix) {
			continue
		}
		name := strings.TrimPrefix(e.Name, prefix)
		full := filepath.Join(dir, filepath.FromSlash(name))
		if _, err := os.Lstat(full); os.IsNotExist(err) {
			continue // deleted in the working copy
		}
		files = append(files, diskFile{path: name, file: full})
	}
	return files, nil
}

type treeFile struct{ f *object.File }

func (t treeFile) Path() string                 { return t.f.Name }
func (t treeFile) Lstat() (os.FileInfo, error)  { return blobInfo{t.f}, nil }
func (t treeFile) Open() (io.ReadCloser, error) { return t.f.Reader() }

type blobInfo struct{ f *object.File }

func (b blobInfo) Name() string       { return path.Base(b.f.Name) }
func (b blobInfo) Size() int64        { return b.f.Size }
func (b blobInfo) Mode() fs.FileMode  { return 0o644 }
func (b blobInfo) ModTime() time.Time { return time.Time{} }
func (b blobInfo) IsDir() bool        { return false }
func (b blobInfo) Sys() any           { return nil }

type diskFile struct{ path, file string }

func (d diskFile) Path() string                 { return d.path }
func (d diskFile) Lstat() (os.FileInfo, error)  { return os.Lstat(d.file) }
func (d diskFile) Open() (io.ReadCloser, error) { return os.Open(d.file) }

func hashZipBytes(b []byte) (string, error) {
	f, err := os.CreateTemp("", "gomod-zip-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	return dirhash.HashZip(f.Name(), dirhash.DefaultHash)
}

func writeAtomic(file string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(file), "."+filepath.Base(file)+"-*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(data)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o644)
	}
	if err == nil {
		err = os.Rename(tmp.Name(), file)
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}

func addToList(file, version string) error {
	data, err := os.ReadFile(file)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	vers := strings.Fields(string(data))
	for _, v := range vers {
		if v == version {
			return nil
		}
	}
	vers = append(vers, version)
	semver.Sort(vers)
	return writeAtomic(file, []byte(strings.Join(vers, "\n")+"\n"))
}
