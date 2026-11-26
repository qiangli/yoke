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
	"log/slog"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-billy/v5/memfs"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/goproxy/goproxy"
	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
	"golang.org/x/mod/sumdb/dirhash"
	modzip "golang.org/x/mod/zip"
	"golang.org/x/sync/singleflight"

	"github.com/qiangli/coreutils/pkg/weavecli"
	"github.com/qiangli/yoke/pkg/artifact"
)

// LoomEnv overrides the durable bare Loom repository directory for published modules.
const LoomEnv = "BASHY_LOOM_DIR"

const (
	logicalPrefixSource         = "gomod-source/"
	logicalPrefixInfo           = "gomod-info/"
	logicalPrefixMod            = "gomod-mod/"
	logicalPrefixZip            = "gomod-zip/"
	logicalPrefixCachedVersions = "gomod-cached-versions/"

	mediaTypeLoomSource = "application/vnd.bashy.loom.source.v1+json"
	mediaTypeGoModInfo  = "application/json"
	mediaTypeGoModFile  = "text/plain; charset=utf-8"
	mediaTypeGoModZip   = "application/zip"

	defaultReadLeaseTTL = 2 * time.Minute
	regenTimeout        = 2 * time.Minute
)

var loomWriteMu sync.Mutex

// DefaultLoomRepoDir resolves the durable bare Loom Git repository directory
// holding immutable published module refs (<artifact_root>/loom/modules.git).
func DefaultLoomRepoDir(artifactRoot string) string {
	if d := strings.TrimSpace(os.Getenv(LoomEnv)); d != "" {
		return d
	}
	root := strings.TrimSpace(artifactRoot)
	if root == "" {
		root = artifact.DefaultRoot()
	}
	return filepath.Join(root, "loom", "modules.git")
}

// SealedBuildEnv returns the hermetic Go build environment for sealed offline
// builds against an embedded proxy URL: no `,direct` or public proxy fallback,
// local toolchain only, no sum.golang.org network lookup, and empty GOPRIVATE /
// GONOPROXY so inherited host patterns cannot bypass the local proxy.
func SealedBuildEnv(proxyURL string, getenv func(string) string) []string {
	_ = getenv
	return []string{
		"GOPROXY=" + strings.TrimSpace(proxyURL),
		"GOTOOLCHAIN=local",
		"GOSUMDB=off",
		"GONOPROXY=",
		"GOPRIVATE=",
	}
}

// HashGoModBytes returns the canonical go.sum h1: hash for a go.mod file.
func HashGoModBytes(gomod []byte) (string, error) {
	return dirhash.DefaultHash([]string{"go.mod"}, func(name string) (io.ReadCloser, error) {
		if name != "go.mod" {
			return nil, fs.ErrNotExist
		}
		return io.NopCloser(bytes.NewReader(gomod)), nil
	})
}

// loomSourceDescriptor is persisted in the artifact store as a RetentionProtectedRoot
// anchor binding a published module version's recipe to its durable Loom ref and commit.
type loomSourceDescriptor struct {
	Schema  string `json:"schema"`
	Module  string `json:"module"`
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Tree    string `json:"tree"`
	Subdir  string `json:"subdir,omitempty"`
	LoomRef string `json:"loom_ref"`
}

// PublishOptions configures immutable internal module publication into Loom + CAS.
type PublishOptions struct {
	Dir          string
	Version      string
	ArtifactRoot string
	LoomRepoDir  string
	Backend      artifact.Backend
}

// ArtifactPublication reports the result of PublishArtifact.
type ArtifactPublication struct {
	Module       string                     `json:"module"`
	Version      string                     `json:"version"`
	Commit       string                     `json:"commit"`
	Subdir       string                     `json:"subdir,omitempty"`
	LoomRef      string                     `json:"loom_ref"`
	Hash         string                     `json:"hash"`
	GoModSum     string                     `json:"gomod_sum"`
	InfoDigest   string                     `json:"info_digest"`
	ModDigest    string                     `json:"mod_digest"`
	ZipDigest    string                     `json:"zip_digest"`
	SourceDigest string                     `json:"source_digest"`
	ArtifactRoot string                     `json:"artifact_root,omitempty"`
	LoomDir      string                     `json:"loom_dir"`
	Published    bool                       `json:"published"`
	Record       artifact.PublicationRecord `json:"record"`
}

