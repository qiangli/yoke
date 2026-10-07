package binmgr

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// EnsureMembers downloads and verifies one release archive once, then returns
// its requested executable members together. The verified archive remains cached.
func EnsureMembers(ctx context.Context, tool Tool, members []string) (map[string]string, error) {
	if len(members) == 0 {
		return nil, fmt.Errorf("archive members are required")
	}
	names := append([]string(nil), members...)
	sort.Strings(names)
	for i, name := range names {
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\") || (i > 0 && name == names[i-1]) {
			return nil, fmt.Errorf("invalid archive member %q", name)
		}
	}
	archive, err := EnsureArchive(ctx, tool)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(filepath.Dir(archive), fmt.Sprintf("members-%x", sha256.Sum256([]byte(strings.Join(names, "\n")))))
	paths := make(map[string]string, len(members))
	complete := true
	for _, name := range members {
		paths[name] = filepath.Join(dir, name)
		complete = complete && isExecutable(paths[name])
	}
	if complete {
		return paths, nil
	}
	tmp, err := os.MkdirTemp(filepath.Dir(archive), ".members-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	if _, err := ExtractMembers(archive, tmp, members); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, dir); err != nil {
		// A concurrent publisher may have won. Never publish individual files:
		// readers see either one complete immutable set or no set at all.
		for _, name := range members {
			if !isExecutable(paths[name]) {
				return nil, err
			}
		}
	}

	return paths, nil
}

// EnsureArchive uses Ensure's checksum, cache and offline policy while retaining
// the downloaded archive instead of extracting just its default executable.
func EnsureArchive(ctx context.Context, tool Tool) (string, error) {
	assets := make(map[string]Asset, len(tool.Assets))
	for platform, asset := range tool.Assets {
		if pin, ok := pinnedSHA256(tool.Name, tool.Version, platform); ok {
			asset.SHA256 = pin
			asset.SHA512 = ""
			asset.MD5 = ""
		}
		asset.Binary = ""
		asset.Tree = false
		asset.Entrypoint = ""
		assets[platform] = asset
	}
	tool.Name += "-archive"
	tool.Assets = assets
	return Ensure(ctx, tool)
}

// ExtractMembers extracts only named regular root-level files from a tar.gz or
// zip archive. It rejects missing/duplicate members and links, and never uses
// archive paths as output paths. Caller owns the empty staging directory.
func ExtractMembers(archive, dir string, members []string) (map[string]string, error) {
	wanted := make(map[string]bool, len(members))
	paths := make(map[string]string, len(members))
	for _, name := range members {
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\") || wanted[name] {
			return nil, fmt.Errorf("invalid archive member %q", name)
		}
		wanted[name] = true
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	put := func(name string, regular bool, r io.Reader) error {
		name = strings.TrimPrefix(name, "./")
		if strings.HasPrefix(name, "/") || strings.Contains(name, "\\") {
			return fmt.Errorf("unsafe archive path %q", name)
		}
		for _, part := range strings.Split(name, "/") {
			if part == ".." {
				return fmt.Errorf("unsafe archive path %q", name)
			}
		}
		if !wanted[name] {
			return nil
		}
		if !regular || paths[name] != "" {
			return fmt.Errorf("archive member %q must be a unique regular file", name)
		}
		dst := filepath.Join(dir, name)
		f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0755)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, r)
		cerr := f.Close()
		if err != nil {
			return err
		}
		if cerr != nil {
			return cerr
		}
		paths[name] = dst
		return nil
	}
	f, err := os.Open(archive)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var magic [2]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil {
		return nil, err
	}
	if _, err := f.Seek(0, 0); err != nil {
		return nil, err
	}
	if magic == [2]byte{0x1f, 0x8b} {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		tr := tar.NewReader(gz)
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, err
			}
			if err := put(h.Name, h.Typeflag == tar.TypeReg || h.Typeflag == tar.TypeRegA, tr); err != nil {
				return nil, err
			}
		}
	} else if magic == [2]byte{'P', 'K'} {
		z, err := zip.OpenReader(archive)
		if err != nil {
			return nil, err
		}
		defer z.Close()
		for _, h := range z.File {
			r, err := h.Open()
			if err != nil {
				return nil, err
			}
			err = put(h.Name, h.Mode().IsRegular(), r)
			r.Close()
			if err != nil {
				return nil, err
			}
		}
	} else {
		return nil, fmt.Errorf("not a tar.gz or zip release archive")
	}
	for name := range wanted {
		if paths[name] == "" {
			return nil, fmt.Errorf("release archive missing %s", name)
		}
	}
	return paths, nil
}

// IsArchive checks content, not a filename suffix (staging files have none).
func IsArchive(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var b [2]byte
	_, err = io.ReadFull(f, b[:])
	return err == nil && (b == [2]byte{0x1f, 0x8b} || b == [2]byte{'P', 'K'})
}
