package verify

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func programPathInside(root, path string) bool {
	rel, e := filepath.Rel(root, path)
	return e == nil && (rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
func newProgramRuntimeRoot(sources ...string) (string, error) {
	temp, e := filepath.EvalSymlinks(os.TempDir())
	if e != nil {
		return "", e
	}
	temp, e = filepath.Abs(temp)
	if e != nil {
		return "", e
	}
	for _, source := range sources {
		root, e := filepath.EvalSymlinks(source)
		if e != nil {
			return "", e
		}
		root, e = filepath.Abs(root)
		if e != nil {
			return "", e
		}
		if programPathInside(root, temp) {
			return "", errors.New("program runtime temp root is inside source; configure TMPDIR (TEMP/TMP on Windows) outside source and retry")
		}
	}
	return os.MkdirTemp(temp, "shape-program-runtime-")
}
func programRuntimeEnvironment(root, goos string) (map[string]string, error) {
	home, tmp := filepath.Join(root, "home"), filepath.Join(root, "tmp")
	cache, config, data := filepath.Join(root, "cache"), filepath.Join(root, "config"), filepath.Join(root, "data")
	goPath, goCache := filepath.Join(root, "go"), filepath.Join(cache, "go-build")
	modCache := filepath.Join(goPath, "pkg", "mod")
	roaming, local := filepath.Join(home, "AppData", "Roaming"), filepath.Join(home, "AppData", "Local")
	for _, dir := range []string{home, tmp, cache, config, data, goPath, goCache, modCache, roaming, local} {
		if e := os.MkdirAll(dir, 0700); e != nil {
			return nil, e
		}
	}
	env := map[string]string{"HOME": home, "TMPDIR": tmp, "TEMP": tmp, "TMP": tmp, "GOPATH": goPath, "GOCACHE": goCache, "GOMODCACHE": modCache, "XDG_CACHE_HOME": cache, "XDG_CONFIG_HOME": config, "XDG_DATA_HOME": data, "BASHY_HOME": filepath.Join(home, ".bashy")}
	if goos == "windows" {
		env["USERPROFILE"] = home
		env["APPDATA"] = roaming
		env["LOCALAPPDATA"] = local
	}
	return env, nil
}

// Go's module cache can contain read-only directories. Only this verifier-owned
// tree is made writable for cleanup; symlinks are removed, never followed.
func removeProgramRuntimeRoot(root string) error {
	if e := filepath.WalkDir(root, func(path string, entry fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if entry.IsDir() {
			return os.Chmod(path, 0700)
		}
		if entry.Type().IsRegular() {
			return os.Chmod(path, 0600)
		}
		return nil
	}); e != nil {
		return e
	}
	return os.RemoveAll(root)
}