// PublishArtifact persists the module's exact Git commit and immutable Loom ref
// BEFORE storing canonical .info, .mod, and reconstructible .zip artifacts and
// committing the immutable PublicationRecord in the catalog.
func PublishArtifact(ctx context.Context, opts PublishOptions) (*ArtifactPublication, error) {
	if strings.TrimSpace(opts.Dir) == "" {
		return nil, &Error{Code: weavecli.ExitInvalidArg, Msg: "module directory is required"}
	}
	abs, err := filepath.Abs(opts.Dir)
	if err != nil {
		return nil, err
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}

	diskGoMod, err := os.ReadFile(filepath.Join(abs, "go.mod"))
	if err != nil {
		return nil, &Error{Code: weavecli.ExitInvalidArg, Msg: opts.Dir + " is not a module directory: " + err.Error(), Err: err}
	}
	modPath := modfile.ModulePath(diskGoMod)
	if modPath == "" {
		return nil, &Error{Code: weavecli.ExitInvalidArg, Msg: opts.Dir + "/go.mod declares no module path"}
	}
	if err := module.CheckPath(modPath); err != nil {
		return nil, &Error{
			Code: weavecli.ExitInvalidArg,
			Msg:  fmt.Sprintf("module path %q cannot be served by a proxy (%v); give it a domain-rooted path such as github.com/OWNER/REPO/SUBDIR", modPath, err),
			Err:  err,
		}
	}

	version := strings.TrimSpace(opts.Version)
	if version != "" {
		if !semver.IsValid(version) || semver.Canonical(version) != version {
			return nil, &Error{Code: weavecli.ExitInvalidArg, Msg: fmt.Sprintf("version %q is not a canonical semantic version (vX.Y.Z)", version)}
		}
		if err := module.Check(modPath, version); err != nil {
			return nil, &Error{Code: weavecli.ExitInvalidArg, Msg: err.Error(), Err: err}
		}
	}

	artRoot := strings.TrimSpace(opts.ArtifactRoot)
	if artRoot == "" && opts.Backend == nil {
		artRoot = artifact.DefaultRoot()
	}
	if artRoot != "" {
		if resolved, err := filepath.Abs(artRoot); err == nil {
			artRoot = resolved
		}
	}

	loomDir := strings.TrimSpace(opts.LoomRepoDir)
	if loomDir == "" {
		loomDir = DefaultLoomRepoDir(artRoot)
	}
	if resolved, err := filepath.Abs(loomDir); err == nil {
		loomDir = resolved
	}

	backend := opts.Backend
	closeBackend := false
	if backend == nil {
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

	// Step 1: Resolve or synthesize exact Git commit, verify cleanliness, and
	// persist commit + tree + immutable ref into the durable bare Loom repo BEFORE catalog publication.
	var (
		srcRepo *gogit.Repository
		commit  *object.Commit
		subdir  string
	)

	workRepo, workRoot, repoErr := openWorktree(abs)
	if version == "" {
		if repoErr != nil {
			return nil, &Error{Code: weavecli.ExitInvalidArg, Msg: "a pseudo-version needs a git commit (" + repoErr.Error() + "); pass --version", Err: repoErr}
		}
		headRef, err := workRepo.Head()
		if err != nil {
			return nil, &Error{Code: weavecli.ExitInvalidArg, Msg: "a pseudo-version needs a git commit (" + err.Error() + "); pass --version", Err: err}
		}
		c, err := workRepo.CommitObject(headRef.Hash())
		if err != nil {
			return nil, err
		}
		rel, err := filepath.Rel(workRoot, abs)
		if err != nil {
			return nil, err
		}
		relSlash := filepath.ToSlash(rel)
		if relSlash != "." {
			subdir = relSlash
		}
		// Re-verify go.mod module path inside the HEAD commit tree itself.
		committedGoMod, _, err := commitFiles(c, relSlash)
		if err != nil {
			return nil, err
		}
		if gotMod := modfile.ModulePath(committedGoMod); gotMod != modPath {
			if gotMod == "" {
				return nil, &Error{Code: weavecli.ExitInvalidArg, Msg: opts.Dir + "/go.mod at HEAD declares no module path"}
			}
			modPath = gotMod
			if err := module.CheckPath(modPath); err != nil {
				return nil, &Error{Code: weavecli.ExitInvalidArg, Msg: err.Error(), Err: err}
			}
		}
		when := c.Committer.When.UTC()
		_, pathMajor, _ := module.SplitPathVersion(modPath)
		version = module.PseudoVersion(module.PathMajorPrefix(pathMajor), "", when, c.Hash.String()[:12])
		if err := module.Check(modPath, version); err != nil {
			return nil, &Error{Code: weavecli.ExitInvalidArg, Msg: err.Error(), Err: err}
		}
		srcRepo = workRepo
		commit = c
	} else if repoErr == nil {
		headRef, err := workRepo.Head()
		if err != nil {
			return nil, &Error{
				Code: weavecli.ExitPrecondFail,
				Msg:  fmt.Sprintf("%s is an uncommitted git repository (%v); create a commit before publishing %s", opts.Dir, err, version),
				Err:  err,
			}
		}
		c, err := workRepo.CommitObject(headRef.Hash())
		if err != nil {
			return nil, err
		}
		rel, err := filepath.Rel(workRoot, abs)
		if err != nil {
			return nil, err
		}
		relSlash := filepath.ToSlash(rel)
		if relSlash != "." {
			subdir = relSlash
		}
		if err := verifyCleanModuleWorktree(workRepo, c, abs, relSlash); err != nil {
			return nil, &Error{
				Code: weavecli.ExitPrecondFail,
				Msg:  fmt.Sprintf("%s has uncommitted changes to tracked module files (%v); commit before publishing %s", opts.Dir, err, version),
				Err:  err,
			}
		}
		srcRepo = workRepo
		commit = c
	}

	loomRef, err := artifact.LoomPublicationRef(modPath, version)
	if err != nil {
		return nil, &Error{Code: weavecli.ExitInvalidArg, Msg: err.Error(), Err: err}
	}

	loomCommit, err := persistCommitToLoom(loomDir, loomRef, modPath, version, abs, srcRepo, commit)
	if err != nil {
		if errors.Is(err, artifact.ErrImmutablePublicationConflict) {
			return nil, &Error{Code: weavecli.ExitPrecondFail, Msg: err.Error(), Err: err}
		}
		return nil, err
	}
	commitSHA := loomCommit.Hash.String()

	relForTree := "."
	if subdir != "" {
		relForTree = subdir
	}
	gomodBytes, files, err := commitFiles(loomCommit, relForTree)
	if err != nil {
		return nil, err
	}
	files = filterNestedModulesFromCommitFiles(files)

	// Step 2: Build canonical .info, .mod, and .zip + h1: hashes from persisted Loom commit.
	mver := module.Version{Path: modPath, Version: version}
	var zbuf bytes.Buffer
	if err := modzip.Create(&zbuf, mver, files); err != nil {
		return nil, fmt.Errorf("zip %s: %w", mver, err)
	}
	zipBytes := zbuf.Bytes()
	zipDigest := artifact.DigestBytes(zipBytes)
	h1Hash, err := hashZipBytes(zipBytes)
	if err != nil {
		return nil, err
	}
	gomodSum, err := HashGoModBytes(gomodBytes)
	if err != nil {
		return nil, err
	}

	when := loomCommit.Committer.When.UTC().Truncate(time.Second)
	infoBytes, err := json.Marshal(struct {
		Version string
		Time    string
	}{
		Version: version,
		Time:    when.Format(time.RFC3339),
	})
	if err != nil {
		return nil, err
	}
	infoDigest := artifact.DigestBytes(infoBytes)
	modDigest := artifact.DigestBytes(gomodBytes)

	srcDesc := loomSourceDescriptor{
		Schema:  "bashy-loom-source-v1",
		Module:  modPath,
		Version: version,
		Commit:  commitSHA,
		Tree:    loomCommit.TreeHash.String(),
		Subdir:  subdir,
		LoomRef: loomRef,
	}
	srcBytes, err := json.Marshal(srcDesc)
	if err != nil {
		return nil, err
	}
	sourceDigest := artifact.DigestBytes(srcBytes)

	candidate := artifact.PublicationRecord{
		SchemaVersion: artifact.PublicationSchemaV1,
		Module:        modPath,
		Version:       version,
		Commit:        commitSHA,
		Subdir:        subdir,
		LoomRef:       loomRef,
		InfoDigest:    infoDigest,
		ModDigest:     modDigest,
		ZipDigest:     zipDigest,
		GoSumH1:       h1Hash,
		PublishedAt:   time.Now().UTC(),
	}

	// Check existing publication for idempotency or immutable conflict BEFORE writing blobs.
	if existing, getErr := backend.GetPublication(ctx, modPath, version); getErr == nil {
		idem, chkErr := artifact.CheckImmutablePublication(existing, candidate)
		if chkErr != nil {
			return nil, &Error{Code: weavecli.ExitPrecondFail, Msg: chkErr.Error(), Err: chkErr}
		}
		if idem {
			return &ArtifactPublication{
				Module:       modPath,
				Version:      version,
				Commit:       existing.Commit,
				Subdir:       existing.Subdir,
				LoomRef:      existing.LoomRef,
				Hash:         existing.GoSumH1,
				GoModSum:     gomodSum,
				InfoDigest:   existing.InfoDigest,
				ModDigest:    existing.ModDigest,
				ZipDigest:    existing.ZipDigest,
				SourceDigest: sourceDigest,
				ArtifactRoot: artRoot,
				LoomDir:      loomDir,
				Published:    false,
				Record:       existing,
			}, nil
		}
	} else if !errors.Is(getErr, artifact.ErrNotFound) {
		return nil, getErr
	}

	// Persist protected source-anchor record so artifact.CanEvict tracks retained recipe input.
	srcRec := artifact.CatalogRecordV1{
		SchemaVersion:  artifact.CatalogRecordSchemaV1,
		Digest:         sourceDigest,
		Size:           int64(len(srcBytes)),
		MediaType:      mediaTypeLoomSource,
		LogicalName:    logicalPrefixSource + modPath,
		LogicalVersion: version,
		Provenance:     "loom:" + loomRef,
		Retention:      artifact.RetentionProtectedRoot,
		RetentionRoots: []string{"loom:" + loomRef},
	}
	if _, err := backend.PutArtifact(ctx, srcRec, bytes.NewReader(srcBytes)); err != nil {
		return nil, err
	}

	// Persist protected .info and .mod records.
	infoRec := artifact.CatalogRecordV1{
		SchemaVersion:  artifact.CatalogRecordSchemaV1,
		Digest:         infoDigest,
		Size:           int64(len(infoBytes)),
		MediaType:      mediaTypeGoModInfo,
		LogicalName:    logicalPrefixInfo + modPath,
		LogicalVersion: version,
		Dependencies:   []string{sourceDigest},
		Provenance:     "loom:" + loomRef,
		Retention:      artifact.RetentionProtectedRoot,
		RetentionRoots: []string{"gomod-pub:" + modPath + "@" + version},
	}
	if _, err := backend.PutArtifact(ctx, infoRec, bytes.NewReader(infoBytes)); err != nil {
		return nil, err
	}

	modRec := artifact.CatalogRecordV1{
		SchemaVersion:  artifact.CatalogRecordSchemaV1,
		Digest:         modDigest,
		Size:           int64(len(gomodBytes)),
		MediaType:      mediaTypeGoModFile,
		LogicalName:    logicalPrefixMod + modPath,
		LogicalVersion: version,
		Dependencies:   []string{sourceDigest},
		Provenance:     "loom:" + loomRef,
		Retention:      artifact.RetentionProtectedRoot,
		RetentionRoots: []string{"gomod-pub:" + modPath + "@" + version},
	}
	if _, err := backend.PutArtifact(ctx, modRec, bytes.NewReader(gomodBytes)); err != nil {
		return nil, err
	}

	// Persist reconstructible .zip record bound to RecipeGoModZipV1.
	zipRec := artifact.CatalogRecordV1{
		SchemaVersion:  artifact.CatalogRecordSchemaV1,
		Digest:         zipDigest,
		Size:           int64(len(zipBytes)),
		MediaType:      mediaTypeGoModZip,
		LogicalName:    logicalPrefixZip + modPath,
		LogicalVersion: version,
		Provenance:     "loom:" + loomRef,
		Retention:      artifact.RetentionReconstructible,
		Recipe: &artifact.RegenerationRecipe{
			Kind:           artifact.RecipeGoModZipV1,
			InputDigests:   []string{sourceDigest},
			LoomRef:        loomRef,
			Commit:         commitSHA,
			Subdir:         subdir,
			ExpectedDigest: zipDigest,
			ExpectedGoSum:  h1Hash,
		},
	}
	if _, err := backend.PutArtifact(ctx, zipRec, bytes.NewReader(zipBytes)); err != nil {
		return nil, err
	}

	idem, err := backend.PublishModule(ctx, candidate)
	if err != nil {
		if errors.Is(err, artifact.ErrImmutablePublicationConflict) {
			return nil, &Error{Code: weavecli.ExitPrecondFail, Msg: err.Error(), Err: err}
		}
		return nil, err
	}

	return &ArtifactPublication{
		Module:       modPath,
		Version:      version,
		Commit:       commitSHA,
		Subdir:       subdir,
		LoomRef:      loomRef,
		Hash:         h1Hash,
		GoModSum:     gomodSum,
		InfoDigest:   infoDigest,
		ModDigest:    modDigest,
		ZipDigest:    zipDigest,
		SourceDigest: sourceDigest,
		ArtifactRoot: artRoot,
		LoomDir:      loomDir,
		Published:    !idem,
		Record:       candidate,
	}, nil
}

func persistCommitToLoom(loomDir, loomRef, modPath, version, absDir string, srcRepo *gogit.Repository, commit *object.Commit) (*object.Commit, error) {
	loomWriteMu.Lock()
	defer loomWriteMu.Unlock()

	loomRepo, err := openOrInitBareLoomRepo(loomDir)
	if err != nil {
		return nil, err
	}

	var targetHash plumbing.Hash
	if srcRepo != nil && commit != nil {
		targetHash = commit.Hash
		if existingRef, refErr := loomRepo.Reference(plumbing.ReferenceName(loomRef), false); refErr == nil {
			if existingRef.Hash() != targetHash {
				return nil, fmt.Errorf("%w: %s@%s loom ref %s points to %s, candidate commit is %s",
					artifact.ErrImmutablePublicationConflict, modPath, version, loomRef, existingRef.Hash(), targetHash)
			}
		}
		if err := copyCommitAndTreeObjects(srcRepo.Storer, loomRepo.Storer, targetHash); err != nil {
			return nil, fmt.Errorf("persist commit %s into loom: %w", targetHash, err)
		}
	} else {
		synthRepo, synthHash, err := synthesizeDeterministicRepo(absDir, modPath, version)
		if err != nil {
			return nil, err
		}
		targetHash = synthHash
		if existingRef, refErr := loomRepo.Reference(plumbing.ReferenceName(loomRef), false); refErr == nil {
			if existingRef.Hash() != targetHash {
				return nil, fmt.Errorf("%w: %s@%s loom ref %s points to %s, candidate commit is %s",
					artifact.ErrImmutablePublicationConflict, modPath, version, loomRef, existingRef.Hash(), targetHash)
			}
		}
		if err := copyCommitAndTreeObjects(synthRepo.Storer, loomRepo.Storer, targetHash); err != nil {
			return nil, fmt.Errorf("persist synthesized commit %s into loom: %w", targetHash, err)
		}
	}

	loomCommit, err := loomRepo.CommitObject(targetHash)
	if err != nil {
		return nil, fmt.Errorf("verify persisted loom commit %s: %w", targetHash, err)
	}
	if _, err := loomCommit.Tree(); err != nil {
		return nil, fmt.Errorf("verify persisted loom tree %s: %w", targetHash, err)
	}

	if existingRef, refErr := loomRepo.Reference(plumbing.ReferenceName(loomRef), false); refErr == nil {
		if existingRef.Hash() != targetHash {
			return nil, fmt.Errorf("%w: %s@%s loom ref %s points to %s, candidate commit is %s",
				artifact.ErrImmutablePublicationConflict, modPath, version, loomRef, existingRef.Hash(), targetHash)
		}
		return loomCommit, nil
	}

	ref := plumbing.NewHashReference(plumbing.ReferenceName(loomRef), targetHash)
	if err := loomRepo.Storer.SetReference(ref); err != nil {
		return nil, fmt.Errorf("write loom ref %s: %w", loomRef, err)
	}
	return loomCommit, nil
}

func openOrInitBareLoomRepo(loomDir string) (*gogit.Repository, error) {
	if err := os.MkdirAll(loomDir, 0o755); err != nil {
		return nil, err
	}
	repo, err := gogit.PlainOpen(loomDir)
	if err == nil {
		return repo, nil
	}
	repo, initErr := gogit.PlainInit(loomDir, true)
	if initErr != nil {
		if errors.Is(initErr, gogit.ErrRepositoryAlreadyExists) {
			return gogit.PlainOpen(loomDir)
		}
		return nil, initErr
	}
	return repo, nil
}

func synthesizeDeterministicRepo(absDir, modPath, version string) (*gogit.Repository, plumbing.Hash, error) {
	memStore := memory.NewStorage()
	fsys := memfs.New()
	repo, err := gogit.Init(memStore, fsys)
	if err != nil {
		return nil, plumbing.ZeroHash, err
	}
	wt, err := repo.Worktree()
	if err != nil {
		return nil, plumbing.ZeroHash, err
	}

	var relPaths []string
	err = filepath.WalkDir(absDir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			if p != absDir {
				if _, statErr := os.Stat(filepath.Join(p, "go.mod")); statErr == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(absDir, p)
		if err != nil {
			return err
		}
		relSlash := filepath.ToSlash(rel)
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if dir := path.Dir(relSlash); dir != "." {
			if err := fsys.MkdirAll(dir, 0o755); err != nil {
				return err
			}
		}
		f, err := fsys.OpenFile(relSlash, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		if _, err := f.Write(data); err != nil {
			_ = f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		relPaths = append(relPaths, relSlash)
		return nil
	})
	if err != nil {
		return nil, plumbing.ZeroHash, err
	}

	slices.Sort(relPaths)
	for _, relSlash := range relPaths {
		if _, err := wt.Add(relSlash); err != nil {
			return nil, plumbing.ZeroHash, err
		}
	}

	sig := &object.Signature{
		Name:  "bashy",
		Email: "bashy@local.invalid",
		When:  time.Unix(0, 0).UTC(),
	}
	h, err := wt.Commit("publish "+modPath+"@"+version, &gogit.CommitOptions{
		Author:            sig,
		Committer:         sig,
		AllowEmptyCommits: true,
	})
	if err != nil {
		return nil, plumbing.ZeroHash, err
	}
	return repo, h, nil
}

func verifyCleanModuleWorktree(repo *gogit.Repository, commit *object.Commit, absDir, relSlash string) error {
	tree, err := commit.Tree()
	if err != nil {
		return err
	}
	if relSlash != "." && relSlash != "" {
		tree, err = tree.Tree(relSlash)
		if err != nil {
			return err
		}
	}

	nestedDirs := map[string]bool{}
	committed := map[string]plumbing.Hash{}
	err = tree.Files().ForEach(func(f *object.File) error {
		if f.Mode != filemode.Regular && f.Mode != filemode.Executable && f.Mode != filemode.Deprecated {
			return nil
		}
		if f.Name != "go.mod" && path.Base(f.Name) == "go.mod" {
			nestedDirs[path.Dir(f.Name)] = true
		}
		committed[f.Name] = f.Hash
		return nil
	})
	if err != nil {
		return err
	}

	inNestedModule := func(relFile string) bool {
		for nd := range nestedDirs {
			if relFile == nd || strings.HasPrefix(relFile, nd+"/") {
				return true
			}
		}
		for dir := path.Dir(relFile); dir != "." && dir != ""; dir = path.Dir(dir) {
			if _, err := os.Stat(filepath.Join(absDir, filepath.FromSlash(dir), "go.mod")); err == nil {
				return true
			}
		}
		return false
	}

	for name, wantHash := range committed {
		if inNestedModule(name) {
			continue
		}
		diskPath := filepath.Join(absDir, filepath.FromSlash(name))
		data, err := os.ReadFile(diskPath)
		if err != nil {
			return fmt.Errorf("tracked file %s missing or unreadable: %w", name, err)
		}
		if gotHash := plumbing.ComputeHash(plumbing.BlobObject, data); gotHash != wantHash {
			return fmt.Errorf("tracked file %s has uncommitted modifications", name)
		}
	}

	if idx, err := repo.Storer.Index(); err == nil && idx != nil {
		prefix := ""
		if relSlash != "." && relSlash != "" {
			prefix = relSlash + "/"
		}
		for _, e := range idx.Entries {
			if !strings.HasPrefix(e.Name, prefix) {
				continue
			}
			name := strings.TrimPrefix(e.Name, prefix)
			if name == "" || inNestedModule(name) {
				continue
			}
			wantHash, ok := committed[name]
			if !ok {
				return fmt.Errorf("staged file %s is not committed at HEAD", name)
			}
			if e.Hash != wantHash {
				return fmt.Errorf("staged file %s differs from HEAD", name)
			}
		}
	}
	return nil
}

func filterNestedModulesFromCommitFiles(files []modzip.File) []modzip.File {
	nested := map[string]bool{}
	for _, f := range files {
		p := f.Path()
		if p != "go.mod" && path.Base(p) == "go.mod" {
			nested[path.Dir(p)] = true
		}
	}
	if len(nested) == 0 {
		return files
	}
	out := make([]modzip.File, 0, len(files))
	for _, f := range files {
		p := f.Path()
		skip := false
		for nd := range nested {
			if p == nd || strings.HasPrefix(p, nd+"/") {
				skip = true
				break
			}
		}
		if !skip {
			out = append(out, f)
		}
	}
	return out
}

func copyCommitAndTreeObjects(src, dst storer.EncodedObjectStorer, commitHash plumbing.Hash) error {
	c, err := object.GetCommit(src, commitHash)
	if err != nil {
		return err
	}
	if err := copyTreeObjectsRecursive(src, dst, c.TreeHash); err != nil {
		return err
	}
	if err := copySingleEncodedObject(src, dst, commitHash); err != nil {
		return err
	}

	missingParent := false
	for _, p := range c.ParentHashes {
		if dst.HasEncodedObject(p) != nil {
			missingParent = true
			break
		}
	}
	if missingParent {
		if ss, ok := dst.(storer.ShallowStorer); ok {
			cur, err := ss.Shallow()
			if err == nil && !slices.Contains(cur, commitHash) {
				cur = append(cur, commitHash)
				_ = ss.SetShallow(cur)
			}
		}
	}
	return nil
}

func copyTreeObjectsRecursive(src, dst storer.EncodedObjectStorer, treeHash plumbing.Hash) error {
	if err := copySingleEncodedObject(src, dst, treeHash); err != nil {
		return err
	}
	tree, err := object.GetTree(src, treeHash)
	if err != nil {
		return err
	}
	for _, entry := range tree.Entries {
		switch entry.Mode {
		case filemode.Submodule:
			continue
		case filemode.Dir:
			if err := copyTreeObjectsRecursive(src, dst, entry.Hash); err != nil {
				return err
			}
		default:
			if err := copySingleEncodedObject(src, dst, entry.Hash); err != nil {
				return err
			}
		}
	}
	return nil
}

func copySingleEncodedObject(src, dst storer.EncodedObjectStorer, h plumbing.Hash) error {
	if dst.HasEncodedObject(h) == nil {
		return nil
	}
	obj, err := src.EncodedObject(plumbing.AnyObject, h)
	if err != nil {
		return err
	}
	rc, err := obj.Reader()
	if err != nil {
		return err
	}
	defer rc.Close()

	dstObj := dst.NewEncodedObject()
	dstObj.SetType(obj.Type())
	dstObj.SetSize(obj.Size())
	wc, err := dstObj.Writer()
	if err != nil {
		return err
	}
	if _, err := io.Copy(wc, rc); err != nil {
		_ = wc.Close()
		return err
	}
	if err := wc.Close(); err != nil {
		return err
	}
	got, err := dst.SetEncodedObject(dstObj)
	if err != nil {
		return err
	}
	if got != h {
		return fmt.Errorf("copied git object hash mismatch: got %s, want %s", got, h)
	}
	return nil
}

// CatalogVersionStatus reports one published version's hashes and CAS .zip presence.
type CatalogVersionStatus struct {
	Version    string `json:"version"`
	Commit     string `json:"commit"`
	Subdir     string `json:"subdir,omitempty"`
	LoomRef    string `json:"loom_ref"`
	Hash       string `json:"hash"`
	ZipDigest  string `json:"zip_digest"`
	ZipPresent bool   `json:"zip_present"`
}

// CatalogModuleStatus summarizes all published versions of one module in artifact.Backend.
type CatalogModuleStatus struct {
	Module       string                 `json:"module"`
	Versions     []string               `json:"versions"`
	Publications []CatalogVersionStatus `json:"publications"`
}

// CatalogStatusOptions configures CatalogProxyStatus.
type CatalogStatusOptions struct {
	ArtifactRoot string
	Backend      artifact.Backend
}

// CatalogProxyStatus lists published modules, versions, hashes, and .zip CAS presence
// from artifact.Backend without creating directories or bumping service_epoch when empty.
func CatalogProxyStatus(ctx context.Context, opts CatalogStatusOptions) ([]CatalogModuleStatus, error) {
	backend := opts.Backend
	closeBackend := false
	if backend == nil {
		root := strings.TrimSpace(opts.ArtifactRoot)
		if root == "" {
			root = artifact.DefaultRoot()
		}
		if client, ok := artifact.TryConnectService(ctx, root); ok {
			backend = client
			closeBackend = true
		} else {
			dbPath := filepath.Join(root, "catalog.db")
			fi, err := os.Stat(dbPath)
			if os.IsNotExist(err) {
				return nil, nil
			}
			if err != nil {
				return nil, err
			}
			if fi.IsDir() {
				return nil, nil
			}
			store, err := artifact.OpenStore(ctx, artifact.StoreOptions{Root: root, ReadOnly: true})
			if err != nil {
				if errors.Is(err, artifact.ErrNotFound) {
					return nil, nil
				}
				return nil, err
			}
			backend = store
			closeBackend = true
		}
	}
	defer func() {
		if closeBackend {
			_ = backend.Close()
		}
	}()

	pubs, err := backend.ListPublications(ctx)
	if err != nil {
		return nil, err
	}
	if len(pubs) == 0 {
		return nil, nil
	}

	byMod := make(map[string][]artifact.PublicationRecord)
	for _, pub := range pubs {
		byMod[pub.Module] = append(byMod[pub.Module], pub)
	}

	modNames := make([]string, 0, len(byMod))
	for m := range byMod {
		modNames = append(modNames, m)
	}
	sort.Strings(modNames)

	out := make([]CatalogModuleStatus, 0, len(modNames))
	for _, m := range modNames {
		mpubs := byMod[m]
		slices.SortFunc(mpubs, func(a, b artifact.PublicationRecord) int {
			if c := semver.Compare(a.Version, b.Version); c != 0 {
				return c
			}
			return strings.Compare(a.Version, b.Version)
		})
		ms := CatalogModuleStatus{
			Module:       m,
			Versions:     make([]string, 0, len(mpubs)),
			Publications: make([]CatalogVersionStatus, 0, len(mpubs)),
		}
		for _, p := range mpubs {
			zipPresent := false
			if st, err := backend.GetRecordStatus(ctx, p.ZipDigest); err == nil {
				zipPresent = st.PayloadPresent
			}
			ms.Versions = append(ms.Versions, p.Version)
			ms.Publications = append(ms.Publications, CatalogVersionStatus{
				Version:    p.Version,
				Commit:     p.Commit,
				Subdir:     p.Subdir,
				LoomRef:    p.LoomRef,
				Hash:       p.GoSumH1,
				ZipDigest:  p.ZipDigest,
				ZipPresent: zipPresent,
			})
		}
		out = append(out, ms)
	}
	return out, nil
}

// ProxyServerOptions configures NewProxyHandler and StartProxyServer.
type ProxyServerOptions struct {
	Addr                   string // default "127.0.0.1:0"
	AllowMeshBind          bool   // required to bind non-loopback addresses
	ArtifactRoot           string
	LoomRepoDir            string
	Backend                artifact.Backend
	CloseBackendOnShutdown bool
	Sealed                 bool
	Fetcher                goproxy.Fetcher
	Logger                 *slog.Logger
	OnRegenerate           func(module, version string)
}

func effectiveSealed(explicit bool, artifactRoot string) bool {
	if explicit {
		return true
	}
	return artifact.IsSealed(artifactRoot)
}

func validateProxyBindAddr(addr string, allowMesh bool) (string, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("gomod proxy: invalid bind address %q: %w", addr, err)
	}
	if strings.EqualFold(host, "localhost") {
		host = "127.0.0.1"
		addr = net.JoinHostPort(host, port)
	}
	if allowMesh {
		return addr, nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return "", fmt.Errorf("%w: %q", artifact.ErrNonLoopbackBind, addr)
	}
	return addr, nil
}

// ProxyServer serves the standard Go module proxy HTTP protocol backed by
// artifact.Backend CAS + durable Loom source regeneration.
type ProxyServer struct {
	addr         string
	url          string
	sealed       bool
	backend      artifact.Backend
	closeBackend bool
	ln           net.Listener
	httpSrv      *http.Server

	closeOnce sync.Once
	closeErr  error
	serveDone chan struct{}
}

func (s *ProxyServer) Addr() string { return s.addr }
func (s *ProxyServer) URL() string  { return s.url }
func (s *ProxyServer) Sealed() bool { return s.sealed }

func (s *ProxyServer) Shutdown(ctx context.Context) error {
	s.closeOnce.Do(func() {
		err := s.httpSrv.Shutdown(ctx)
		<-s.serveDone
		if s.closeBackend && s.backend != nil {
			err = errors.Join(err, s.backend.Close())
		}
		s.closeErr = err
	})
	return s.closeErr
}

func (s *ProxyServer) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.Shutdown(ctx)
}

// StartProxyServer starts a loopback HTTP Go module proxy server.
func StartProxyServer(ctx context.Context, opts ProxyServerOptions) (*ProxyServer, error) {
	bindAddr, err := validateProxyBindAddr(opts.Addr, opts.AllowMeshBind)
	if err != nil {
		return nil, err
	}

	artRoot := strings.TrimSpace(opts.ArtifactRoot)
	if artRoot == "" && opts.Backend == nil {
		artRoot = artifact.DefaultRoot()
	}
	if artRoot != "" {
		if resolved, err := filepath.Abs(artRoot); err == nil {
			artRoot = resolved
		}
	}

	loomDir := strings.TrimSpace(opts.LoomRepoDir)
	if loomDir == "" {
		loomDir = DefaultLoomRepoDir(artRoot)
	}
	if resolved, err := filepath.Abs(loomDir); err == nil {
		loomDir = resolved
	}

	backend := opts.Backend
	closeBackend := opts.CloseBackendOnShutdown
	if backend == nil {
		backend, err = artifact.ConnectOrOpen(ctx, artRoot)
		if err != nil {
			return nil, err
		}
		closeBackend = true
	}

	hopts := opts
	hopts.ArtifactRoot = artRoot
	hopts.LoomRepoDir = loomDir
	hopts.Backend = backend
	handler, err := NewProxyHandler(hopts)
	if err != nil {
		if closeBackend {
			_ = backend.Close()
		}
		return nil, err
	}

	ln, err := net.Listen("tcp", bindAddr)
	if err != nil {
		if closeBackend {
			_ = backend.Close()
		}
		return nil, err
	}

	addr := ln.Addr().String()
	srv := &ProxyServer{
		addr:         addr,
		url:          "http://" + addr,
		sealed:       effectiveSealed(opts.Sealed, artRoot),
		backend:      backend,
		closeBackend: closeBackend,
		ln:           ln,
		httpSrv: &http.Server{
			Handler:           handler,
			ReadHeaderTimeout: 10 * time.Second,
		},
		serveDone: make(chan struct{}),
	}

	go func() {
		defer close(srv.serveDone)
		_ = srv.httpSrv.Serve(ln)
	}()

	return srv, nil
}

type proxyDiagKey struct{}

func setProxyDiag(ctx context.Context, err error) {
	if err == nil {
		return
	}
	if slot, ok := ctx.Value(proxyDiagKey{}).(*error); ok && slot != nil && *slot == nil {
		*slot = err
	}
}

type diagResponseWriter struct {
	http.ResponseWriter
	req     *http.Request
	diag    *error
	wrote   bool
	discard bool
}

func (w *diagResponseWriter) WriteHeader(statusCode int) {
	if w.wrote {
		return
	}
	w.wrote = true
	if w.diag != nil && *w.diag != nil && statusCode >= http.StatusBadRequest {
		w.Header().Set("Cache-Control", "no-store")
		if statusCode == http.StatusInternalServerError {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Del("Content-Length")
			w.ResponseWriter.WriteHeader(statusCode)
			if w.req.Method != http.MethodHead {
				_, _ = w.ResponseWriter.Write([]byte((*w.diag).Error()))
			}
			w.discard = true
			return
		}
	}
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *diagResponseWriter) Write(p []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	if w.discard {
		return len(p), nil
	}
	return w.ResponseWriter.Write(p)
}

// NewProxyHandler constructs an http.Handler wrapping github.com/goproxy/goproxy.Goproxy
// with CAS-backed Cacher, single-flight Loom .zip regeneration, and sealed-mode enforcement.
func NewProxyHandler(opts ProxyServerOptions) (http.Handler, error) {
	if opts.Backend == nil {
		return nil, errors.New("gomod proxy: Backend is required for NewProxyHandler")
	}
	loomDir := strings.TrimSpace(opts.LoomRepoDir)
	if loomDir == "" {
		loomDir = DefaultLoomRepoDir(opts.ArtifactRoot)
	}
	if resolved, err := filepath.Abs(loomDir); err == nil {
		loomDir = resolved
	}

	sealed := effectiveSealed(opts.Sealed, opts.ArtifactRoot)
	var upstream goproxy.Fetcher
	if sealed {
		upstream = sealedFetcher{}
	} else if opts.Fetcher != nil {
		upstream = opts.Fetcher
	} else {
		upstream = &goproxy.GoFetcher{MaxConcurrentDirectFetches: 4}
	}

	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	cacher := &casCacher{
		backend:      opts.Backend,
		loomDir:      loomDir,
		onRegenerate: opts.OnRegenerate,
	}
	fetcher := &catalogFetcher{
		backend:      opts.Backend,
		artifactRoot: opts.ArtifactRoot,
		sealed:       sealed,
		upstream:     upstream,
	}

	gp := &goproxy.Goproxy{
		Fetcher: fetcher,
		Cacher:  cacher,
		Logger:  logger,
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var diagErr error
		ctx := context.WithValue(r.Context(), proxyDiagKey{}, &diagErr)
		dw := &diagResponseWriter{
			ResponseWriter: w,
			req:            r,
			diag:           &diagErr,
		}
		gp.ServeHTTP(dw, r.WithContext(ctx))
	}), nil
}

