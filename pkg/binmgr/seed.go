package binmgr

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

type seedEntry struct {
	Path     string `json:"path"`
	Name     string `json:"name"`
	Version  string `json:"version"`
	SHA256   string `json:"sha256"`
	Size     int64  `json:"size"`
	Mode     int64  `json:"mode"`
	Modified int64  `json:"modified,omitempty"`
	// Link is a relative symlink target that stays inside the cache. Its
	// SHA256/Size cover the target string. Tree tools (podman-static, node,
	// python) and the darwin podman helper dir contain such links.
	Link string `json:"link,omitempty"`
}
type seedManifest struct {
	Schema   int         `json:"schema"`
	Platform string      `json:"platform"`
	Entries  []seedEntry `json:"entries"`
}

func safeSeedPath(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "\\:\x00") && !strings.HasPrefix(name, "/") && !strings.HasPrefix(name, "../") && path.Clean(name) == name
}
func linkDigest(target string) string {
	sum := sha256.Sum256([]byte(target))
	return hex.EncodeToString(sum[:])
}

// seedLinkTarget validates a slash-separated symlink target for the entry at
// rel: it must be relative and resolve lexically inside the cache root.
func seedLinkTarget(rel, target string) bool {
	if target == "" || strings.ContainsAny(target, "\\:\x00") || strings.HasPrefix(target, "/") {
		return false
	}
	resolved := path.Join(path.Dir(rel), target)
	return safeSeedPath(resolved)
}

// exportLinkTarget returns the portable target of the cache symlink file, or
// an error when it points outside root. Absolute in-cache targets (the darwin
// helper links) are rewritten relative so the seed works at any cache path.
func exportLinkTarget(file, rel string) (string, error) {
	target, err := os.Readlink(file)
	if err != nil {
		return "", err
	}
	if filepath.IsAbs(target) {
		r, err := filepath.Rel(filepath.Dir(file), target)
		if err != nil {
			return "", fmt.Errorf("binmgr: seed cannot include symlink %s -> %s outside the cache", file, target)
		}
		target = r
	}
	target = filepath.ToSlash(target)
	if !seedLinkTarget(rel, target) {
		return "", fmt.Errorf("binmgr: seed cannot include symlink %s -> %s outside the cache", file, target)
	}
	return target, nil
}

// resolvedInside requires the symlink file to resolve, through its whole link
// chain, to an existing path strictly inside base (already symlink-free).
// Dangling, cyclic and escaping links are refused: lexical checks alone miss
// chains through other links and directory links followed by "..".
func resolvedInside(base, file string) error {
	real, err := filepath.EvalSymlinks(file)
	if err == nil {
		var rel string
		if rel, err = filepath.Rel(base, real); err == nil && (rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel)) {
			err = fmt.Errorf("resolves to %s", real)
		}
	}
	if err != nil {
		return fmt.Errorf("binmgr: seed symlink %s does not resolve inside the cache: %v", file, err)
	}
	return nil
}

