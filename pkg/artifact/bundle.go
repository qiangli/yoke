// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package artifact

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/mod/module"
	modzip "golang.org/x/mod/zip"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/oci"
)

const (
	bundleMetadataName     = "bundle-metadata.json"
	bundleArtifactMedia    = "application/vnd.bashy.portable-bundle.v1"
	maxBundleMetadataBytes = 32 << 20
	maxBundleEntryBytes    = 8 << 30
)

// BundleTarEntry pins the exact path, byte length, and SHA-256 of one member
// inside a portable self-recovery .tar archive.
type BundleTarEntry struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// BundleMetadataV1 is the leading header entry (bundle-metadata.json) of a
// portable recovery archive.
type BundleMetadataV1 struct {
	SchemaVersion  string              `json:"schema_version"`
	ExportedAt     string              `json:"exported_at"`
	Platform       string              `json:"platform"`
	Sealed         bool                `json:"sealed"`
	OCIRef         string              `json:"oci_ref"`
	OCIManifest    string              `json:"oci_manifest_digest"`
	Profile        ProfileV1           `json:"profile"`
	Manifest       ManifestV1          `json:"manifest"`
	Records        []CatalogRecordV1   `json:"records"`
	EvictedDigests []string            `json:"evicted_digests,omitempty"`
	Publications   []PublicationRecord `json:"publications,omitempty"`
	LoomRefs       map[string]string   `json:"loom_refs,omitempty"`
	Entries        []BundleTarEntry    `json:"entries"`
}

// ExportOptions configures ExportBundle.
type ExportOptions struct {
	Root       string
	LoomDir    string
	Backend    Backend
	OutputPath string
	Generation uint64
	Profile    *ProfileV1
	Now        func() time.Time
}

// ExportResult reports a completed portable bundle export.
type ExportResult struct {
	OutputPath   string `json:"output_path"`
	BundleSHA256 string `json:"bundle_sha256"`
	BundleSize   int64  `json:"bundle_size"`
	Platform     string `json:"platform"`
	Generation   uint64 `json:"generation"`
	OCIRef       string `json:"oci_ref"`
	BlobCount    int    `json:"blob_count"`
	LoomRefCount int    `json:"loom_ref_count"`
}

// ImportOptions configures ImportBundle.
type ImportOptions struct {
	Root                 string
	LoomDir              string
	Backend              Backend
	BundlePath           string
	Seal                 bool
	AllowForeignPlatform bool
	TargetPlatform       string // defaults to CurrentPlatform()
	DiskFreeFunc         func(string) (uint64, error)
	Now                  func() time.Time
	// Materialize runs after verified catalog/CAS/Loom commit and before optional seal activation.
	Materialize func(ctx context.Context, backend Backend, manifest ManifestV1) error
}

// ImportResult reports a completed verified portable bundle import.
type ImportResult struct {
	ArtifactRoot       string `json:"artifact_root"`
	LoomDir            string `json:"loom_dir"`
	Platform           string `json:"platform"`
	ImportedGeneration uint64 `json:"imported_generation"`
	BaselineGeneration uint64 `json:"baseline_generation,omitempty"`
	Sealed             bool   `json:"sealed"`
	ManifestDigest     string `json:"manifest_digest"`
	BlobsImported      int    `json:"blobs_imported"`
	Publications       int    `json:"publications"`
}

func safeBundleMemberPath(name string) bool {
	if !SafeLogicalPath(name) || strings.Contains(name, ":") {
		return false
	}
	return strings.HasPrefix(name, "oci/") || strings.HasPrefix(name, "loom/")
}

func copyGitObject(src, dst storer.EncodedObjectStorer, h plumbing.Hash) error {
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
		return fmt.Errorf("artifact: git object hash mismatch %s != %s", got, h)
	}
	return nil
}

func copyGitTreeRecursive(src, dst storer.EncodedObjectStorer, treeHash plumbing.Hash) error {
	if err := copyGitObject(src, dst, treeHash); err != nil {
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
			if err := copyGitTreeRecursive(src, dst, entry.Hash); err != nil {
				return err
			}
		default:
			if err := copyGitObject(src, dst, entry.Hash); err != nil {
				return err
			}
		}
	}
	return nil
}