// sealedDeniedError satisfies fs.ErrNotExist for goproxy HTTP 404 protocol
// signaling while wrapping artifact.ErrSealedNetworkDenied for errors.Is checks.
type sealedDeniedError struct {
	target string
}

func (e sealedDeniedError) Error() string {
	return fmt.Sprintf("%s: %s", artifact.ErrSealedNetworkDenied.Error(), e.target)
}

func (e sealedDeniedError) Unwrap() error { return artifact.ErrSealedNetworkDenied }

func (sealedDeniedError) Is(target error) bool { return target == fs.ErrNotExist }

type sealedFetcher struct{}

func (sealedFetcher) Query(ctx context.Context, modPath, query string) (string, time.Time, error) {
	err := sealedDeniedError{target: modPath + "@" + query}
	setProxyDiag(ctx, err)
	return "", time.Time{}, err
}

func (sealedFetcher) List(ctx context.Context, modPath string) ([]string, error) {
	err := sealedDeniedError{target: modPath}
	setProxyDiag(ctx, err)
	return nil, err
}

func (sealedFetcher) Download(ctx context.Context, modPath, version string) (io.ReadSeekCloser, io.ReadSeekCloser, io.ReadSeekCloser, error) {
	err := sealedDeniedError{target: modPath + "@" + version}
	setProxyDiag(ctx, err)
	return nil, nil, nil, err
}