func seedDigest(file string) (string, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ExportSeed writes a portable, checksummed tar of the current platform's cache.
// Symlinks inside the cache are carried as relative links; symlinks leaving it
// and special files are refused rather than exporting content outside the
// selected cache. No network or installed-tool execution is involved.
func ExportSeed(ctx context.Context, archivePath string) error {
	root, err := CacheDir()
	if err != nil {
		return err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return err
	}
	target, err := filepath.Abs(archivePath)
	if err != nil {
		return err
	}
	if target == root || strings.HasPrefix(target, root+string(filepath.Separator)) {
		return fmt.Errorf("binmgr: seed output must be outside the cache")
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	manifest := seedManifest{Schema: 1, Platform: Platform()}
	err = filepath.WalkDir(root, func(file string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if file == root {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".seed-") || strings.HasPrefix(d.Name(), ".dl-") || strings.HasPrefix(d.Name(), ".resolution-") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		st, err := d.Info()
		if err != nil {
			return err
		}
		isLink := st.Mode()&fs.ModeSymlink != 0
		if !st.Mode().IsRegular() && !isLink {
			return fmt.Errorf("binmgr: seed cannot include special file %s", file)
		}
		rel, err := filepath.Rel(root, file)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !safeSeedPath(rel) {
			return fmt.Errorf("binmgr: invalid cache path %q", rel)
		}
		parts := strings.Split(rel, "/")
		name, version := parts[0], "managed"
		if len(parts) >= 3 {
			version = parts[1]
		}
		if isLink {
			target, err := exportLinkTarget(file, rel)
			if err != nil {
				return err
			}
			if err = resolvedInside(realRoot, file); err != nil {
				return err
			}
			manifest.Entries = append(manifest.Entries, seedEntry{Path: rel, Name: name, Version: version, SHA256: linkDigest(target), Size: int64(len(target)), Mode: 0777, Link: target})
			return nil
		}
		digest, err := seedDigest(file)
		if err != nil {
			return err
		}
		manifest.Entries = append(manifest.Entries, seedEntry{rel, name, version, digest, st.Size(), int64(st.Mode().Perm()), st.ModTime().UnixNano(), ""})
		return nil
	})
	if err != nil {
		return err
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(target), ".seed-export-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	tw := tar.NewWriter(f)
	if err = tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0600, Size: int64(len(data))}); err != nil {
		return err
	}
	if _, err = tw.Write(data); err != nil {
		return err
	}
	for _, entry := range manifest.Entries {
		if err = ctx.Err(); err != nil {
			return err
		}
		if entry.Link != "" {
			if err = tw.WriteHeader(&tar.Header{Name: "cache/" + entry.Path, Typeflag: tar.TypeSymlink, Linkname: entry.Link, Mode: entry.Mode}); err != nil {
				return err
			}
			continue
		}
		if err = tw.WriteHeader(&tar.Header{Name: "cache/" + entry.Path, Mode: entry.Mode, Size: entry.Size}); err != nil {
			return err
		}
		in, e := os.Open(filepath.Join(root, filepath.FromSlash(entry.Path)))
		if e != nil {
			return e
		}
		h := sha256.New()
		_, e = io.Copy(io.MultiWriter(tw, h), in)
		in.Close()
		if e != nil {
			return e
		}
		if hex.EncodeToString(h.Sum(nil)) != entry.SHA256 {
			return fmt.Errorf("binmgr: cache changed during seed export: %s", entry.Path)
		}
	}
	if err = tw.Close(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), target)
}

