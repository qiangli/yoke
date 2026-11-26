// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package gomod

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"

	"github.com/qiangli/yoke/pkg/artifact"
)

// ModuleClosureOptions configures CaptureModuleClosure.
type ModuleClosureOptions struct {
	ModuleDirs     []string
	PublishDirs    []string
	PublishVersion string
	GoBin          string
	GOROOT         string
	GoModCache     string
	ArtifactRoot   string
	LoomRepoDir    string
	Backend        artifact.Backend
	Runner         Runner
}

// ModuleClosureResult summarizes module publications and external dependency
// artifacts captured into the protected CAS catalog.
type ModuleClosureResult struct {
	Published  []ArtifactPublication     `json:"published,omitempty"`
	Ingested   int                       `json:"ingested"`
	Roots      []artifact.RootDescriptor `json:"roots"`
	TotalBytes uint64                    `json:"total_bytes"`
}

type downloadedModJSON struct {
	Path    string `json:"Path"`
	Version string `json:"Version"`
	Error   string `json:"Error,omitempty"`
	Info    string `json:"Info,omitempty"`
	GoMod   string `json:"GoMod,omitempty"`
	Zip     string `json:"Zip,omitempty"`
}

type wantedModVersion struct {
	modPath string
	version string
	needZip bool
	strict  bool
	info    string
	gomod   string
	zip     string
}

func resolveGoModCacheDir(ctx context.Context, opts ModuleClosureOptions) (string, error) {
	if c := strings.TrimSpace(opts.GoModCache); c != "" {
		return filepath.Abs(c)
	}
	if envCache := strings.TrimSpace(os.Getenv("GOMODCACHE")); envCache != "" {
		return filepath.Abs(envCache)
	}
	if opts.GoBin != "" {
		cmd := exec.CommandContext(ctx, opts.GoBin, "env", "GOMODCACHE")
		if opts.GOROOT != "" {
			cmd.Env = append(os.Environ(), "GOROOT="+opts.GOROOT, "GOTOOLCHAIN=local")
		}
		if out, err := cmd.Output(); err == nil {
			if p := strings.TrimSpace(string(out)); p != "" {
				return filepath.Abs(p)
			}
		}
	}
	if gopath := strings.TrimSpace(os.Getenv("GOPATH")); gopath != "" {
		first := filepath.SplitList(gopath)[0]
		if first != "" {
			return filepath.Abs(filepath.Join(first, "pkg", "mod"))
		}
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return "", errors.New("gomod bootstrap: cannot determine GOMODCACHE")
	}
	return filepath.Join(home, "go", "pkg", "mod"), nil
}

func parseWantedFromGoModAndSum(modDir string, out map[string]*wantedModVersion) error {
	add := func(modPath, ver string, needZip, strict bool) {
		if modPath == "" || ver == "" || !semver.IsValid(ver) {
			return
		}
		if module.Check(modPath, ver) != nil {
			return
		}
		key := modPath + "@" + ver
		cur, exists := out[key]
		if !exists {
			out[key] = &wantedModVersion{modPath: modPath, version: ver, needZip: needZip, strict: strict}
			return
		}
		if needZip {
			cur.needZip = true
		}
		if strict {
			cur.strict = true
		}
	}

	gomodBytes, err := os.ReadFile(filepath.Join(modDir, "go.mod"))
	if err != nil {
		return fmt.Errorf("gomod bootstrap: read %s/go.mod: %w", modDir, err)
	}
	mf, err := modfile.Parse(filepath.Join(modDir, "go.mod"), gomodBytes, nil)
	if err != nil {
		mf, err = modfile.ParseLax(filepath.Join(modDir, "go.mod"), gomodBytes, nil)
	}
	if err != nil {
		return fmt.Errorf("gomod bootstrap: parse %s/go.mod: %w", modDir, err)
	}

	replacedToLocal := make(map[string]bool)
	replacedTarget := make(map[string]module.Version)
	for _, rep := range mf.Replace {
		keyOld := rep.Old.Path
		if rep.Old.Version != "" {
			keyOld += "@" + rep.Old.Version
		}
		if rep.New.Version == "" {
			replacedToLocal[rep.Old.Path] = true
			replacedToLocal[keyOld] = true
			continue
		}
		replacedTarget[keyOld] = rep.New
	}

	resolveReplacement := func(mpath, ver string) (string, string, bool) {
		if replacedToLocal[mpath] || replacedToLocal[mpath+"@"+ver] {
			return "", "", false
		}
		if tgt, ok := replacedTarget[mpath+"@"+ver]; ok {
			return tgt.Path, tgt.Version, true
		}
		if tgt, ok := replacedTarget[mpath]; ok {
			return tgt.Path, tgt.Version, true
		}
		return mpath, ver, true
	}

	for _, req := range mf.Require {
		effPath, effVer, ok := resolveReplacement(req.Mod.Path, req.Mod.Version)
		if !ok {
			continue
		}
		add(effPath, effVer, true, !req.Indirect)
	}

	sumBytes, err := os.ReadFile(filepath.Join(modDir, "go.sum"))
	if err == nil {
		for _, line := range strings.Split(string(sumBytes), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 3 {
				continue
			}
			mpath := fields[0]
			verField := fields[1]
			if ver, isModOnly := strings.CutSuffix(verField, "/go.mod"); isModOnly {
				if effPath, effVer, ok := resolveReplacement(mpath, ver); ok {
					add(effPath, effVer, false, false)
				}
			} else {
				if effPath, effVer, ok := resolveReplacement(mpath, verField); ok {
					add(effPath, effVer, true, false)
				}
			}
		}
	}
	return nil
}