type catalogFetcher struct {
	backend      artifact.Backend
	artifactRoot string
	sealed       bool
	upstream     goproxy.Fetcher
}

func (f *catalogFetcher) isSealed() bool {
	return f.sealed || effectiveSealed(false, f.artifactRoot)
}

func (f *catalogFetcher) List(ctx context.Context, modPath string) ([]string, error) {
	pubs, err := f.backend.ListPublications(ctx)
	if err != nil {
		return nil, err
	}
	var vers []string
	for _, pub := range pubs {
		if pub.Module == modPath {
			vers = append(vers, pub.Version)
		}
	}
	if cached := f.cachedPublicVersions(ctx, modPath); len(cached) > 0 {
		vers = append(vers, cached...)
	}
	if len(vers) > 0 {
		semver.Sort(vers)
		vers = slices.Compact(vers)
		return vers, nil
	}
	if f.isSealed() {
		return sealedFetcher{}.List(ctx, modPath)
	}
	return f.upstream.List(ctx, modPath)
}

func (f *catalogFetcher) Query(ctx context.Context, modPath, query string) (string, time.Time, error) {
	pubs, err := f.backend.ListPublications(ctx)
	if err != nil {
		return "", time.Time{}, err
	}
	var modPubs []artifact.PublicationRecord
	for _, pub := range pubs {
		if pub.Module == modPath {
			modPubs = append(modPubs, pub)
		}
	}
	if len(modPubs) > 0 {
		matched, ok := matchPublishedVersion(modPubs, query)
		if ok {
			t := f.readInfoTime(ctx, matched.InfoDigest, matched.PublishedAt)
			return matched.Version, t, nil
		}
		// Never fall back to network for an unknown query on a published internal module in sealed mode.
		if f.isSealed() {
			return sealedFetcher{}.Query(ctx, modPath, query)
		}
	}

	// Support cached public modules in sealed mode.
	if cachedVers := f.cachedPublicVersions(ctx, modPath); len(cachedVers) > 0 {
		if v, ok := matchVersionString(cachedVers, query); ok {
			t := f.readCachedInfoTime(ctx, modPath, v)
			return v, t, nil
		}
	}

	if f.isSealed() {
		return sealedFetcher{}.Query(ctx, modPath, query)
	}
	return f.upstream.Query(ctx, modPath, query)
}