// ImportSeed verifies the entire archive before touching any cached entry.
// Invalid hashes, traversal, duplicates, foreign-platform seeds and links fail
// closed. A failed commit restores previously replaced entries.
func ImportSeed(ctx context.Context, archivePath string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	hdr, err := tr.Next()
	if err != nil {
		return err
	}
	if hdr.Name != "manifest.json" || hdr.Typeflag != tar.TypeReg || hdr.Size > 8<<20 {
		return fmt.Errorf("binmgr: seed must begin with a regular manifest.json (max 8 MiB)")
	}
	var manifest seedManifest
	if err = json.NewDecoder(io.LimitReader(tr, 8<<20)).Decode(&manifest); err != nil {
		return err
	}
	if manifest.Schema != 1 || manifest.Platform != Platform() {
		return fmt.Errorf("binmgr: seed schema/platform mismatch: %d %s (want 1 %s)", manifest.Schema, manifest.Platform, Platform())
	}
	entries := make(map[string]seedEntry, len(manifest.Entries))
	for _, entry := range manifest.Entries {
		raw, e := hex.DecodeString(entry.SHA256)
		if !safeSeedPath(entry.Path) || e != nil || len(raw) != 32 || entry.Size < 0 || entry.Name == "" || entry.Version == "" {
			return fmt.Errorf("binmgr: invalid seed entry %q", entry.Path)
		}
		if entry.Link != "" && (!seedLinkTarget(entry.Path, entry.Link) || entry.Size != int64(len(entry.Link)) || entry.SHA256 != linkDigest(entry.Link)) {
			return fmt.Errorf("binmgr: invalid seed link %q -> %q", entry.Path, entry.Link)
		}
		if _, ok := entries[entry.Path]; ok {
			return fmt.Errorf("binmgr: duplicate seed entry %q", entry.Path)
		}
		entries[entry.Path] = entry
	}
	// No entry may be written through a link the seed itself declares.
	for name := range entries {
		for dir := path.Dir(name); dir != "."; dir = path.Dir(dir) {
			if parent, ok := entries[dir]; ok && parent.Link != "" {
				return fmt.Errorf("binmgr: seed entry %q traverses seed link %q", name, dir)
			}
		}
	}
	root, err := CacheDir()
	if err != nil {
		return err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(root, 0755); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(root, ".seed-import-*")
	if err != nil {
		return err
	}
	preserveStage := false
	defer func() {
		if !preserveStage {
			_ = os.RemoveAll(stage)
		}
	}()
	seen := make(map[string]bool, len(entries))
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		hdr, err = tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(hdr.Name, "cache/")
		entry, ok := entries[name]
		if !strings.HasPrefix(hdr.Name, "cache/") || !ok || seen[name] {
			return fmt.Errorf("binmgr: unexpected seed member %q", hdr.Name)
		}
		dest := filepath.Join(stage, "new", filepath.FromSlash(name))
		if entry.Link != "" {
			if hdr.Typeflag != tar.TypeSymlink || hdr.Linkname != entry.Link {
				return fmt.Errorf("binmgr: unexpected seed member %q", hdr.Name)
			}
			if err = os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
				return err
			}
			if err = os.Symlink(filepath.FromSlash(entry.Link), dest); err != nil {
				return err
			}
			seen[name] = true
			continue
		}
		if hdr.Typeflag != tar.TypeReg || hdr.Size != entry.Size {
			return fmt.Errorf("binmgr: unexpected seed member %q", hdr.Name)
		}
		if err = os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			return err
		}
		out, e := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(entry.Mode)&0755|0600)
		if e != nil {
			return e
		}
		h := sha256.New()
		_, e = io.Copy(io.MultiWriter(out, h), tr)
		closeErr := out.Close()
		if e != nil {
			return e
		}
		if closeErr != nil {
			return closeErr
		}
		if hex.EncodeToString(h.Sum(nil)) != entry.SHA256 {
			return fmt.Errorf("binmgr: seed digest mismatch for %s", name)
		}
		if entry.Modified > 0 {
			modified := time.Unix(0, entry.Modified)
			if err = os.Chtimes(dest, modified, modified); err != nil {
				return err
			}
		}
		seen[name] = true
	}
	if len(seen) != len(entries) {
		return fmt.Errorf("binmgr: seed missing declared files")
	}
	// Resolve every link against the staged seed alone, never the existing
	// cache: the seed must carry each link's final target.
	if err = os.MkdirAll(filepath.Join(stage, "new"), 0755); err != nil {
		return err
	}
	staged, err := filepath.EvalSymlinks(filepath.Join(stage, "new"))
	if err != nil {
		return err
	}
	for name, entry := range entries {
		if entry.Link == "" {
			continue
		}
		if err = resolvedInside(staged, filepath.Join(staged, filepath.FromSlash(name))); err != nil {
			return err
		}
	}
	// Refuse symlink parents in the existing cache before moving any file. A
	// link entry may replace an existing link at its own path.
	for name, entry := range entries {
		cur := root
		parts := strings.Split(name, "/")
		for i, part := range parts {
			cur = filepath.Join(cur, part)
			if entry.Link != "" && i == len(parts)-1 {
				break
			}
			info, e := os.Lstat(cur)
			if e != nil && !os.IsNotExist(e) {
				return e
			}
			if e == nil && info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("binmgr: seed target traverses symlink %s", cur)
			}
		}
	}
	type changed struct{ dest, old string }
	var applied []changed
	rollback := func(cause error) error {
		for i := len(applied) - 1; i >= 0; i-- {
			v := applied[i]
			if e := os.Remove(v.dest); e != nil && !os.IsNotExist(e) {
				preserveStage = true
				cause = fmt.Errorf("%w; rollback remove: %v; recovery files in %s", cause, e, stage)
			}
			if v.old != "" {
				if e := os.Rename(v.old, v.dest); e != nil {
					preserveStage = true
					cause = fmt.Errorf("%w; rollback restore: %v; recovery files in %s", cause, e, stage)
				}
			}
		}
		return cause
	}
	for _, entry := range manifest.Entries {
		if err = ctx.Err(); err != nil {
			return rollback(err)
		}
		dest := filepath.Join(root, filepath.FromSlash(entry.Path))
		if err = os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			return rollback(err)
		}
		old := ""
		if info, e := os.Lstat(dest); e == nil {
			if !info.Mode().IsRegular() && (entry.Link == "" || info.Mode()&os.ModeSymlink == 0) {
				return rollback(fmt.Errorf("binmgr: nonregular seed target %s", dest))
			}
			old = filepath.Join(stage, fmt.Sprintf("previous-%d", len(applied)))
			if err = os.Rename(dest, old); err != nil {
				return rollback(err)
			}
		} else if !os.IsNotExist(e) {
			return rollback(e)
		}
		applied = append(applied, changed{dest, old})
		if err = os.Rename(filepath.Join(stage, "new", filepath.FromSlash(entry.Path)), dest); err != nil {
			return rollback(err)
		}
	}
	return nil
}