func scanAllCachedDownloads(downloadRoot string, out map[string]*wantedModVersion) error {
	if _, err := os.Stat(downloadRoot); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return filepath.WalkDir(downloadRoot, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(downloadRoot, p)
		if err != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		modPath, ver, ext, ok := parseProxyCacheName(relSlash)
		if !ok {
			return nil
		}
		key := modPath + "@" + ver
		cur, exists := out[key]
		if !exists {
			cur = &wantedModVersion{modPath: modPath, version: ver}
			out[key] = cur
		}
		switch ext {
		case ".info":
			cur.info = p
		case ".mod":
			cur.gomod = p
		case ".zip":
			cur.zip = p
			cur.needZip = true
		}
		return nil
	})
}

func isCachedInDownloadRoot(downloadRoot string, w *wantedModVersion) bool {
	if downloadRoot == "" || w == nil {
		return false
	}
	escMod, err := module.EscapePath(w.modPath)
	if err != nil {
		return false
	}
	escVer, err := module.EscapeVersion(w.version)
	if err != nil {
		return false
	}
	base := filepath.Join(downloadRoot, filepath.FromSlash(escMod), "@v", escVer)
	if w.needZip {
		if fi, err := os.Stat(base + ".zip"); err != nil || !fi.Mode().IsRegular() {
			return false
		}
	}
	if _, err := os.Stat(base + ".mod"); err != nil {
		if _, ierr := os.Stat(base + ".info"); ierr != nil && !w.needZip {
			return false
		}
	}
	return true
}

func runGoModDownloadAll(ctx context.Context, modDir string, opts ModuleClosureOptions, modCache string, publishedSet map[string]bool, out map[string]*wantedModVersion) error {
	env := []string{"GOWORK=off", "GOFLAGS=-mod=readonly"}
	if modCache != "" {
		env = append(env, "GOMODCACHE="+modCache)
	}
	if opts.GOROOT != "" {
		env = append(env, "GOROOT="+opts.GOROOT, "GOTOOLCHAIN=local")
	}

	var rawOut []byte
	var runErr error
	if opts.Runner != nil {
		rawOut, runErr = opts.Runner(ctx, modDir, env, "mod", "download", "-json", "all")
	} else if opts.GoBin != "" {
		cmd := exec.CommandContext(ctx, opts.GoBin, "mod", "download", "-json", "all")
		cmd.Dir = modDir
		cmd.Env = append(os.Environ(), env...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		rawOut, runErr = cmd.Output()
		if runErr != nil && len(rawOut) == 0 {
			return fmt.Errorf("gomod bootstrap: go mod download -json all in %s: %w (%s)", modDir, runErr, strings.TrimSpace(stderr.String()))
		}
	} else {
		return nil
	}

	sawUnpublishedErr := false
	dec := json.NewDecoder(bytes.NewReader(rawOut))
	for {
		var item downloadedModJSON
		if err := dec.Decode(&item); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("gomod bootstrap: decode go mod download json: %w", err)
		}
		key := item.Path + "@" + item.Version
		if item.Error != "" {
			if publishedSet[key] {
				continue
			}
			sawUnpublishedErr = true
			return fmt.Errorf("gomod bootstrap: download %s@%s: %s", item.Path, item.Version, item.Error)
		}
		if item.Path == "" || item.Version == "" || publishedSet[key] {
			continue
		}
		cur, exists := out[key]
		if !exists {
			cur = &wantedModVersion{modPath: item.Path, version: item.Version}
			out[key] = cur
		}
		if item.Info != "" {
			cur.info = item.Info
		}
		if item.GoMod != "" {
			cur.gomod = item.GoMod
		}
		if item.Zip != "" {
			cur.zip = item.Zip
			cur.needZip = true
			cur.strict = true
		}
	}
	if sawUnpublishedErr {
		return runErr
	}
	return nil
}