func (f *catalogFetcher) Download(ctx context.Context, modPath, version string) (io.ReadSeekCloser, io.ReadSeekCloser, io.ReadSeekCloser, error) {
	// Defense-in-depth: an internal published module version must NEVER fall back to upstream.
	if _, err := f.backend.GetPublication(ctx, modPath, version); err == nil {
		missErr := fmt.Errorf("%w: %s@%s missing from local CAS without recovery", artifact.ErrMissingRecoveryInputs, modPath, version)
		setProxyDiag(ctx, missErr)
		return nil, nil, nil, missErr
	}
	if f.isSealed() {
		return sealedFetcher{}.Download(ctx, modPath, version)
	}
	return f.upstream.Download(ctx, modPath, version)
}

func (f *catalogFetcher) cachedPublicVersions(ctx context.Context, modPath string) []string {
	st, err := f.backend.LookupLogical(ctx, logicalPrefixCachedVersions+modPath, "list")
	if err != nil || !st.PayloadPresent {
		return nil
	}
	data, err := readBlobWithLease(ctx, f.backend, st.Record.Digest)
	if err != nil {
		return nil
	}
	vers := strings.Fields(string(data))
	semver.Sort(vers)
	return slices.Compact(vers)
}

func (f *catalogFetcher) readInfoTime(ctx context.Context, infoDigest string, fallback time.Time) time.Time {
	data, err := readBlobWithLease(ctx, f.backend, infoDigest)
	if err == nil {
		var info struct {
			Time time.Time `json:"Time"`
		}
		if json.Unmarshal(data, &info) == nil && !info.Time.IsZero() {
			return info.Time.UTC()
		}
	}
	if !fallback.IsZero() {
		return fallback.UTC()
	}
	return time.Unix(0, 0).UTC()
}