func copyGitCommitClosure(src, dst storer.EncodedObjectStorer, commitHash plumbing.Hash) error {
	c, err := object.GetCommit(src, commitHash)
	if err != nil {
		return err
	}
	if err := copyGitTreeRecursive(src, dst, c.TreeHash); err != nil {
		return err
	}
	if err := copyGitObject(src, dst, commitHash); err != nil {
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

func openOrInitBareRepo(dir string) (*gogit.Repository, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if r, err := gogit.PlainOpen(dir); err == nil {
		return r, nil
	}
	r, err := gogit.PlainInit(dir, true)
	if err != nil {
		if errors.Is(err, gogit.ErrRepositoryAlreadyExists) {
			return gogit.PlainOpen(dir)
		}
		return nil, err
	}
	return r, nil
}

func fileSHA256AndSize(filePath string) (string, int64, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// ExportBundle stages a verified ORAS OCI layout + durable Loom bare repository
// for the target generation and writes a deterministic checksummed .tar archive.
func ExportBundle(ctx context.Context, opts ExportOptions) (*ExportResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(opts.OutputPath) == "" {
		return nil, errors.New("artifact: bundle output path is required")
	}
	outAbs, err := filepath.Abs(opts.OutputPath)
	if err != nil {
		return nil, err
	}

	root := resolveRoot(opts.Root)
	loomDir := strings.TrimSpace(opts.LoomDir)
	if loomDir == "" {
		loomDir = defaultLoomDirForRoot(root)
	} else if abs, err := filepath.Abs(loomDir); err == nil {
		loomDir = abs
	}

	sealSt, hasSealSt, err := LoadSealState(root)
	if err != nil {
		return nil, err
	}

	backend := opts.Backend
	closeBackend := false
	if backend == nil {
		var opened bool
		backend, opened, err = openReadBackendIfPresent(ctx, root)
		if err != nil {
			return nil, err
		}
		if !opened {
			return nil, fmt.Errorf("%w: no artifact catalog at %s", ErrNotFound, root)
		}
		closeBackend = true
	}
	defer func() {
		if closeBackend {
			_ = backend.Close()
		}
	}()

	// Resolve target ManifestV1.
	var manifest ManifestV1
	switch {
	case opts.Generation > 0:
		manifest, err = backend.GetManifest(ctx, opts.Generation)
	case hasSealSt && sealSt.ActiveGeneration > 0:
		manifest, err = backend.GetManifest(ctx, sealSt.ActiveGeneration)
		if errors.Is(err, ErrNotFound) {
			var all []ManifestV1
			all, err = backend.ListManifests(ctx)
			if err == nil && len(all) > 0 {
				slices.SortFunc(all, func(a, b ManifestV1) int {
					if a.Generation < b.Generation {
						return -1
					}
					if a.Generation > b.Generation {
						return 1
					}
					return 0
				})
				manifest = all[len(all)-1]
			}
		}
	default:
		var all []ManifestV1
		all, err = backend.ListManifests(ctx)
		if err == nil {
			if len(all) == 0 {
				err = fmt.Errorf("%w: no manifests in %s", ErrNotFound, root)
			} else {
				slices.SortFunc(all, func(a, b ManifestV1) int {
					if a.Generation < b.Generation {
						return -1
					}
					if a.Generation > b.Generation {
						return 1
					}
					return 0
				})
				manifest = all[len(all)-1]
			}
		}
	}
	if err != nil {
		return nil, err
	}
	if err := manifest.Validate(); err != nil {
		return nil, err
	}

	var sealPtr *SealStateV1
	if hasSealSt {
		sealPtr = &sealSt
	}
	profile, err := resolveEffectiveProfile(root, opts.Profile, "", &manifest, sealPtr)
	if err != nil {
		return nil, err
	}

	pubs, err := backend.ListPublications(ctx)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(pubs, func(a, b PublicationRecord) int {
		if c := strings.Compare(a.Module, b.Module); c != 0 {
			return c
		}
		return strings.Compare(a.Version, b.Version)
	})

	var allRecordStatuses []RecordStatus
	if rl, ok := backend.(interface {
		ListRecords(context.Context) ([]RecordStatus, error)
	}); ok {
		allRecordStatuses, err = rl.ListRecords(ctx)
		if err != nil {
			return nil, err
		}
	}

	// Build staging directory outside CAS.
	if err := os.MkdirAll(filepath.Dir(outAbs), 0o755); err != nil {
		return nil, err
	}
	stageDir, err := os.MkdirTemp(filepath.Dir(outAbs), ".bundle-export-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(stageDir)

	stageOCIDir := filepath.Join(stageDir, "oci")
	if err := os.MkdirAll(stageOCIDir, 0o700); err != nil {
		return nil, err
	}
	stageOCI, err := oci.NewWithContext(ctx, stageOCIDir)
	if err != nil {
		return nil, err
	}
	stageOCI.AutoSaveIndex = true
	stageOCI.AutoGC = false

	// Collect all required non-evicted digests and catalog records.
	type blobSpec struct {
		dgst      string
		mediaType string
	}
	blobMap := make(map[string]blobSpec)
	addBlob := func(dgst, mediaType string) {
		if !ValidDigest(dgst) {
			return
		}
		if mediaType == "" {
			mediaType = defaultMediaType
		}
		if _, exists := blobMap[dgst]; !exists {
			blobMap[dgst] = blobSpec{dgst: dgst, mediaType: mediaType}
		}
	}

	addBlob(manifest.ProfileDigest, "application/json")
	addBlob(manifest.ProductOverlaySHA, defaultMediaType)
	if manifest.BuildImageDigest != "" {
		addBlob(manifest.BuildImageDigest, defaultMediaType)
	}
	for _, r := range manifest.Roots {
		addBlob(r.Digest, r.MediaType)
		for _, dep := range r.Deps {
			addBlob(dep, defaultMediaType)
		}
	}

	evictedSet := make(map[string]bool)
	var records []CatalogRecordV1
	for _, rs := range allRecordStatuses {
		if rs.Record.Retention == RetentionScratch {
			continue
		}
		records = append(records, rs.Record)
		if rs.PayloadPresent {
			addBlob(rs.Record.Digest, rs.Record.MediaType)
		} else {
			evictedSet[rs.Record.Digest] = true
		}
		for _, dep := range rs.Record.Dependencies {
			addBlob(dep, defaultMediaType)
		}
		if rs.Record.Recipe != nil {
			for _, in := range rs.Record.Recipe.InputDigests {
				addBlob(in, defaultMediaType)
			}
		}
	}

	for _, pub := range pubs {
		addBlob(pub.InfoDigest, "application/json")
		addBlob(pub.ModDigest, "text/plain; charset=utf-8")
		if !evictedSet[pub.ZipDigest] {
			addBlob(pub.ZipDigest, "application/zip")
		}
	}

	digests := make([]string, 0, len(blobMap))
	for d := range blobMap {
		digests = append(digests, d)
	}
	slices.Sort(digests)

	// Push verified blobs into stageOCI and assemble layer descriptors.
	layers := make([]ocispec.Descriptor, 0, len(digests))
	for _, dgst := range digests {
		spec := blobMap[dgst]
		// Ensure ProfileDigest is materialized even if caller never stored profile bytes separately.
		rc, openErr := backend.OpenBlob(ctx, dgst)
		if openErr != nil && dgst == manifest.ProfileDigest {
			if pb, pErr := profile.CanonicalBytes(); pErr == nil && DigestBytes(pb) == dgst {
				rc = io.NopCloser(bytes.NewReader(pb))
				openErr = nil
			}
		}
		if openErr != nil {
			return nil, fmt.Errorf("artifact: export missing required blob %s: %w", dgst, openErr)
		}

		tmpBlob, err := os.CreateTemp(stageDir, ".blob-*")
		if err != nil {
			_ = rc.Close()
			return nil, err
		}
		h := sha256.New()
		sz, copyErr := io.Copy(io.MultiWriter(tmpBlob, h), rc)
		closeRC := rc.Close()
		if copyErr != nil {
			_ = tmpBlob.Close()
			_ = os.Remove(tmpBlob.Name())
			return nil, fmt.Errorf("artifact: read blob %s: %w", dgst, copyErr)
		}
		if closeRC != nil {
			_ = tmpBlob.Close()
			_ = os.Remove(tmpBlob.Name())
			return nil, closeRC
		}
		if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); got != dgst {
			_ = tmpBlob.Close()
			_ = os.Remove(tmpBlob.Name())
			return nil, fmt.Errorf("%w: export %s hashed to %s", ErrCorruptPayload, dgst, got)
		}
		if _, err := tmpBlob.Seek(0, io.SeekStart); err != nil {
			_ = tmpBlob.Close()
			_ = os.Remove(tmpBlob.Name())
			return nil, err
		}

		desc := ocispec.Descriptor{
			MediaType: spec.mediaType,
			Digest:    digest.Digest(dgst),
			Size:      sz,
		}
		if err := stageOCI.Push(ctx, desc, tmpBlob); err != nil {
			_ = tmpBlob.Close()
			_ = os.Remove(tmpBlob.Name())
			return nil, fmt.Errorf("artifact: stage oci push %s: %w", dgst, err)
		}
		_ = tmpBlob.Close()
		_ = os.Remove(tmpBlob.Name())
		layers = append(layers, desc)
	}

	ociRef := fmt.Sprintf("gen-%d", manifest.Generation)
	packedDesc, err := oras.PackManifest(ctx, stageOCI, oras.PackManifestVersion1_1, bundleArtifactMedia, oras.PackManifestOptions{
		Layers: layers,
		ManifestAnnotations: map[string]string{
			ocispec.AnnotationCreated: manifest.CreatedAt,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("artifact: pack oci manifest: %w", err)
	}
	if err := stageOCI.Tag(ctx, packedDesc, ociRef); err != nil {
		return nil, fmt.Errorf("artifact: tag oci manifest %s: %w", ociRef, err)
	}
	if err := stageOCI.SaveIndex(); err != nil {
		return nil, fmt.Errorf("artifact: save staged oci index: %w", err)
	}

	// Stage durable Loom bare repository refs & objects for refs/bashy/published/...
	loomRefs := make(map[string]string)
	stageLoomDir := filepath.Join(stageDir, "loom")
	if len(pubs) > 0 {
		srcLoom, err := gogit.PlainOpen(loomDir)
		if err != nil {
			return nil, fmt.Errorf("artifact: open loom repo %s for export: %w", loomDir, err)
		}
		dstLoom, err := openOrInitBareRepo(stageLoomDir)
		if err != nil {
			return nil, fmt.Errorf("artifact: init staged loom repo: %w", err)
		}
		for _, pub := range pubs {
			ref, err := srcLoom.Reference(plumbing.ReferenceName(pub.LoomRef), true)
			if err != nil {
				return nil, fmt.Errorf("artifact: missing loom ref %s for %s@%s: %w", pub.LoomRef, pub.Module, pub.Version, err)
			}
			if ref.Hash().String() != pub.Commit {
				return nil, fmt.Errorf("artifact: loom ref %s commit %s != %s", pub.LoomRef, ref.Hash().String(), pub.Commit)
			}
			if err := copyGitCommitClosure(srcLoom.Storer, dstLoom.Storer, ref.Hash()); err != nil {
				return nil, fmt.Errorf("artifact: stage loom commit %s: %w", pub.Commit, err)
			}
			if err := dstLoom.Storer.SetReference(plumbing.NewHashReference(plumbing.ReferenceName(pub.LoomRef), ref.Hash())); err != nil {
				return nil, err
			}
			loomRefs[pub.LoomRef] = pub.Commit
		}
	} else if srcLoom, err := gogit.PlainOpen(loomDir); err == nil {
		if iter, rErr := srcLoom.References(); rErr == nil {
			var dstLoom *gogit.Repository
			_ = iter.ForEach(func(ref *plumbing.Reference) error {
				if ref.Type() != plumbing.HashReference || !strings.HasPrefix(ref.Name().String(), "refs/bashy/published/") {
					return nil
				}
				if dstLoom == nil {
					var initErr error
					dstLoom, initErr = openOrInitBareRepo(stageLoomDir)
					if initErr != nil {
						return initErr
					}
				}
				if err := copyGitCommitClosure(srcLoom.Storer, dstLoom.Storer, ref.Hash()); err != nil {
					return err
				}
				if err := dstLoom.Storer.SetReference(plumbing.NewHashReference(ref.Name(), ref.Hash())); err != nil {
					return err
				}
				loomRefs[ref.Name().String()] = ref.Hash().String()
				return nil
			})
		}
	}

	// Collect all staged regular files under oci/ and loom/ in deterministic sorted order.
	var entries []BundleTarEntry
	for _, sub := range []string{"oci", "loom"} {
		subRoot := filepath.Join(stageDir, sub)
		if _, err := os.Stat(subRoot); errors.Is(err, os.ErrNotExist) {
			continue
		}
		err := filepath.WalkDir(subRoot, func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("artifact: unexpected non-regular file in bundle stage: %s", p)
			}
			rel, err := filepath.Rel(stageDir, p)
			if err != nil {
				return err
			}
			relSlash := filepath.ToSlash(rel)
			if !safeBundleMemberPath(relSlash) {
				return fmt.Errorf("artifact: unsafe staged bundle path %q", relSlash)
			}
			sum, sz, err := fileSHA256AndSize(p)
			if err != nil {
				return err
			}
			entries = append(entries, BundleTarEntry{
				Path:   relSlash,
				Size:   sz,
				SHA256: sum,
			})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	slices.SortFunc(entries, func(a, b BundleTarEntry) int {
		return strings.Compare(a.Path, b.Path)
	})

	var evictedList []string
	for d := range evictedSet {
		evictedList = append(evictedList, d)
	}
	slices.Sort(evictedList)

	now := time.Now().UTC()
	if opts.Now != nil {
		now = opts.Now().UTC()
	}

	meta := BundleMetadataV1{
		SchemaVersion:  PortableBundleSchemaV1,
		ExportedAt:     now.Format(time.RFC3339),
		Platform:       manifest.Platform,
		Sealed:         IsSealed(root),
		OCIRef:         ociRef,
		OCIManifest:    packedDesc.Digest.String(),
		Profile:        profile,
		Manifest:       manifest,
		Records:        records,
		EvictedDigests: evictedList,
		Publications:   pubs,
		LoomRefs:       loomRefs,
		Entries:        entries,
	}
	metaBytes, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return nil, err
	}
	metaBytes = append(metaBytes, '\n')

	tmpOut, err := os.CreateTemp(filepath.Dir(outAbs), ".bundle-tar-*")
	if err != nil {
		return nil, err
	}
	tmpOutName := tmpOut.Name()
	cleanupOut := true
	defer func() {
		_ = tmpOut.Close()
		if cleanupOut {
			_ = os.Remove(tmpOutName)
		}
	}()

	bundleHasher := sha256.New()
	mw := io.MultiWriter(tmpOut, bundleHasher)
	tw := tar.NewWriter(mw)
	fixedTime := time.Unix(0, 0).UTC()

	if err := tw.WriteHeader(&tar.Header{
		Name:     bundleMetadataName,
		Typeflag: tar.TypeReg,
		Mode:     0o600,
		Size:     int64(len(metaBytes)),
		ModTime:  fixedTime,
	}); err != nil {
		return nil, err
	}
	if _, err := tw.Write(metaBytes); err != nil {
		return nil, err
	}

	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := tw.WriteHeader(&tar.Header{
			Name:     entry.Path,
			Typeflag: tar.TypeReg,
			Mode:     0o600,
			Size:     entry.Size,
			ModTime:  fixedTime,
		}); err != nil {
			return nil, err
		}
		srcFile, err := os.Open(filepath.Join(stageDir, filepath.FromSlash(entry.Path)))
		if err != nil {
			return nil, err
		}
		eh := sha256.New()
		n, copyErr := io.Copy(io.MultiWriter(tw, eh), srcFile)
		_ = srcFile.Close()
		if copyErr != nil {
			return nil, copyErr
		}
		if n != entry.Size || hex.EncodeToString(eh.Sum(nil)) != entry.SHA256 {
			return nil, fmt.Errorf("artifact: staged file %s mutated during tar write", entry.Path)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	stOut, err := tmpOut.Stat()
	if err != nil {
		return nil, err
	}
	bundleSize := stOut.Size()
	if err := tmpOut.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(tmpOutName, outAbs); err != nil {
		return nil, err
	}
	cleanupOut = false

	return &ExportResult{
		OutputPath:   outAbs,
		BundleSHA256: "sha256:" + hex.EncodeToString(bundleHasher.Sum(nil)),
		BundleSize:   bundleSize,
		Platform:     manifest.Platform,
		Generation:   manifest.Generation,
		OCIRef:       ociRef,
		BlobCount:    len(layers),
		LoomRefCount: len(loomRefs),
	}, nil
}

func reconstructZipFromLoomRepo(repo *gogit.Repository, pub PublicationRecord) ([]byte, error) {
	ref, err := repo.Reference(plumbing.ReferenceName(pub.LoomRef), true)
	if err != nil {
		return nil, err
	}
	if ref.Hash().String() != pub.Commit {
		return nil, fmt.Errorf("loom ref %s mismatch", pub.LoomRef)
	}
	c, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return nil, err
	}
	tree, err := c.Tree()
	if err != nil {
		return nil, err
	}
	if pub.Subdir != "" && pub.Subdir != "." {
		tree, err = tree.Tree(pub.Subdir)
		if err != nil {
			return nil, err
		}
	}
	var rawFiles []*object.File
	nested := map[string]bool{}
	if err := tree.Files().ForEach(func(f *object.File) error {
		if f.Mode != filemode.Regular && f.Mode != filemode.Executable && f.Mode != filemode.Deprecated {
			return nil
		}
		if f.Name != "go.mod" && path.Base(f.Name) == "go.mod" {
			nested[path.Dir(f.Name)] = true
		}
		rawFiles = append(rawFiles, f)
		return nil
	}); err != nil {
		return nil, err
	}
	var zfiles []modzip.File
	for _, f := range rawFiles {
		skip := false
		for nd := range nested {
			if f.Name == nd || strings.HasPrefix(f.Name, nd+"/") {
				skip = true
				break
			}
		}
		if !skip {
			zfiles = append(zfiles, loomTreeZipFile{f})
		}
	}
	var buf bytes.Buffer
	if err := modzip.Create(&buf, module.Version{Path: pub.Module, Version: pub.Version}, zfiles); err != nil {
		return nil, err
	}
	out := buf.Bytes()
	if DigestBytes(out) != pub.ZipDigest {
		return nil, fmt.Errorf("%w: reconstructed zip digest %s != %s", ErrCorruptPayload, DigestBytes(out), pub.ZipDigest)
	}
	return out, nil
}

type loomTreeZipFile struct {
	f *object.File
}

func (zf loomTreeZipFile) Path() string { return zf.f.Name }
func (zf loomTreeZipFile) Lstat() (os.FileInfo, error) {
	return loomTreeFileInfo{name: path.Base(zf.f.Name), size: zf.f.Size}, nil
}
func (zf loomTreeZipFile) Open() (io.ReadCloser, error) { return zf.f.Reader() }

type loomTreeFileInfo struct {
	name string
	size int64
}

func (fi loomTreeFileInfo) Name() string       { return fi.name }
func (fi loomTreeFileInfo) Size() int64        { return fi.size }
func (fi loomTreeFileInfo) Mode() os.FileMode  { return 0o644 }
func (fi loomTreeFileInfo) ModTime() time.Time { return time.Time{} }
func (fi loomTreeFileInfo) IsDir() bool        { return false }
func (fi loomTreeFileInfo) Sys() any           { return nil }

// ImportBundle stages and cryptographically verifies a portable self-recovery
// archive BEFORE mutating the destination store, Loom repository, or seal.json.
func ImportBundle(ctx context.Context, opts ImportOptions) (*ImportResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(opts.BundlePath) == "" {
		return nil, errors.New("artifact: bundle path is required")
	}

	root := resolveRoot(opts.Root)
	loomDir := strings.TrimSpace(opts.LoomDir)
	if loomDir == "" {
		loomDir = defaultLoomDirForRoot(root)
	} else if abs, err := filepath.Abs(loomDir); err == nil {
		loomDir = abs
	}

	targetPlatform := strings.TrimSpace(opts.TargetPlatform)
	if targetPlatform == "" {
		targetPlatform = CurrentPlatform()
	}

	f, err := os.Open(opts.BundlePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Create isolated staging directory without creating <root>/catalog.db or mutating existing state.
	stageParent := os.TempDir()
	if fi, statErr := os.Stat(root); statErr == nil && fi.IsDir() {
		stageParent = root
	}
	stageDir, err := os.MkdirTemp(stageParent, ".import-stage-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(stageDir)

	tr := tar.NewReader(f)
	firstHdr, err := tr.Next()
	if err != nil {
		return nil, fmt.Errorf("artifact: read bundle header: %w", err)
	}
	if firstHdr.Name != bundleMetadataName || firstHdr.Typeflag != tar.TypeReg {
		return nil, fmt.Errorf("artifact: bundle must start with regular %s, got %q (type %v)", bundleMetadataName, firstHdr.Name, firstHdr.Typeflag)
	}
	if firstHdr.Size <= 0 || firstHdr.Size > maxBundleMetadataBytes {
		return nil, fmt.Errorf("artifact: invalid %s size %d", bundleMetadataName, firstHdr.Size)
	}

	metaRaw, err := io.ReadAll(io.LimitReader(tr, maxBundleMetadataBytes+1))
	if err != nil {
		return nil, fmt.Errorf("artifact: read %s: %w", bundleMetadataName, err)
	}
	if int64(len(metaRaw)) != firstHdr.Size {
		return nil, fmt.Errorf("%w: truncated %s", ErrCorruptPayload, bundleMetadataName)
	}

	var meta BundleMetadataV1
	if err := json.Unmarshal(metaRaw, &meta); err != nil {
		return nil, fmt.Errorf("artifact: decode %s: %w", bundleMetadataName, err)
	}
	if meta.SchemaVersion != PortableBundleSchemaV1 {
		return nil, fmt.Errorf("artifact: unsupported bundle schema %q", meta.SchemaVersion)
	}
	if !opts.AllowForeignPlatform && meta.Platform != targetPlatform {
		return nil, fmt.Errorf("%w: bundle platform %q != host platform %q", ErrPlatformMismatch, meta.Platform, targetPlatform)
	}
	if err := meta.Profile.Validate(); err != nil {
		return nil, fmt.Errorf("artifact: invalid bundle profile: %w", err)
	}
	if err := meta.Manifest.Validate(); err != nil {
		return nil, fmt.Errorf("artifact: invalid bundle manifest: %w", err)
	}
	if meta.Manifest.Platform != meta.Platform {
		return nil, fmt.Errorf("%w: manifest platform %q != bundle platform %q", ErrPlatformMismatch, meta.Manifest.Platform, meta.Platform)
	}
	for _, rec := range meta.Records {
		if err := rec.Validate(); err != nil {
			return nil, fmt.Errorf("artifact: invalid bundle record %s: %w", rec.Digest, err)
		}
	}
	for _, pub := range meta.Publications {
		if err := pub.Validate(); err != nil {
			return nil, fmt.Errorf("artifact: invalid bundle publication %s@%s: %w", pub.Module, pub.Version, err)
		}
	}

	expectedEntries := make(map[string]BundleTarEntry, len(meta.Entries))
	var totalStageBytes uint64
	for _, e := range meta.Entries {
		if !safeBundleMemberPath(e.Path) {
			return nil, fmt.Errorf("artifact: unsafe bundle manifest path %q", e.Path)
		}
		rawSum, decErr := hex.DecodeString(e.SHA256)
		if decErr != nil || len(rawSum) != 32 || e.Size < 0 || e.Size > maxBundleEntryBytes {
			return nil, fmt.Errorf("artifact: invalid bundle entry metadata for %q", e.Path)
		}
		if _, dup := expectedEntries[e.Path]; dup {
			return nil, fmt.Errorf("artifact: duplicate bundle entry %q", e.Path)
		}
		expectedEntries[e.Path] = e
		totalStageBytes += uint64(e.Size)
	}

	// Pre-flight capacity reserve check BEFORE unpacking large layers or opening destination DB.
	reserveBytes := meta.Profile.Capacity.RebuildReserveBytes
	if meta.Manifest.RebuildReserveBytes > reserveBytes {
		reserveBytes = meta.Manifest.RebuildReserveBytes
	}
	if _, err := CheckCapacityReserve(root, totalStageBytes, reserveBytes, opts.DiskFreeFunc); err != nil {
		return nil, err
	}

	// Unpack and SHA-256 verify every tar entry inside stageDir; reject symlinks, special files, traversal, duplicates, and truncation.
	seenEntries := make(map[string]bool, len(expectedEntries))
	cleanStage := filepath.Clean(stageDir)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: corrupt tar stream: %v", ErrCorruptPayload, err)
		}
		if hdr.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("artifact: forbidden non-regular tar member %q (type %v, link %q)", hdr.Name, hdr.Typeflag, hdr.Linkname)
		}
		if !safeBundleMemberPath(hdr.Name) {
			return nil, fmt.Errorf("artifact: unsafe tar member path %q", hdr.Name)
		}
		exp, ok := expectedEntries[hdr.Name]
		if !ok {
			return nil, fmt.Errorf("artifact: undeclared tar member %q", hdr.Name)
		}
		if seenEntries[hdr.Name] {
			return nil, fmt.Errorf("artifact: duplicate tar member %q", hdr.Name)
		}
		if hdr.Size != exp.Size {
			return nil, fmt.Errorf("%w: tar member %q size %d != declared %d", ErrCorruptPayload, hdr.Name, hdr.Size, exp.Size)
		}

		dstPath := filepath.Join(cleanStage, filepath.FromSlash(hdr.Name))
		if dstPath != cleanStage && !strings.HasPrefix(dstPath, cleanStage+string(filepath.Separator)) {
			return nil, fmt.Errorf("artifact: tar member escapes staging dir: %q", hdr.Name)
		}
		if err := os.MkdirAll(filepath.Dir(dstPath), 0o700); err != nil {
			return nil, err
		}
		outFile, err := os.OpenFile(dstPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		h := sha256.New()
		n, copyErr := io.Copy(io.MultiWriter(outFile, h), io.LimitReader(tr, exp.Size+1))
		closeErr := outFile.Close()
		if copyErr != nil {
			return nil, fmt.Errorf("%w: read tar member %q: %v", ErrCorruptPayload, hdr.Name, copyErr)
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if n != exp.Size {
			return nil, fmt.Errorf("%w: truncated tar member %q (%d != %d)", ErrCorruptPayload, hdr.Name, n, exp.Size)
		}
		if gotHex := hex.EncodeToString(h.Sum(nil)); gotHex != exp.SHA256 {
			return nil, fmt.Errorf("%w: tar member %q sha256 %s != %s", ErrCorruptPayload, hdr.Name, gotHex, exp.SHA256)
		}
		seenEntries[hdr.Name] = true
	}
	if len(seenEntries) != len(expectedEntries) {
		return nil, fmt.Errorf("%w: truncated bundle tar (%d/%d entries present)", ErrCorruptPayload, len(seenEntries), len(expectedEntries))
	}

	// Verify ORAS OCI layout and copy DAG into a verified staging OCI target via oras.Copy.
	stageOCIDir := filepath.Join(cleanStage, "oci")
	srcOCI, err := oci.NewWithContext(ctx, stageOCIDir)
	if err != nil {
		return nil, fmt.Errorf("artifact: open staged oci layout: %w", err)
	}
	verifiedOCIDir := filepath.Join(cleanStage, "oci-verified")
	if err := os.MkdirAll(verifiedOCIDir, 0o700); err != nil {
		return nil, err
	}
	verifiedOCI, err := oci.NewWithContext(ctx, verifiedOCIDir)
	if err != nil {
		return nil, err
	}
	copiedRoot, err := oras.Copy(ctx, srcOCI, meta.OCIRef, verifiedOCI, meta.OCIRef, oras.DefaultCopyOptions)
	if err != nil {
		return nil, fmt.Errorf("%w: oras verify/copy %s failed: %v", ErrCorruptPayload, meta.OCIRef, err)
	}
	if meta.OCIManifest != "" && copiedRoot.Digest.String() != meta.OCIManifest {
		return nil, fmt.Errorf("%w: oci manifest digest %s != %s", ErrCorruptPayload, copiedRoot.Digest.String(), meta.OCIManifest)
	}

	evictedSet := make(map[string]bool, len(meta.EvictedDigests))
	for _, d := range meta.EvictedDigests {
		evictedSet[d] = true
	}

	// Verify EVERY required root, dependency, non-evicted record, and publication blob in verifiedOCI.
	requiredBlobs := make(map[string]int64)
	requireBlob := func(dgst string, sz int64) {
		if ValidDigest(dgst) {
			if _, ok := requiredBlobs[dgst]; !ok || sz >= 0 {
				requiredBlobs[dgst] = sz
			}
		}
	}
	requireBlob(meta.Manifest.ProfileDigest, -1)
	requireBlob(meta.Manifest.ProductOverlaySHA, -1)
	if meta.Manifest.BuildImageDigest != "" {
		requireBlob(meta.Manifest.BuildImageDigest, -1)
	}
	for _, r := range meta.Manifest.Roots {
		requireBlob(r.Digest, r.Size)
		for _, dep := range r.Deps {
			requireBlob(dep, -1)
		}
	}
	for _, rec := range meta.Records {
		if !evictedSet[rec.Digest] {
			requireBlob(rec.Digest, rec.Size)
		}
		for _, dep := range rec.Dependencies {
			requireBlob(dep, -1)
		}
		if rec.Recipe != nil {
			for _, in := range rec.Recipe.InputDigests {
				requireBlob(in, -1)
			}
		}
	}
	for _, pub := range meta.Publications {
		requireBlob(pub.InfoDigest, -1)
		requireBlob(pub.ModDigest, -1)
		if !evictedSet[pub.ZipDigest] {
			requireBlob(pub.ZipDigest, -1)
		}
	}

	blobDescs := make(map[string]ocispec.Descriptor, len(requiredBlobs))
	manifestBytes, err := func() ([]byte, error) {
		rc, err := verifiedOCI.Fetch(ctx, copiedRoot)
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(rc)
	}()
	if err != nil {
		return nil, fmt.Errorf("%w: read oci manifest: %v", ErrCorruptPayload, err)
	}
	var ociMan ocispec.Manifest
	if err := json.Unmarshal(manifestBytes, &ociMan); err != nil {
		return nil, fmt.Errorf("%w: decode oci manifest: %v", ErrCorruptPayload, err)
	}
	for _, layer := range ociMan.Layers {
		blobDescs[layer.Digest.String()] = layer
	}

	sortedRequired := make([]string, 0, len(requiredBlobs))
	for d := range requiredBlobs {
		sortedRequired = append(sortedRequired, d)
	}
	slices.Sort(sortedRequired)

	for _, dgst := range sortedRequired {
		desc, ok := blobDescs[dgst]
		if !ok {
			return nil, fmt.Errorf("%w: required blob %s missing from bundle OCI manifest", ErrIncompleteClosure, dgst)
		}
		wantSz := requiredBlobs[dgst]
		if wantSz >= 0 && desc.Size != wantSz {
			return nil, fmt.Errorf("%w: blob %s size %d != %d", ErrCorruptPayload, dgst, desc.Size, wantSz)
		}
		rc, err := verifiedOCI.Fetch(ctx, desc)
		if err != nil {
			return nil, fmt.Errorf("%w: fetch staged blob %s: %v", ErrCorruptPayload, dgst, err)
		}
		h := sha256.New()
		n, copyErr := io.Copy(h, rc)
		_ = rc.Close()
		if copyErr != nil {
			return nil, fmt.Errorf("%w: verify staged blob %s: %v", ErrCorruptPayload, dgst, copyErr)
		}
		if n != desc.Size || "sha256:"+hex.EncodeToString(h.Sum(nil)) != dgst {
			return nil, fmt.Errorf("%w: staged blob %s digest/size mismatch", ErrCorruptPayload, dgst)
		}
	}

	// Verify staged Loom repository and check destination Loom / Publication conflicts BEFORE any destination mutation.
	var stagedLoom *gogit.Repository
	stageLoomDir := filepath.Join(cleanStage, "loom")
	if len(meta.Publications) > 0 || len(meta.LoomRefs) > 0 {
		stagedLoom, err = gogit.PlainOpen(stageLoomDir)
		if err != nil {
			return nil, fmt.Errorf("%w: open staged loom repository: %v", ErrIncompleteClosure, err)
		}
		for _, pub := range meta.Publications {
			if err := verifyLoomPublicationCommit(stageLoomDir, pub); err != nil {
				return nil, fmt.Errorf("%w: staged loom verification failed for %s@%s: %v", ErrIncompleteClosure, pub.Module, pub.Version, err)
			}
		}
		for refName, commitHex := range meta.LoomRefs {
			if !strings.HasPrefix(refName, "refs/bashy/published/") || len(commitHex) != 40 {
				return nil, fmt.Errorf("artifact: invalid loom ref %q -> %q in bundle", refName, commitHex)
			}
			ref, err := stagedLoom.Reference(plumbing.ReferenceName(refName), true)
			if err != nil || ref.Hash().String() != commitHex {
				return nil, fmt.Errorf("%w: staged loom ref %s mismatch", ErrCorruptPayload, refName)
			}
		}
		// Pre-check existing destination Loom repo for immutable ref conflicts before writing!
		if existingLoom, openErr := gogit.PlainOpen(loomDir); openErr == nil {
			for refName, commitHex := range meta.LoomRefs {
				if exRef, rErr := existingLoom.Reference(plumbing.ReferenceName(refName), true); rErr == nil {
					if exRef.Hash().String() != commitHex {
						return nil, fmt.Errorf("%w: loom ref %s points to %s, bundle has %s",
							ErrImmutablePublicationConflict, refName, exRef.Hash().String(), commitHex)
					}
				}
			}
		}
	}

	// Pre-check existing destination Backend (read-only if not yet opened) for immutable publication conflicts!
	if opts.Backend != nil {
		for _, pub := range meta.Publications {
			if existingPub, pErr := opts.Backend.GetPublication(ctx, pub.Module, pub.Version); pErr == nil {
				if _, chkErr := CheckImmutablePublication(existingPub, pub); chkErr != nil {
					return nil, chkErr
				}
			}
		}
	} else if roBack, opened, roErr := openReadBackendIfPresent(ctx, root); roErr != nil {
		return nil, roErr
	} else if opened {
		for _, pub := range meta.Publications {
			if existingPub, pErr := roBack.GetPublication(ctx, pub.Module, pub.Version); pErr == nil {
				if _, chkErr := CheckImmutablePublication(existingPub, pub); chkErr != nil {
					_ = roBack.Close()
					return nil, chkErr
				}
			}
		}
		_ = roBack.Close()
	}

	// All integrity, platform, capacity, and conflict checks passed! Now commit to destination.
	dstBackend := opts.Backend
	closeDst := false
	if dstBackend == nil {
		dstBackend, err = ConnectOrOpen(ctx, root)
		if err != nil {
			return nil, err
		}
		closeDst = true
	}
	defer func() {
		if closeDst {
			_ = dstBackend.Close()
		}
	}()

	// 1. Restore Loom commits & refs first (so publications with evicted zips always have durable Loom backing).
	if stagedLoom != nil && (len(meta.Publications) > 0 || len(meta.LoomRefs) > 0) {
		dstLoom, err := openOrInitBareRepo(loomDir)
		if err != nil {
			return nil, err
		}
		refNames := make([]string, 0, len(meta.LoomRefs))
		for rn := range meta.LoomRefs {
			refNames = append(refNames, rn)
		}
		slices.Sort(refNames)
		for _, rn := range refNames {
			targetHash := plumbing.NewHash(meta.LoomRefs[rn])
			if err := copyGitCommitClosure(stagedLoom.Storer, dstLoom.Storer, targetHash); err != nil {
				return nil, err
			}
			if err := dstLoom.Storer.SetReference(plumbing.NewHashReference(plumbing.ReferenceName(rn), targetHash)); err != nil {
				return nil, err
			}
		}
	}

	// 2. Copy all verified OCI blobs into dstBackend.
	importedBlobs := 0
	for _, layer := range ociMan.Layers {
		rc, err := verifiedOCI.Fetch(ctx, layer)
		if err != nil {
			return nil, err
		}
		_, putErr := dstBackend.PutBlob(ctx, layer.Digest.String(), layer.Size, layer.MediaType, rc)
		_ = rc.Close()
		if putErr != nil {
			return nil, putErr
		}
		importedBlobs++
	}

	// 3. Commit CatalogRecordV1 entries (preserving evicted status on *Store, or reconstructing evicted zips over HTTP Client).
	dstStore, isDirectStore := dstBackend.(*Store)
	for _, rec := range meta.Records {
		if evictedSet[rec.Digest] {
			if isDirectStore {
				if _, err := dstStore.putEvictedRecord(ctx, rec); err != nil {
					return nil, err
				}
				continue
			}
			// Over HTTP *Client: if this is an evicted published zip, reconstruct from stagedLoom.
			var matchedPub *PublicationRecord
			for i := range meta.Publications {
				if meta.Publications[i].ZipDigest == rec.Digest {
					matchedPub = &meta.Publications[i]
					break
				}
			}
			if matchedPub != nil && stagedLoom != nil {
				zipBytes, err := reconstructZipFromLoomRepo(stagedLoom, *matchedPub)
				if err != nil {
					return nil, err
				}
				if _, err := dstBackend.PutArtifact(ctx, rec, bytes.NewReader(zipBytes)); err != nil {
					return nil, err
				}
			}
			continue
		}
		if _, err := dstBackend.PutRecord(ctx, rec); err != nil {
			return nil, err
		}
	}

	// 4. Commit PublicationRecords.
	for _, pub := range meta.Publications {
		if _, err := dstBackend.PublishModule(ctx, pub); err != nil {
			return nil, err
		}
	}

	// 5. Assign additive generation newGen = maxExistingGen + 1 (preserving BaselineGeneration and prior manifest:gen-* roots!).
	existingManifests, err := dstBackend.ListManifests(ctx)
	if err != nil {
		return nil, err
	}
	var maxGen uint64
	for _, m := range existingManifests {
		if m.Generation > maxGen {
			maxGen = m.Generation
		}
	}
	newGen := maxGen + 1
	importedManifest := meta.Manifest
	importedManifest.Generation = newGen
	if err := dstBackend.PutManifest(ctx, importedManifest); err != nil {
		return nil, err
	}
	if err := SaveProfile(root, meta.Profile); err != nil {
		return nil, err
	}

	// 6. Run optional post-import materializer hook (e.g. gotoolchain / binmgr seed restore).
	if opts.Materialize != nil {
		if err := opts.Materialize(ctx, dstBackend, importedManifest); err != nil {
			return nil, err
		}
	}

	manifestDigest, err := importedManifest.Digest()
	if err != nil {
		return nil, err
	}

	existingSeal, hasExistingSeal, err := LoadSealState(root)
	if err != nil {
		return nil, err
	}
	shouldSeal := opts.Seal || (hasExistingSeal && existingSeal.Sealed)
	var baselineGen uint64
	if hasExistingSeal && existingSeal.BaselineGeneration > 0 {
		baselineGen = existingSeal.BaselineGeneration
	}

	if shouldSeal {
		if closeDst {
			_ = dstBackend.Close()
			closeDst = false
		}
		var sealBack Backend
		if !closeDst && opts.Backend != nil {
			sealBack = opts.Backend
		}
		sealSt, err := Seal(ctx, SealOptions{
			Root:         root,
			LoomDir:      loomDir,
			Backend:      sealBack,
			Profile:      &meta.Profile,
			Generation:   newGen,
			Now:          opts.Now,
			DiskFreeFunc: opts.DiskFreeFunc,
		})
		if err != nil {
			return nil, err
		}
		baselineGen = sealSt.BaselineGeneration
	}

	return &ImportResult{
		ArtifactRoot:       root,
		LoomDir:            loomDir,
		Platform:           importedManifest.Platform,
		ImportedGeneration: newGen,
		BaselineGeneration: baselineGen,
		Sealed:             shouldSeal,
		ManifestDigest:     manifestDigest,
		BlobsImported:      importedBlobs,
		Publications:       len(meta.Publications),
	}, nil
}