// CaptureModuleClosure publishes optional local/sibling modules into Loom + CAS
// and ingests external module dependencies from GOMODCACHE/cache/download into
// artifact.Backend using the exact logical naming expected by the sealed proxy.
func CaptureModuleClosure(ctx context.Context, opts ModuleClosureOptions) (*ModuleClosureResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	artRoot := strings.TrimSpace(opts.ArtifactRoot)
	if artRoot == "" && opts.Backend == nil {
		artRoot = artifact.DefaultRoot()
	}
	if artRoot != "" {
		if abs, err := filepath.Abs(artRoot); err == nil {
			artRoot = abs
		}
	}

	loomDir := strings.TrimSpace(opts.LoomRepoDir)
	if loomDir == "" {
		loomDir = DefaultLoomRepoDir(artRoot)
	} else if abs, err := filepath.Abs(loomDir); err == nil {
		loomDir = abs
	}

	backend := opts.Backend
	closeBackend := false
	if backend == nil {
		var err error
		backend, err = artifact.ConnectOrOpen(ctx, artRoot)
		if err != nil {
			return nil, err
		}
		closeBackend = true
	}
	defer func() {
		if closeBackend {
			_ = backend.Close()
		}
	}()

	res := &ModuleClosureResult{}
	publishedSet := make(map[string]bool)

	// Collect already-published modules so we don't duplicate them as external cache entries.
	if existingPubs, err := backend.ListPublications(ctx); err == nil {
		for _, p := range existingPubs {
			publishedSet[p.Module+"@"+p.Version] = true
		}
	}

	// 1. Publish local/sibling modules into Loom + CAS when requested.
	for _, pdir := range opts.PublishDirs {
		if strings.TrimSpace(pdir) == "" {
			continue
		}
		pub, err := PublishArtifact(ctx, PublishOptions{
			Dir:          pdir,
			Version:      opts.PublishVersion,
			ArtifactRoot: artRoot,
			LoomRepoDir:  loomDir,
			Backend:      backend,
		})
		if err != nil {
			return nil, err
		}
		res.Published = append(res.Published, *pub)
		publishedSet[pub.Module+"@"+pub.Version] = true

		srcRec, err := backend.GetRecord(ctx, pub.SourceDigest)
		if err != nil {
			return nil, err
		}
		res.Roots = append(res.Roots, artifact.RootDescriptor{
			Kind:      "gomod",
			Name:      logicalPrefixSource + pub.Module,
			Version:   pub.Version,
			MediaType: mediaTypeLoomSource,
			Digest:    pub.SourceDigest,
			Size:      srcRec.Size,
			Deps:      []string{pub.InfoDigest, pub.ModDigest},
		})
		res.TotalBytes += uint64(srcRec.Size)
	}

	// 2. Discover external module versions from ModuleDirs (and GOMODCACHE/cache/download).
	modCache, cacheErr := resolveGoModCacheDir(ctx, opts)
	downloadRoot := ""
	if cacheErr == nil && modCache != "" {
		downloadRoot = filepath.Join(modCache, "cache", "download")
	}

	wanted := make(map[string]*wantedModVersion)
	sealed := artifact.IsSealed(artRoot)

	if len(opts.ModuleDirs) > 0 {
		for _, mdir := range opts.ModuleDirs {
			if strings.TrimSpace(mdir) == "" {
				continue
			}
			dirWanted := make(map[string]*wantedModVersion)
			if err := parseWantedFromGoModAndSum(mdir, dirWanted); err != nil {
				return nil, err
			}
			needsDownload := false
			for k, w := range dirWanted {
				if cur, exists := wanted[k]; !exists {
					wanted[k] = w
				} else {
					if w.needZip {
						cur.needZip = true
					}
					if w.strict {
						cur.strict = true
					}
				}
				if !publishedSet[k] && w.strict && !isCachedInDownloadRoot(downloadRoot, w) {
					needsDownload = true
				}
			}
			if !sealed && (needsDownload || opts.Runner != nil) && (opts.GoBin != "" || opts.Runner != nil) {
				if err := runGoModDownloadAll(ctx, mdir, opts, modCache, publishedSet, wanted); err != nil {
					return nil, err
				}
			}
		}
	} else if downloadRoot != "" {
		if err := scanAllCachedDownloads(downloadRoot, wanted); err != nil {
			return nil, err
		}
	}

	if len(wanted) > 0 && downloadRoot == "" {
		return nil, fmt.Errorf("gomod bootstrap: GOMODCACHE is required to ingest %d module(s)", len(wanted))
	}

	keys := make([]string, 0, len(wanted))
	for k := range wanted {
		if !publishedSet[k] {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)

	versionsByModule := make(map[string][]string)
	type pendingRoot struct {
		modPath    string
		version    string
		mediaType  string
		name       string
		digest     string
		size       int64
		infoDigest string
		modDigest  string
		hasZip     bool
	}
	var pendingRoots []pendingRoot

	for _, key := range keys {
		w := wanted[key]
		escMod, err := module.EscapePath(w.modPath)
		if err != nil {
			return nil, err
		}
		escVer, err := module.EscapeVersion(w.version)
		if err != nil {
			return nil, err
		}
		basePath := filepath.Join(downloadRoot, filepath.FromSlash(escMod), "@v", escVer)
		infoPath := w.info
		if infoPath == "" {
			infoPath = basePath + ".info"
		}
		modPathFile := w.gomod
		if modPathFile == "" {
			modPathFile = basePath + ".mod"
		}
		zipPath := w.zip
		if zipPath == "" {
			zipPath = basePath + ".zip"
		}

		infoBytes, infoErr := os.ReadFile(infoPath)
		modBytes, modErr := os.ReadFile(modPathFile)
		zipStat, zipStatErr := os.Stat(zipPath)
		hasZip := zipStatErr == nil && zipStat.Mode().IsRegular()

		if !w.strict {
			if infoErr != nil && modErr != nil && !hasZip {
				continue
			}
		} else {
			if w.needZip && !hasZip {
				return nil, fmt.Errorf("gomod bootstrap: required module zip missing from cache for %s@%s (%s)", w.modPath, w.version, zipPath)
			}
			if infoErr != nil && modErr != nil && !hasZip {
				return nil, fmt.Errorf("gomod bootstrap: module %s@%s missing from cache (%s)", w.modPath, w.version, basePath)
			}
		}
		if infoErr != nil {
			infoBytes, _ = json.Marshal(struct {
				Version string
				Time    string
			}{Version: w.version, Time: "1970-01-01T00:00:00Z"})
		}
		if modErr != nil {
			modBytes = []byte("module " + w.modPath + "\n")
		}

		rootTag := "gomod-bootstrap:" + w.modPath + "@" + w.version

		infoDigest := artifact.DigestBytes(infoBytes)
		infoRec := artifact.CatalogRecordV1{
			SchemaVersion:  artifact.CatalogRecordSchemaV1,
			Digest:         infoDigest,
			Size:           int64(len(infoBytes)),
			MediaType:      mediaTypeGoModInfo,
			LogicalName:    logicalPrefixInfo + w.modPath,
			LogicalVersion: w.version,
			Provenance:     "gomod-bootstrap",
			Retention:      artifact.RetentionProtectedRoot,
			RetentionRoots: []string{rootTag},
		}
		if _, err := backend.PutArtifact(ctx, infoRec, bytes.NewReader(infoBytes)); err != nil {
			return nil, err
		}
		res.TotalBytes += uint64(len(infoBytes))

		modDigest := artifact.DigestBytes(modBytes)
		modRec := artifact.CatalogRecordV1{
			SchemaVersion:  artifact.CatalogRecordSchemaV1,
			Digest:         modDigest,
			Size:           int64(len(modBytes)),
			MediaType:      mediaTypeGoModFile,
			LogicalName:    logicalPrefixMod + w.modPath,
			LogicalVersion: w.version,
			Provenance:     "gomod-bootstrap",
			Retention:      artifact.RetentionProtectedRoot,
			RetentionRoots: []string{rootTag},
		}
		if _, err := backend.PutArtifact(ctx, modRec, bytes.NewReader(modBytes)); err != nil {
			return nil, err
		}
		res.TotalBytes += uint64(len(modBytes))

		if hasZip {
			zf, err := os.Open(zipPath)
			if err != nil {
				return nil, err
			}
			zipBytes, readErr := io.ReadAll(zf)
			_ = zf.Close()
			if readErr != nil {
				return nil, readErr
			}
			zipDigest := artifact.DigestBytes(zipBytes)
			zipRec := artifact.CatalogRecordV1{
				SchemaVersion:  artifact.CatalogRecordSchemaV1,
				Digest:         zipDigest,
				Size:           int64(len(zipBytes)),
				MediaType:      mediaTypeGoModZip,
				LogicalName:    logicalPrefixZip + w.modPath,
				LogicalVersion: w.version,
				Dependencies:   []string{infoDigest, modDigest},
				Provenance:     "gomod-bootstrap",
				Retention:      artifact.RetentionProtectedRoot,
				RetentionRoots: []string{rootTag},
			}
			if _, err := backend.PutArtifact(ctx, zipRec, bytes.NewReader(zipBytes)); err != nil {
				return nil, err
			}
			res.TotalBytes += uint64(len(zipBytes))
			pendingRoots = append(pendingRoots, pendingRoot{
				modPath:    w.modPath,
				version:    w.version,
				mediaType:  mediaTypeGoModZip,
				name:       logicalPrefixZip + w.modPath,
				digest:     zipDigest,
				size:       int64(len(zipBytes)),
				infoDigest: infoDigest,
				modDigest:  modDigest,
				hasZip:     true,
			})
		} else {
			pendingRoots = append(pendingRoots, pendingRoot{
				modPath:    w.modPath,
				version:    w.version,
				mediaType:  mediaTypeGoModFile,
				name:       logicalPrefixMod + w.modPath,
				digest:     modDigest,
				size:       int64(len(modBytes)),
				infoDigest: infoDigest,
				modDigest:  modDigest,
				hasZip:     false,
			})
		}

		versionsByModule[w.modPath] = append(versionsByModule[w.modPath], w.version)
		res.Ingested++
	}

	// 3. Persist gomod-cached-versions/<mod>@list for every ingested module so @v/list and Query work offline.
	listDigests := make(map[string]string, len(versionsByModule))
	modNames := make([]string, 0, len(versionsByModule))
	for m := range versionsByModule {
		modNames = append(modNames, m)
	}
	slices.Sort(modNames)

	for _, modPath := range modNames {
		vers := slices.Clone(versionsByModule[modPath])
		if st, err := backend.LookupLogical(ctx, logicalPrefixCachedVersions+modPath, "list"); err == nil && st.PayloadPresent {
			if raw, rerr := backend.FetchVerified(ctx, st.Record.Digest); rerr == nil {
				vers = append(vers, strings.Fields(string(raw))...)
			}
		}
		semver.Sort(vers)
		vers = slices.Compact(vers)
		listBytes := []byte(strings.Join(vers, "\n") + "\n")
		listDigest := artifact.DigestBytes(listBytes)
		listRec := artifact.CatalogRecordV1{
			SchemaVersion:  artifact.CatalogRecordSchemaV1,
			Digest:         listDigest,
			Size:           int64(len(listBytes)),
			MediaType:      mediaTypeGoModFile,
			LogicalName:    logicalPrefixCachedVersions + modPath,
			LogicalVersion: "list",
			Provenance:     "gomod-bootstrap",
			Retention:      artifact.RetentionProtectedRoot,
			RetentionRoots: []string{"gomod-bootstrap-list:" + modPath},
		}
		if _, err := backend.PutArtifact(ctx, listRec, bytes.NewReader(listBytes)); err != nil {
			return nil, err
		}
		res.TotalBytes += uint64(len(listBytes))
		listDigests[modPath] = listDigest
	}

	for _, pr := range pendingRoots {
		var deps []string
		if pr.hasZip {
			deps = []string{pr.infoDigest, pr.modDigest}
		} else {
			deps = []string{pr.infoDigest}
		}
		if ld := listDigests[pr.modPath]; ld != "" {
			deps = append(deps, ld)
		}
		res.Roots = append(res.Roots, artifact.RootDescriptor{
			Kind:      "gomod",
			Name:      pr.name,
			Version:   pr.version,
			MediaType: pr.mediaType,
			Digest:    pr.digest,
			Size:      pr.size,
			Deps:      deps,
		})
	}

	return res, nil
}