func (f *catalogFetcher) readCachedInfoTime(ctx context.Context, modPath, version string) time.Time {
	st, err := f.backend.LookupLogical(ctx, logicalPrefixInfo+modPath, version)
	if err == nil && st.PayloadPresent {
		return f.readInfoTime(ctx, st.Record.Digest, st.Record.CreatedAt)
	}
	return time.Unix(0, 0).UTC()
}

func matchPublishedVersion(pubs []artifact.PublicationRecord, query string) (artifact.PublicationRecord, bool) {
	sorted := slices.Clone(pubs)
	slices.SortFunc(sorted, func(a, b artifact.PublicationRecord) int {
		return semver.Compare(a.Version, b.Version)
	})
	if query == "latest" {
		var bestRelease *artifact.PublicationRecord
		for i := range sorted {
			if semver.Prerelease(sorted[i].Version) == "" {
				bestRelease = &sorted[i]
			}
		}
		if bestRelease != nil {
			return *bestRelease, true
		}
		return sorted[len(sorted)-1], true
	}
	for _, p := range sorted {
		if p.Version == query {
			return p, true
		}
	}
	if semver.IsValid(query) {
		var best *artifact.PublicationRecord
		for i := range sorted {
			v := sorted[i].Version
			if v == query || semver.MajorMinor(v) == query || semver.Major(v) == query {
				best = &sorted[i]
			}
		}
		if best != nil {
			return *best, true
		}
	}
	if isHexPrefix(query) {
		q := strings.ToLower(query)
		var best *artifact.PublicationRecord
		for i := range sorted {
			if strings.HasPrefix(strings.ToLower(sorted[i].Commit), q) {
				if best == nil || (semver.Prerelease(sorted[i].Version) == "" && semver.Prerelease(best.Version) != "") || semver.Compare(sorted[i].Version, best.Version) > 0 {
					best = &sorted[i]
				}
			}
		}
		if best != nil {
			return *best, true
		}
	}
	return artifact.PublicationRecord{}, false
}

