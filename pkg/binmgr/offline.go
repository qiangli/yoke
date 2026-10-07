package binmgr

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Offline forbids managed-tool network acquisition, not local execution.
func Offline() bool { return strings.TrimSpace(os.Getenv("BASHY_OFFLINE")) == "1" }

func offlineMissing(name, version string) error {
	if version == "" {
		version = "latest"
	}
	return fmt.Errorf("binmgr: %s %s is not cached; BASHY_OFFLINE=1 forbids downloading it (install a matching seed cache)", name, version)
}

type cachedResolution struct {
	Repo     string `json:"repo"`
	Platform string `json:"platform"`
	Tool     Tool   `json:"tool"`
}

func cacheComponent(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, "/\\:")
}

func rememberGitHubTool(spec GitHubSpec, tool Tool) {
	if !cacheComponent(tool.Name) || !cacheComponent(tool.Version) {
		return
	}
	root, err := CacheDir()
	if err != nil {
		return
	}
	dir := filepath.Join(root, tool.Name, tool.Version)
	if os.MkdirAll(dir, 0755) != nil {
		return
	}
	b, err := json.Marshal(cachedResolution{spec.Repo, Platform(), tool})
	if err != nil {
		return
	}
	f, err := os.CreateTemp(dir, ".resolution-*")
	if err != nil {
		return
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err != nil {
		f.Close()
		return
	}
	if f.Close() != nil {
		return
	}
	// Failure to persist is harmless: the next lookup may resolve online again.
	_ = os.Rename(f.Name(), filepath.Join(dir, ".resolution.json"))
}

func cachedGitHubTool(spec GitHubSpec) (Tool, bool) {
	if !cacheComponent(spec.Name) {
		return Tool{}, false
	}
	root, err := CacheDir()
	if err != nil {
		return Tool{}, false
	}
	latest := spec.Version == "" || spec.Version == "latest"
	var dirs []string
	if latest {
		dirs, _ = filepath.Glob(filepath.Join(root, spec.Name, "*"))
		if spec.RequireArchive {
			archiveDirs, _ := filepath.Glob(filepath.Join(root, spec.Name+"-archive", "*"))
			dirs = append(dirs, archiveDirs...)
		}
	} else if cacheComponent(spec.Version) {
		dirs = []string{filepath.Join(root, spec.Name, spec.Version)}
		if spec.RequireArchive {
			dirs = append(dirs, filepath.Join(root, spec.Name+"-archive", spec.Version))
		}
	} else {
		return Tool{}, false
	}
	var best Tool
	var bestTime int64
	consider := func(path string, tool Tool) {
		if !isExecutable(path) {
			return
		}
		st, err := os.Stat(path)
		if err != nil {
			return
		}
		if best.cachedPath == "" || st.ModTime().UnixNano() > bestTime {
			best, bestTime = tool, st.ModTime().UnixNano()
			best.cachedName, best.cachedPath = spec.Name, path
		}
	}
	seen := make(map[string]bool, len(dirs))
	for _, dir := range dirs {
		version := filepath.Base(dir)
		if seen[version] {
			continue
		}
		seen[version] = true
		path := filepath.Join(dir, binaryName(spec.Name))
		if spec.Tree {
			if !safeSeedPath(spec.Entrypoint) {
				continue
			}
			path = filepath.Join(dir, filepath.FromSlash(spec.Entrypoint))
		}
		tool := Tool{Name: spec.Name, Version: version, Assets: map[string]Asset{Platform(): {URL: "cache-only", Tree: spec.Tree, Entrypoint: spec.Entrypoint}}}
		data, err := os.ReadFile(filepath.Join(dir, ".resolution.json"))
		if err != nil && spec.RequireArchive {
			data, err = os.ReadFile(filepath.Join(root, spec.Name, version, ".resolution.json"))
		}
		if err == nil {
			var record cachedResolution
			if json.Unmarshal(data, &record) != nil || record.Repo != spec.Repo || record.Platform != Platform() || record.Tool.Name != spec.Name || record.Tool.Version != version {
				continue
			}
			asset, exists := record.Tool.Assets[Platform()]
			if !exists || asset.Tree != spec.Tree || asset.Entrypoint != spec.Entrypoint || asset.Binary != spec.Member {
				continue
			}
			tool = record.Tool
		} else if spec.RequireArchive {
			continue
		}
		if spec.RequireArchive {
			archivePath := filepath.Join(root, spec.Name+"-archive", version, binaryName(spec.Name+"-archive"))
			if isExecutable(archivePath) {
				consider(archivePath, tool)
				if best.cachedPath == archivePath {
					best.cachedName = spec.Name + "-archive"
				}
			}
			continue
		}
		consider(path, tool)
	}
	if latest && !spec.RequireArchive {
		if cached := CachedBinary(spec.Name); cached != "" {
			consider(cached, Tool{Name: spec.Name, Version: "cached", Assets: map[string]Asset{Platform(): {URL: "cache-only"}}})
		} else {
			consider(filepath.Join(root, binaryName(spec.Name)), Tool{Name: spec.Name, Version: "cached", Assets: map[string]Asset{Platform(): {URL: "cache-only"}}})
		}
	}
	return best, best.cachedPath != ""
}