func matchVersionString(vers []string, query string) (string, bool) {
	if len(vers) == 0 {
		return "", false
	}
	sorted := slices.Clone(vers)
	semver.Sort(sorted)
	if query == "latest" {
		best := ""
		for _, v := range sorted {
			if semver.Prerelease(v) == "" {
				best = v
			}
		}
		if best != "" {
			return best, true
		}
		return sorted[len(sorted)-1], true
	}
	for _, v := range sorted {
		if v == query {
			return v, true
		}
	}
	if semver.IsValid(query) {
		best := ""
		for _, v := range sorted {
			if v == query || semver.MajorMinor(v) == query || semver.Major(v) == query {
				best = v
			}
		}
		if best != "" {
			return best, true
		}
	}
	return "", false
}

func isHexPrefix(s string) bool {
	if len(s) < 7 || len(s) > 40 {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

type bytesReadSeekCloser struct {
	*bytes.Reader
}

func (bytesReadSeekCloser) Close() error { return nil }

func readBlobWithLease(ctx context.Context, backend artifact.Backend, dgst string) ([]byte, error) {
	var data []byte
	err := backend.WithReadLease(ctx, 0, "", defaultReadLeaseTTL, []string{dgst}, func(lctx context.Context) error {
		var fetchErr error
		data, fetchErr = backend.FetchVerified(lctx, dgst)
		return fetchErr
	})
	return data, err
}

// casCacher implements goproxy.Cacher over artifact.Backend with single-flight
// deterministic .zip reconstruction from durable Loom refs on cache eviction.
type casCacher struct {
	backend      artifact.Backend
	loomDir      string
	onRegenerate func(module, version string)

	sf      singleflight.Group
	indexMu sync.Mutex
}

var _ goproxy.Cacher = (*casCacher)(nil)

func parseProxyCacheName(name string) (modPath, version, ext string, ok bool) {
	escapedMod, after, found := strings.Cut(strings.TrimPrefix(name, "/"), "/@v/")
	if !found {
		return "", "", "", false
	}
	ext = path.Ext(after)
	switch ext {
	case ".info", ".mod", ".zip":
	default:
		return "", "", "", false
	}
	escapedVer := strings.TrimSuffix(after, ext)
	var err error
	modPath, err = module.UnescapePath(escapedMod)
	if err != nil {
		return "", "", "", false
	}
	version, err = module.UnescapeVersion(escapedVer)
	if err != nil {
		return "", "", "", false
	}
	return modPath, version, ext, true
}

func (c *casCacher) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	modPath, version, ext, ok := parseProxyCacheName(name)
	if !ok {
		return nil, fs.ErrNotExist
	}

	// 1. Check whether (modPath, version) is an immutable published internal module.
	pub, pubErr := c.backend.GetPublication(ctx, modPath, version)
	if pubErr == nil {
		rc, err := c.getPublishedArtifact(ctx, pub, ext)
		if err != nil {
			setProxyDiag(ctx, err)
			return nil, err
		}
		return rc, nil
	} else if !errors.Is(pubErr, artifact.ErrNotFound) {
		setProxyDiag(ctx, pubErr)
		return nil, pubErr
	}

	// 2. Otherwise check cached public module artifacts in CAS.
	logicalName := logicalNameForExt(ext, modPath)
	st, err := c.backend.LookupLogical(ctx, logicalName, version)
	if err != nil {
		if errors.Is(err, artifact.ErrNotFound) {
			return nil, fs.ErrNotExist
		}
		return nil, err
	}
	if !st.PayloadPresent {
		return nil, fs.ErrNotExist
	}
	data, err := readBlobWithLease(ctx, c.backend, st.Record.Digest)
	if err != nil {
		if errors.Is(err, artifact.ErrPayloadEvicted) || errors.Is(err, artifact.ErrNotFound) {
			return nil, fs.ErrNotExist
		}
		setProxyDiag(ctx, err)
		return nil, err
	}
	return bytesReadSeekCloser{bytes.NewReader(data)}, nil
}

func (c *casCacher) getPublishedArtifact(ctx context.Context, pub artifact.PublicationRecord, ext string) (io.ReadCloser, error) {
	switch ext {
	case ".info":
		data, err := readBlobWithLease(ctx, c.backend, pub.InfoDigest)
		if err != nil {
			// Format cause with %s so fs.ErrNotExist is NEVER wrapped, preventing upstream fallback.
			return nil, fmt.Errorf("%w: published .info missing for %s@%s (%s)", artifact.ErrMissingRecoveryInputs, pub.Module, pub.Version, err.Error())
		}
		return bytesReadSeekCloser{bytes.NewReader(data)}, nil

	case ".mod":
		data, err := readBlobWithLease(ctx, c.backend, pub.ModDigest)
		if err != nil {
			return nil, fmt.Errorf("%w: published .mod missing for %s@%s (%s)", artifact.ErrMissingRecoveryInputs, pub.Module, pub.Version, err.Error())
		}
		return bytesReadSeekCloser{bytes.NewReader(data)}, nil

	case ".zip":
		st, stErr := c.backend.GetRecordStatus(ctx, pub.ZipDigest)
		if stErr == nil && st.PayloadPresent {
			data, err := readBlobWithLease(ctx, c.backend, pub.ZipDigest)
			if err == nil {
				return bytesReadSeekCloser{bytes.NewReader(data)}, nil
			}
			if !errors.Is(err, artifact.ErrPayloadEvicted) {
				return nil, fmt.Errorf("%w: published .zip unreadable for %s@%s (%s)", artifact.ErrMissingRecoveryInputs, pub.Module, pub.Version, err.Error())
			}
		} else if stErr != nil && !errors.Is(stErr, artifact.ErrNotFound) {
			return nil, fmt.Errorf("%w: catalog lookup failed for %s@%s (%s)", artifact.ErrMissingRecoveryInputs, pub.Module, pub.Version, stErr.Error())
		}

		// Evicted .zip: coalesce concurrent requests via singleflight keyed by module@version.
		sfKey := pub.Module + "@" + pub.Version
		val, err, _ := c.sf.Do(sfKey, func() (any, error) {
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), regenTimeout)
			defer cancel()

			// Re-check under singleflight in case an immediately preceding flight just restored it.
			if freshSt, err := c.backend.GetRecordStatus(rctx, pub.ZipDigest); err == nil && freshSt.PayloadPresent {
				if data, rerr := readBlobWithLease(rctx, c.backend, pub.ZipDigest); rerr == nil {
					return data, nil
				}
			}

			return c.regeneratePublishedZip(rctx, pub)
		})
		if err != nil {
			return nil, err
		}
		zipBytes := val.([]byte)
		return bytesReadSeekCloser{bytes.NewReader(zipBytes)}, nil

	default:
		return nil, fs.ErrNotExist
	}
}

func (c *casCacher) regeneratePublishedZip(ctx context.Context, pub artifact.PublicationRecord) ([]byte, error) {
	if c.onRegenerate != nil {
		c.onRegenerate(pub.Module, pub.Version)
	}

	var inputDigests []string
	if st, err := c.backend.GetRecordStatus(ctx, pub.ZipDigest); err == nil && st.Record.Recipe != nil {
		inputDigests = slices.Clone(st.Record.Recipe.InputDigests)
		for _, inDgst := range inputDigests {
			if _, inErr := readBlobWithLease(ctx, c.backend, inDgst); inErr != nil {
				return nil, fmt.Errorf("%w: retained recipe input %s unavailable for %s@%s (%s)",
					artifact.ErrMissingRecoveryInputs, inDgst, pub.Module, pub.Version, inErr.Error())
			}
		}
	}

	// Open durable Loom bare repo and resolve immutable ref.
	// Never wrap os.ErrNotExist with %w so goproxy never treats recovery failure as a cache miss.
	loomRepo, err := gogit.PlainOpen(c.loomDir)
	if err != nil {
		return nil, fmt.Errorf("%w: open loom repository for %s@%s (%s)",
			artifact.ErrMissingRecoveryInputs, pub.Module, pub.Version, err.Error())
	}
	ref, err := loomRepo.Reference(plumbing.ReferenceName(pub.LoomRef), true)
	if err != nil {
		return nil, fmt.Errorf("%w: missing loom ref %s for %s@%s (%s)",
			artifact.ErrMissingRecoveryInputs, pub.LoomRef, pub.Module, pub.Version, err.Error())
	}
	if ref.Hash().String() != pub.Commit {
		return nil, fmt.Errorf("%w: loom ref %s points to %s, expected commit %s for %s@%s",
			artifact.ErrMissingRecoveryInputs, pub.LoomRef, ref.Hash().String(), pub.Commit, pub.Module, pub.Version)
	}

	commitObj, err := loomRepo.CommitObject(ref.Hash())
	if err != nil {
		return nil, fmt.Errorf("%w: load commit %s for %s@%s (%s)",
			artifact.ErrMissingRecoveryInputs, pub.Commit, pub.Module, pub.Version, err.Error())
	}

	rel := "."
	if pub.Subdir != "" {
		rel = pub.Subdir
	}
	_, files, err := commitFiles(commitObj, rel)
	if err != nil {
		return nil, fmt.Errorf("%w: read source tree %s (%s) for %s@%s (%s)",
			artifact.ErrMissingRecoveryInputs, pub.Commit, rel, pub.Module, pub.Version, err.Error())
	}
	files = filterNestedModulesFromCommitFiles(files)

	var zbuf bytes.Buffer
	mver := module.Version{Path: pub.Module, Version: pub.Version}
	if err := modzip.Create(&zbuf, mver, files); err != nil {
		return nil, fmt.Errorf("%w: rebuild module zip %s@%s (%s)",
			artifact.ErrMissingRecoveryInputs, pub.Module, pub.Version, err.Error())
	}
	zipBytes := bytes.Clone(zbuf.Bytes())

	// Verify BOTH sha256 CAS digest AND go.sum h1: hash before storing or serving!
	gotDigest := artifact.DigestBytes(zipBytes)
	if gotDigest != pub.ZipDigest {
		return nil, fmt.Errorf("%w: reconstructed zip digest %s != expected %s for %s@%s",
			artifact.ErrMissingRecoveryInputs, gotDigest, pub.ZipDigest, pub.Module, pub.Version)
	}
	gotH1, err := hashZipBytes(zipBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: compute h1 hash for %s@%s (%s)",
			artifact.ErrMissingRecoveryInputs, pub.Module, pub.Version, err.Error())
	}
	if gotH1 != pub.GoSumH1 {
		return nil, fmt.Errorf("%w: reconstructed zip h1 %s != expected %s for %s@%s",
			artifact.ErrMissingRecoveryInputs, gotH1, pub.GoSumH1, pub.Module, pub.Version)
	}

	zipRec := artifact.CatalogRecordV1{
		SchemaVersion:  artifact.CatalogRecordSchemaV1,
		Digest:         pub.ZipDigest,
		Size:           int64(len(zipBytes)),
		MediaType:      mediaTypeGoModZip,
		LogicalName:    logicalPrefixZip + pub.Module,
		LogicalVersion: pub.Version,
		Provenance:     "loom:" + pub.LoomRef,
		Retention:      artifact.RetentionReconstructible,
		Recipe: &artifact.RegenerationRecipe{
			Kind:           artifact.RecipeGoModZipV1,
			InputDigests:   inputDigests,
			LoomRef:        pub.LoomRef,
			Commit:         pub.Commit,
			Subdir:         pub.Subdir,
			ExpectedDigest: pub.ZipDigest,
			ExpectedGoSum:  pub.GoSumH1,
		},
	}
	if _, err := c.backend.PutArtifact(ctx, zipRec, bytes.NewReader(zipBytes)); err != nil {
		return nil, fmt.Errorf("%w: restore regenerated zip into CAS for %s@%s (%s)",
			artifact.ErrMissingRecoveryInputs, pub.Module, pub.Version, err.Error())
	}
	return zipBytes, nil
}

func (c *casCacher) Put(ctx context.Context, name string, content io.ReadSeeker) error {
	modPath, version, ext, ok := parseProxyCacheName(name)
	if !ok {
		return fmt.Errorf("gomod proxy: invalid cache path %q", name)
	}
	if _, err := content.Seek(0, io.SeekStart); err != nil {
		return err
	}
	data, err := io.ReadAll(content)
	if err != nil {
		return err
	}

	dgst := artifact.DigestBytes(data)
	rec := artifact.CatalogRecordV1{
		SchemaVersion:  artifact.CatalogRecordSchemaV1,
		Digest:         dgst,
		Size:           int64(len(data)),
		MediaType:      mediaTypeForExt(ext),
		LogicalName:    logicalNameForExt(ext, modPath),
		LogicalVersion: version,
		Provenance:     "gomod-proxy-cache",
		Retention:      artifact.RetentionProtectedRoot,
		RetentionRoots: []string{"gomod-cache:" + modPath + "@" + version},
	}
	if _, err := c.backend.PutArtifact(ctx, rec, bytes.NewReader(data)); err != nil {
		return err
	}

	if ext == ".zip" {
		if err := c.recordCachedPublicVersion(ctx, modPath, version); err != nil {
			return err
		}
	}
	return nil
}

func (c *casCacher) recordCachedPublicVersion(ctx context.Context, modPath, version string) error {
	c.indexMu.Lock()
	defer c.indexMu.Unlock()

	var vers []string
	if st, err := c.backend.LookupLogical(ctx, logicalPrefixCachedVersions+modPath, "list"); err == nil && st.PayloadPresent {
		if raw, rerr := readBlobWithLease(ctx, c.backend, st.Record.Digest); rerr == nil {
			vers = strings.Fields(string(raw))
		}
	}
	if !slices.Contains(vers, version) {
		vers = append(vers, version)
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
		Provenance:     "gomod-proxy-cache",
		Retention:      artifact.RetentionProtectedRoot,
		RetentionRoots: []string{"gomod-cache-list:" + modPath},
	}
	_, err := c.backend.PutArtifact(ctx, listRec, bytes.NewReader(listBytes))
	return err
}

func logicalNameForExt(ext, modPath string) string {
	switch ext {
	case ".info":
		return logicalPrefixInfo + modPath
	case ".mod":
		return logicalPrefixMod + modPath
	case ".zip":
		return logicalPrefixZip + modPath
	default:
		return "gomod-other/" + modPath
	}
}

func mediaTypeForExt(ext string) string {
	switch ext {
	case ".info":
		return mediaTypeGoModInfo
	case ".mod":
		return mediaTypeGoModFile
	case ".zip":
		return mediaTypeGoModZip
	default:
		return "application/octet-stream"
	}
}
