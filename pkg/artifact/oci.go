// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package artifact

import (
	"archive/tar"
	"bufio"
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
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	// RecipeOCIDerivedV1 names a deterministic, bit-for-bit reproducible OCI image
	// build recipe backed by retained protected inputs in CAS.
	RecipeOCIDerivedV1 RecipeKind = RecipeOCIReproducibleV1

	// RetentionDisposable aliases RetentionScratch for explicitly disposable
	// generated images that make no promise of same-digest reconstruction.
	RetentionDisposable RetentionClass = RetentionScratch

	MediaTypePodmanVMAsset = "application/vnd.bashy.podman-vm.v1+tar"
	MediaTypeAptArchive    = "application/vnd.bashy.apt-archive.v1+tar"
	MediaTypeLangPack      = "application/vnd.bashy.lang-pack.v1+json"

	dockerMediaTypeManifestV2   = "application/vnd.docker.distribution.manifest.v2+json"
	dockerMediaTypeManifestList = "application/vnd.docker.distribution.manifest.list.v2+json"

	annotationRequestedRef = "io.dhnt.bashy.requested-ref"
	maxOCIMetadataBytes    = 32 << 20
	maxOCITarEntryBytes    = 16 << 30
	defaultExportLeaseTTL  = 10 * time.Minute
)

var (
	// ErrNondeterministicImageEviction is returned when a caller attempts to mark
	// a non-deterministic container image build as RetentionReconstructible.
	ErrNondeterministicImageEviction = errors.New("artifact: nondeterministic generated OCI image cannot be marked reconstructible")

	hex64RE = regexp.MustCompile(`^[0-9a-f]{64}$`)

	// MandatoryLanguagePacks are always required in every offline profile.
	MandatoryLanguagePacks = []string{"go", "bashsharp"}

	// ConfigurableLanguagePacks are optional sandbox fence packs that operators
	// may include in ProfileV1.Languages.
	ConfigurableLanguagePacks = []string{"python", "typescript", "rust", "c", "cpp", "java", "zig"}
)

// SupportedLanguagePack reports whether lang is a recognized mandatory or configurable pack.
func SupportedLanguagePack(lang string) bool {
	trimmed := strings.ToLower(strings.TrimSpace(lang))
	return slices.Contains(MandatoryLanguagePacks, trimmed) || slices.Contains(ConfigurableLanguagePacks, trimmed)
}

// CheckSandboxPackAllowed enforces offline language-pack confinement: in sealed
// mode, any pack not declared in profile.Languages fails closed with ErrSealedNetworkDenied.
func CheckSandboxPackAllowed(profile ProfileV1, sealed bool, lang string) error {
	trimmed := strings.ToLower(strings.TrimSpace(lang))
	if trimmed == "" {
		return errors.New("artifact: empty language pack name")
	}
	installed := false
	for _, p := range profile.Languages {
		if strings.EqualFold(strings.TrimSpace(p), trimmed) {
			installed = true
			break
		}
	}
	if sealed && !installed {
		return fmt.Errorf("%w: language pack %q is not installed in sealed profile %s (import a verified pack instead of fetching online)",
			ErrSealedNetworkDenied, trimmed, profile.Name)
	}
	return nil
}

// OCIIngestOptions configures IngestOCILayout and IngestOCIArchive.
type OCIIngestOptions struct {
	LogicalName string
	Version     string
	RefName     string
	Platform    string
	Provenance  string
	Retention   RetentionClass
	Recipe      *RegenerationRecipe
}

// OCIIngestResult summarizes an ingested OCI image closure in CAS.
type OCIIngestResult struct {
	Root           RootDescriptor `json:"root"`
	ManifestDigest string         `json:"manifest_digest"`
	ConfigDigest   string         `json:"config_digest"`
	LayerDigests   []string       `json:"layer_digests"`
	Platform       string         `json:"platform,omitempty"`
	RefName        string         `json:"ref_name,omitempty"`
	TotalSize      int64          `json:"total_size"`
}

// GeneratedOCIImageOptions configures RegisterGeneratedOCIImage.
type GeneratedOCIImageOptions struct {
	ArchivePath    string
	LayoutDir      string
	LogicalName    string
	Version        string
	RefName        string
	Platform       string
	Deterministic  bool
	ExpectedDigest string
	InputDigests   []string
	RecipeParams   map[string]string
	Retention      RetentionClass
}

// OCIExportOptions configures ExportOCIArchiveWithOptions.
type OCIExportOptions struct {
	RefOrDigest     string
	OutputPath      string
	LeaseTTL        time.Duration
	OnLeaseAcquired func(ctx context.Context) error
}

// OCIExportResult reports a reconstructed oci-archive .tar exported from CAS.
type OCIExportResult struct {
	OutputPath     string   `json:"output_path"`
	ManifestDigest string   `json:"manifest_digest"`
	ConfigDigest   string   `json:"config_digest"`
	LayerDigests   []string `json:"layer_digests"`
	RefName        string   `json:"ref_name,omitempty"`
	ArchiveSHA256  string   `json:"archive_sha256"`
	ArchiveSize    int64    `json:"archive_size"`
}

func splitOCIRef(raw string) (repo, tag, dgst string) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", "", ""
	}
	if ValidDigest(s) {
		return "", "", s
	}
	if before, after, ok := strings.Cut(s, "@"); ok {
		s = before
		dgst = after
	}
	lastSlash := strings.LastIndex(s, "/")
	lastColon := strings.LastIndex(s, ":")
	if lastColon > lastSlash {
		repo = s[:lastColon]
		tag = s[lastColon+1:]
	} else {
		repo = s
	}
	return repo, tag, dgst
}

func normalizeOCILogicalRepo(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "oci/")
	s = strings.Trim(s, "/")
	if s == "" {
		return "image"
	}
	s = strings.NewReplacer(":", "-", "\\", "/").Replace(s)
	var cleanSegs []string
	for _, seg := range strings.Split(s, "/") {
		seg = strings.TrimSpace(seg)
		if seg == "" || seg == "." || seg == ".." {
			continue
		}
		cleanSegs = append(cleanSegs, seg)
	}
	if len(cleanSegs) == 0 {
		return "image"
	}
	joined := strings.Join(cleanSegs, "/")
	if !SafeLogicalPath(joined) {
		return "image"
	}
	return joined
}

func cleanArchiveMemberPath(raw string) (string, error) {
	p := strings.TrimSpace(raw)
	for strings.HasPrefix(p, "./") {
		p = strings.TrimPrefix(p, "./")
	}
	p = strings.TrimSuffix(p, "/")
	if p == "" || p == "." {
		return "", nil
	}
	if !SafeLogicalPath(p) {
		return "", fmt.Errorf("artifact: unsafe OCI archive entry path %q", raw)
	}
	return p, nil
}

func extractOCIArchiveToDir(ctx context.Context, archivePath, dstDir string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()

	cleanDst := filepath.Clean(dstDir)
	tr := tar.NewReader(f)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("artifact: read oci archive %s: %w", archivePath, err)
		}
		rel, err := cleanArchiveMemberPath(hdr.Name)
		if err != nil {
			return err
		}
		if rel == "" {
			continue
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			outDir := filepath.Join(cleanDst, filepath.FromSlash(rel))
			if outDir != cleanDst && !strings.HasPrefix(outDir, cleanDst+string(filepath.Separator)) {
				return fmt.Errorf("artifact: OCI archive dir escapes staging root: %q", hdr.Name)
			}
			if err := os.MkdirAll(outDir, 0o700); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if hdr.Size < 0 || hdr.Size > maxOCITarEntryBytes {
				return fmt.Errorf("artifact: invalid OCI archive member size %d for %q", hdr.Size, hdr.Name)
			}
			outPath := filepath.Join(cleanDst, filepath.FromSlash(rel))
			if outPath != cleanDst && !strings.HasPrefix(outPath, cleanDst+string(filepath.Separator)) {
				return fmt.Errorf("artifact: OCI archive member escapes staging root: %q", hdr.Name)
			}
			if err := os.MkdirAll(filepath.Dir(outPath), 0o700); err != nil {
				return err
			}
			outFile, err := os.OpenFile(outPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			n, copyErr := io.Copy(outFile, io.LimitReader(tr, hdr.Size+1))
			closeErr := outFile.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
			if n != hdr.Size {
				return fmt.Errorf("%w: OCI archive member %q size %d != header %d", ErrCorruptPayload, rel, n, hdr.Size)
			}
		default:
			return fmt.Errorf("artifact: unsupported tar entry type %d for %q in OCI archive", hdr.Typeflag, hdr.Name)
		}
	}
	return nil
}

func ociLayoutBlobPath(layoutDir, dgst string) (string, error) {
	if !ValidDigest(dgst) {
		return "", fmt.Errorf("artifact: invalid OCI blob digest %q", dgst)
	}
	hexPart := strings.TrimPrefix(dgst, "sha256:")
	return filepath.Join(layoutDir, "blobs", "sha256", hexPart), nil
}

func readVerifiedLayoutBlobBytes(layoutDir string, desc ocispec.Descriptor, maxBytes int64) ([]byte, error) {
	dgst := string(desc.Digest)
	blobPath, err := ociLayoutBlobPath(layoutDir, dgst)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(blobPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: missing OCI blob %s", ErrCorruptPayload, dgst)
		}
		return nil, err
	}
	if desc.Size >= 0 && fi.Size() != desc.Size {
		return nil, fmt.Errorf("%w: OCI blob %s size %d != descriptor %d", ErrCorruptPayload, dgst, fi.Size(), desc.Size)
	}
	if maxBytes > 0 && fi.Size() > maxBytes {
		return nil, fmt.Errorf("artifact: metadata blob %s (%d bytes) exceeds %d byte limit", dgst, fi.Size(), maxBytes)
	}
	raw, err := os.ReadFile(blobPath)
	if err != nil {
		return nil, err
	}
	if got := DigestBytes(raw); got != dgst {
		return nil, fmt.Errorf("%w: OCI blob %s hashed to %s", ErrCorruptPayload, dgst, got)
	}
	return raw, nil
}

func verifyLayoutLayerFile(layoutDir string, desc ocispec.Descriptor) (string, int64, error) {
	dgst := string(desc.Digest)
	blobPath, err := ociLayoutBlobPath(layoutDir, dgst)
	if err != nil {
		return "", 0, err
	}
	f, err := os.Open(blobPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", 0, fmt.Errorf("%w: missing OCI layer blob %s", ErrCorruptPayload, dgst)
		}
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	if desc.Size >= 0 && n != desc.Size {
		return "", 0, fmt.Errorf("%w: OCI layer %s size %d != descriptor %d", ErrCorruptPayload, dgst, n, desc.Size)
	}
	if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); got != dgst {
		return "", 0, fmt.Errorf("%w: OCI layer %s hashed to %s", ErrCorruptPayload, dgst, got)
	}
	return blobPath, n, nil
}

func isIndexMediaType(mt string) bool {
	return mt == ocispec.MediaTypeImageIndex || mt == dockerMediaTypeManifestList
}

func selectManifestDescriptor(ctx context.Context, layoutDir string, idx ocispec.Index, opts OCIIngestOptions) (ocispec.Descriptor, string, error) {
	if len(idx.Manifests) == 0 {
		return ocispec.Descriptor{}, "", fmt.Errorf("%w: OCI index.json contains no manifests", ErrCorruptPayload)
	}
	chosen := idx.Manifests[0]
	wantRef := strings.TrimSpace(opts.RefName)
	wantVer := strings.TrimSpace(opts.Version)
	if wantRef != "" || wantVer != "" {
		for _, m := range idx.Manifests {
			annRef := m.Annotations[ocispec.AnnotationRefName]
			ctrRef := m.Annotations["io.containerd.image.name"]
			if (wantRef != "" && (annRef == wantRef || ctrRef == wantRef)) ||
				(wantVer != "" && (annRef == wantVer || strings.HasSuffix(annRef, ":"+wantVer) || strings.HasSuffix(ctrRef, ":"+wantVer))) {
				chosen = m
				break
			}
		}
	}
	refFound := firstNonEmptyString(
		opts.RefName,
		chosen.Annotations[ocispec.AnnotationRefName],
		chosen.Annotations["io.containerd.image.name"],
	)

	// Unwrap nested multi-platform image index if present.
	for depth := 0; depth < 3 && isIndexMediaType(chosen.MediaType); depth++ {
		if err := ctx.Err(); err != nil {
			return ocispec.Descriptor{}, "", err
		}
		nestedRaw, err := readVerifiedLayoutBlobBytes(layoutDir, chosen, maxOCIMetadataBytes)
		if err != nil {
			return ocispec.Descriptor{}, "", err
		}
		var nested ocispec.Index
		if err := json.Unmarshal(nestedRaw, &nested); err != nil {
			return ocispec.Descriptor{}, "", fmt.Errorf("%w: decode nested OCI index %s: %w", ErrCorruptPayload, chosen.Digest, err)
		}
		if len(nested.Manifests) == 0 {
			return ocispec.Descriptor{}, "", fmt.Errorf("%w: nested OCI index %s has no manifests", ErrCorruptPayload, chosen.Digest)
		}
		wantPlat := strings.TrimSpace(opts.Platform)
		if wantPlat == "" {
			wantPlat = CurrentPlatform()
		}
		next := nested.Manifests[0]
		for _, cand := range nested.Manifests {
			if cand.Platform != nil && cand.Platform.OS+"/"+cand.Platform.Architecture == wantPlat {
				next = cand
				break
			}
		}
		chosen = next
	}
	return chosen, refFound, nil
}

func firstNonEmptyString(vals ...string) string {
	for _, v := range vals {
		if t := strings.TrimSpace(v); t != "" {
			return t
		}
	}
	return ""
}

// IngestOCIArchive extracts an oci-archive .tar to a temporary staging directory,
// verifies manifest/config/layer SHA-256 digests, and stores the full image closure in CAS.
func IngestOCIArchive(ctx context.Context, backend Backend, archivePath string, opts OCIIngestOptions) (*OCIIngestResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if backend == nil {
		return nil, errors.New("artifact: nil backend")
	}
	stageDir, err := os.MkdirTemp("", "bashy-oci-ingest-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(stageDir)

	if err := extractOCIArchiveToDir(ctx, archivePath, stageDir); err != nil {
		return nil, err
	}
	if opts.LogicalName == "" && opts.RefName == "" {
		base := filepath.Base(archivePath)
		base = strings.TrimSuffix(strings.TrimSuffix(base, ".oci.tar"), ".tar")
		if base != "" && base != "." {
			opts.LogicalName = base
		}
	}
	return IngestOCILayout(ctx, backend, stageDir, opts)
}

// IngestOCILayout parses a standard OCI image layout directory, verifies SHA-256
// digests of manifest, config, and every layer, and commits the closure into CAS.
func IngestOCILayout(ctx context.Context, backend Backend, layoutDir string, opts OCIIngestOptions) (*OCIIngestResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if backend == nil {
		return nil, errors.New("artifact: nil backend")
	}

	retention := opts.Retention
	if retention == "" {
		retention = RetentionProtectedRoot
	}
	if !retention.Valid() {
		return nil, fmt.Errorf("artifact: invalid OCI retention %q", retention)
	}

	layoutRaw, err := os.ReadFile(filepath.Join(layoutDir, ocispec.ImageLayoutFile))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: missing %s in %s", ErrCorruptPayload, ocispec.ImageLayoutFile, layoutDir)
		}
		return nil, err
	}
	var layout ocispec.ImageLayout
	if err := json.Unmarshal(layoutRaw, &layout); err != nil || layout.Version != ocispec.ImageLayoutVersion {
		return nil, fmt.Errorf("%w: invalid OCI layout version in %s", ErrCorruptPayload, layoutDir)
	}

	indexRaw, err := os.ReadFile(filepath.Join(layoutDir, ocispec.ImageIndexFile))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: missing %s in %s", ErrCorruptPayload, ocispec.ImageIndexFile, layoutDir)
		}
		return nil, err
	}
	var idx ocispec.Index
	if err := json.Unmarshal(indexRaw, &idx); err != nil {
		return nil, fmt.Errorf("%w: decode OCI index.json: %w", ErrCorruptPayload, err)
	}

	manifestDesc, refFound, err := selectManifestDescriptor(ctx, layoutDir, idx, opts)
	if err != nil {
		return nil, err
	}
	manifestBytes, err := readVerifiedLayoutBlobBytes(layoutDir, manifestDesc, maxOCIMetadataBytes)
	if err != nil {
		return nil, err
	}
	manifestDigest := string(manifestDesc.Digest)
	manifestMediaType := strings.TrimSpace(manifestDesc.MediaType)
	if manifestMediaType == "" {
		manifestMediaType = ocispec.MediaTypeImageManifest
	}

	if retention == RetentionReconstructible {
		if err := opts.Recipe.Validate(); err != nil {
			return nil, fmt.Errorf("artifact: reconstructible OCI image requires valid recipe: %w", err)
		}
		if opts.Recipe.ExpectedDigest != manifestDigest {
			return nil, fmt.Errorf("%w: built manifest digest %s != expected recipe digest %s",
				ErrNondeterministicImageEviction, manifestDigest, opts.Recipe.ExpectedDigest)
		}
	}

	var manifest ocispec.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return nil, fmt.Errorf("%w: decode OCI manifest %s: %w", ErrCorruptPayload, manifestDigest, err)
	}

	configBytes, err := readVerifiedLayoutBlobBytes(layoutDir, manifest.Config, maxOCIMetadataBytes)
	if err != nil {
		return nil, err
	}
	configDigest := string(manifest.Config.Digest)
	configMediaType := strings.TrimSpace(manifest.Config.MediaType)
	if configMediaType == "" {
		configMediaType = ocispec.MediaTypeImageConfig
	}

	imgPlatform := strings.TrimSpace(opts.Platform)
	if imgPlatform == "" {
		var imgCfg ocispec.Image
		if json.Unmarshal(configBytes, &imgCfg) == nil && imgCfg.OS != "" && imgCfg.Architecture != "" {
			cand := imgCfg.OS + "/" + imgCfg.Architecture
			if validPlatform(cand) {
				imgPlatform = cand
			}
		}
	}
	if imgPlatform == "" && manifestDesc.Platform != nil && manifestDesc.Platform.OS != "" && manifestDesc.Platform.Architecture != "" {
		cand := manifestDesc.Platform.OS + "/" + manifestDesc.Platform.Architecture
		if validPlatform(cand) {
			imgPlatform = cand
		}
	}

	type verifiedLayer struct {
		desc ocispec.Descriptor
		path string
		size int64
	}
	verifiedLayers := make([]verifiedLayer, 0, len(manifest.Layers))
	layerDigests := make([]string, 0, len(manifest.Layers))
	totalSize := int64(len(manifestBytes)) + int64(len(configBytes))

	// Pre-verify every layer's SHA-256 and byte length BEFORE writing any records.
	for _, layerDesc := range manifest.Layers {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		lPath, lSize, err := verifyLayoutLayerFile(layoutDir, layerDesc)
		if err != nil {
			return nil, err
		}
		verifiedLayers = append(verifiedLayers, verifiedLayer{
			desc: layerDesc,
			path: lPath,
			size: lSize,
		})
		layerDigests = append(layerDigests, string(layerDesc.Digest))
		totalSize += lSize
	}

	refRepo, refTag, _ := splitOCIRef(refFound)
	rawName := firstNonEmptyString(opts.LogicalName, refRepo, filepath.Base(layoutDir))
	cleanName := normalizeOCILogicalRepo(rawName)
	versionOrDigest := strings.TrimSpace(opts.Version)
	if versionOrDigest == "" {
		if retention == RetentionScratch {
			shortHex := strings.TrimPrefix(manifestDigest, "sha256:")[:12]
			if refTag != "" {
				versionOrDigest = refTag + "-scratch-" + shortHex
			} else {
				versionOrDigest = "scratch-" + shortHex
			}
		} else if refTag != "" {
			versionOrDigest = refTag
		} else {
			versionOrDigest = manifestDigest
		}
	}

	provenance := strings.TrimSpace(opts.Provenance)
	if provenance == "" {
		if refFound != "" {
			provenance = "oci-ref:" + refFound
		} else {
			provenance = "oci-ingest"
		}
	}

	var retentionRoots []string
	if retention == RetentionProtectedRoot {
		retentionRoots = []string{"oci-image:" + cleanName + "@" + manifestDigest}
	}

	blobRecipeFor := func(blobDigest string) *RegenerationRecipe {
		if retention != RetentionReconstructible || opts.Recipe == nil {
			return nil
		}
		params := make(map[string]string, len(opts.Recipe.Params)+1)
		for k, v := range opts.Recipe.Params {
			params[k] = v
		}
		if blobDigest != manifestDigest {
			params["parent_manifest"] = manifestDigest
		}
		return &RegenerationRecipe{
			Kind:           opts.Recipe.Kind,
			InputDigests:   slices.Clone(opts.Recipe.InputDigests),
			LoomRef:        opts.Recipe.LoomRef,
			Commit:         opts.Recipe.Commit,
			Subdir:         opts.Recipe.Subdir,
			ExpectedDigest: blobDigest,
			ExpectedGoSum:  opts.Recipe.ExpectedGoSum,
			Params:         params,
		}
	}

	// 1. Store verified config blob in CAS.
	if _, err := backend.PutArtifact(ctx, CatalogRecordV1{
		SchemaVersion:  CatalogRecordSchemaV1,
		Digest:         configDigest,
		Size:           int64(len(configBytes)),
		MediaType:      configMediaType,
		Platform:       imgPlatform,
		LogicalName:    "oci-config/" + cleanName,
		LogicalVersion: configDigest,
		Provenance:     provenance,
		Retention:      retention,
		RetentionRoots: retentionRoots,
		Recipe:         blobRecipeFor(configDigest),
	}, bytes.NewReader(configBytes)); err != nil {
		return nil, err
	}

	// 2. Store every verified layer blob in CAS.
	for _, vl := range verifiedLayers {
		lDigest := string(vl.desc.Digest)
		lMedia := strings.TrimSpace(vl.desc.MediaType)
		if lMedia == "" {
			lMedia = ocispec.MediaTypeImageLayerGzip
		}
		lf, err := os.Open(vl.path)
		if err != nil {
			return nil, err
		}
		_, putErr := backend.PutArtifact(ctx, CatalogRecordV1{
			SchemaVersion:  CatalogRecordSchemaV1,
			Digest:         lDigest,
			Size:           vl.size,
			MediaType:      lMedia,
			Platform:       imgPlatform,
			LogicalName:    "oci-layer/" + cleanName,
			LogicalVersion: lDigest,
			Provenance:     provenance,
			Retention:      retention,
			RetentionRoots: retentionRoots,
			Recipe:         blobRecipeFor(lDigest),
		}, lf)
		_ = lf.Close()
		if putErr != nil {
			return nil, putErr
		}
	}

	// 3. Store verified manifest blob with full dependency closure (config + all layers).
	deps := make([]string, 0, 1+len(layerDigests))
	seenDeps := make(map[string]bool, 1+len(layerDigests))
	for _, d := range append([]string{configDigest}, layerDigests...) {
		if !seenDeps[d] {
			seenDeps[d] = true
			deps = append(deps, d)
		}
	}

	if _, err := backend.PutArtifact(ctx, CatalogRecordV1{
		SchemaVersion:  CatalogRecordSchemaV1,
		Digest:         manifestDigest,
		Size:           int64(len(manifestBytes)),
		MediaType:      manifestMediaType,
		Platform:       imgPlatform,
		LogicalName:    "oci/" + cleanName,
		LogicalVersion: versionOrDigest,
		Dependencies:   deps,
		Provenance:     provenance,
		Retention:      retention,
		RetentionRoots: retentionRoots,
		Recipe:         blobRecipeFor(manifestDigest),
	}, bytes.NewReader(manifestBytes)); err != nil {
		return nil, err
	}

	root := RootDescriptor{
		Kind:      "oci-image",
		Name:      "oci/" + cleanName,
		Version:   versionOrDigest,
		Platform:  imgPlatform,
		MediaType: manifestMediaType,
		Digest:    manifestDigest,
		Size:      int64(len(manifestBytes)),
		Deps:      deps,
	}
	if err := root.Validate(); err != nil {
		return nil, err
	}

	return &OCIIngestResult{
		Root:           root,
		ManifestDigest: manifestDigest,
		ConfigDigest:   configDigest,
		LayerDigests:   layerDigests,
		Platform:       imgPlatform,
		RefName:        refFound,
		TotalSize:      totalSize,
	}, nil
}

// RegisterGeneratedOCIImage enforces the Sprint 654 S4 nondeterministic vs
// reproducible generated-image retention policy before committing to CAS:
//   - RetentionReconstructible (RecipeOCIDerivedV1) is permitted ONLY when
//     Deterministic == true, ExpectedDigest != "", and every InputDigests entry
//     exists in CAS as a retained RetentionProtectedRoot.
//   - When Deterministic == false, attempting RetentionReconstructible fails
//     closed with ErrNondeterministicImageEviction; callers must keep the image
//     as RetentionProtectedRoot or explicitly mark it disposable scratch
//     (RetentionScratch / RetentionDisposable) with no promise of same-digest recovery.
func RegisterGeneratedOCIImage(ctx context.Context, backend Backend, opts GeneratedOCIImageOptions) (*OCIIngestResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if backend == nil {
		return nil, errors.New("artifact: nil backend")
	}
	hasArchive := strings.TrimSpace(opts.ArchivePath) != ""
	hasLayout := strings.TrimSpace(opts.LayoutDir) != ""
	if hasArchive == hasLayout {
		return nil, errors.New("artifact: RegisterGeneratedOCIImage requires exactly one of ArchivePath or LayoutDir")
	}

	retention := opts.Retention
	if retention == "" {
		retention = RetentionProtectedRoot
	}
	if !retention.Valid() {
		return nil, fmt.Errorf("artifact: invalid retention %q", retention)
	}

	var recipe *RegenerationRecipe
	if !opts.Deterministic {
		if retention == RetentionReconstructible {
			return nil, fmt.Errorf("%w: image %q has Deterministic=false; store as protected or disposable scratch instead",
				ErrNondeterministicImageEviction, opts.LogicalName)
		}
	} else if retention == RetentionReconstructible {
		if !ValidDigest(opts.ExpectedDigest) {
			return nil, fmt.Errorf("%w: reproducible OCI image requires valid ExpectedDigest (got %q)",
				ErrMissingRecoveryInputs, opts.ExpectedDigest)
		}
		if len(opts.InputDigests) == 0 {
			return nil, fmt.Errorf("%w: reproducible OCI image requires retained protected InputDigests",
				ErrMissingRecoveryInputs)
		}
		for _, inD := range opts.InputDigests {
			if !ValidDigest(inD) {
				return nil, fmt.Errorf("%w: invalid build input digest %q", ErrMissingRecoveryInputs, inD)
			}
			st, err := backend.GetRecordStatus(ctx, inD)
			if err != nil {
				return nil, fmt.Errorf("%w: lookup build input %s: %v", ErrMissingRecoveryInputs, inD, err)
			}
			if !st.PayloadPresent || (st.Record.Retention != RetentionProtectedRoot && len(st.Record.RetentionRoots) == 0) {
				return nil, fmt.Errorf("%w: build input %s must be present as RetentionProtectedRoot in CAS",
					ErrMissingRecoveryInputs, inD)
			}
			if err := verifyBlobInBackend(ctx, backend, inD, st.Record.Size, false); err != nil {
				return nil, fmt.Errorf("%w: build input %s blob unavailable: %v", ErrMissingRecoveryInputs, inD, err)
			}
		}
		recipe = &RegenerationRecipe{
			Kind:           RecipeOCIDerivedV1,
			InputDigests:   slices.Clone(opts.InputDigests),
			ExpectedDigest: opts.ExpectedDigest,
			Params:         opts.RecipeParams,
		}
		if err := recipe.Validate(); err != nil {
			return nil, err
		}
	}

	ingestOpts := OCIIngestOptions{
		LogicalName: opts.LogicalName,
		Version:     opts.Version,
		RefName:     opts.RefName,
		Platform:    opts.Platform,
		Retention:   retention,
		Recipe:      recipe,
	}
	if !opts.Deterministic && retention == RetentionScratch && strings.TrimSpace(ingestOpts.Version) == "" {
		// Leave Version empty so IngestOCILayout stamps a unique digest-derived scratch identity.
		ingestOpts.Provenance = "oci-generated:nondeterministic-scratch"
	} else if opts.Deterministic {
		ingestOpts.Provenance = "oci-generated:deterministic"
	} else {
		ingestOpts.Provenance = "oci-generated:nondeterministic-protected"
	}

	if hasArchive {
		return IngestOCIArchive(ctx, backend, opts.ArchivePath, ingestOpts)
	}
	return IngestOCILayout(ctx, backend, opts.LayoutDir, ingestOpts)
}

func extractRefFromProvenance(prov string) string {
	if after, ok := strings.CutPrefix(strings.TrimSpace(prov), "oci-ref:"); ok {
		return strings.TrimSpace(after)
	}
	return ""
}

// ResolveOCIImage resolves a manifest digest, NAME@sha256:<digest>, NAME:TAG,
// or logical oci/<name> reference in backend to its verified manifest digest and OCI ref tag.
func ResolveOCIImage(ctx context.Context, backend Backend, refOrDigest string) (string, string, error) {
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	if backend == nil {
		return "", "", errors.New("artifact: nil backend")
	}
	raw := strings.TrimSpace(refOrDigest)
	if raw == "" {
		return "", "", errors.New("artifact: empty OCI reference")
	}

	repo, tag, dgst := splitOCIRef(raw)
	if dgst != "" {
		if !ValidDigest(dgst) {
			return "", "", fmt.Errorf("artifact: invalid OCI digest %q in %q", dgst, raw)
		}
		st, err := backend.GetRecordStatus(ctx, dgst)
		if err != nil {
			return "", "", err
		}
		if !st.PayloadPresent {
			return "", "", fmt.Errorf("%w: %s", ErrPayloadEvicted, dgst)
		}
		refTag := extractRefFromProvenance(st.Record.Provenance)
		if repo != "" {
			if tag != "" {
				refTag = repo + ":" + tag
			} else if refTag == "" || !strings.HasPrefix(refTag, repo+":") {
				refTag = repo + ":pinned-" + strings.TrimPrefix(dgst, "sha256:")[:12]
			}
		}
		return dgst, refTag, nil
	}

	cleanRepo := normalizeOCILogicalRepo(repo)
	candidates := []string{"oci/" + cleanRepo}
	if trimmedLocal, ok := strings.CutPrefix(cleanRepo, "localhost/"); ok && trimmedLocal != "" {
		candidates = append(candidates, "oci/"+trimmedLocal)
	} else {
		candidates = append(candidates, "oci/localhost/"+cleanRepo)
	}

	if tag != "" {
		for _, candName := range candidates {
			st, err := backend.LookupLogical(ctx, candName, tag)
			if err == nil {
				if !st.PayloadPresent {
					return "", "", fmt.Errorf("%w: %s", ErrPayloadEvicted, st.Record.Digest)
				}
				refOut := repo + ":" + tag
				if strings.HasPrefix(repo, "oci/") {
					refOut = extractRefFromProvenance(st.Record.Provenance)
				}
				return st.Record.Digest, refOut, nil
			}
		}
	}

	if rl, ok := backend.(interface {
		ListRecords(context.Context) ([]RecordStatus, error)
	}); ok {
		recs, err := rl.ListRecords(ctx)
		if err == nil {
			var best *RecordStatus
			for i := range recs {
				rs := &recs[i]
				if !slices.Contains(candidates, rs.Record.LogicalName) {
					continue
				}
				if tag != "" && rs.Record.LogicalVersion != tag {
					continue
				}
				if best == nil || rs.Record.CreatedAt.After(best.Record.CreatedAt) {
					best = rs
				}
			}
			if best != nil {
				if !best.PayloadPresent {
					return "", "", fmt.Errorf("%w: %s", ErrPayloadEvicted, best.Record.Digest)
				}
				refOut := extractRefFromProvenance(best.Record.Provenance)
				if refOut == "" && repo != "" && !strings.HasPrefix(repo, "oci/") {
					ver := tag
					if ver == "" && !ValidDigest(best.Record.LogicalVersion) {
						ver = best.Record.LogicalVersion
					}
					if ver != "" {
						refOut = repo + ":" + ver
					}
				}
				return best.Record.Digest, refOut, nil
			}
		}
	}

	return "", "", fmt.Errorf("%w: OCI image %q", ErrNotFound, raw)
}

// ExportOCIArchive reconstructs a self-contained oci-archive .tar directly from
// CAS blobs under an active read lease, verifying every blob's SHA-256 digest.
func ExportOCIArchive(ctx context.Context, backend Backend, refOrDigest string, outTarPath string) (*OCIExportResult, error) {
	return ExportOCIArchiveWithOptions(ctx, backend, OCIExportOptions{
		RefOrDigest: refOrDigest,
		OutputPath:  outTarPath,
	})
}

// ExportOCIArchiveWithOptions reconstructs a self-contained oci-archive .tar
// under Backend.WithReadLease with configurable lease TTL and test hooks.
func ExportOCIArchiveWithOptions(ctx context.Context, backend Backend, opts OCIExportOptions) (*OCIExportResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if backend == nil {
		return nil, errors.New("artifact: nil backend")
	}
	if strings.TrimSpace(opts.OutputPath) == "" {
		return nil, errors.New("artifact: OCI export output path is required")
	}
	outAbs, err := filepath.Abs(opts.OutputPath)
	if err != nil {
		return nil, err
	}

	manifestDigest, refName, err := ResolveOCIImage(ctx, backend, opts.RefOrDigest)
	if err != nil {
		return nil, err
	}

	// Read manifest initially to discover full config + layer digest closure for read lease.
	manifestBytesInitial, err := backend.FetchVerified(ctx, manifestDigest)
	if err != nil {
		return nil, err
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(manifestBytesInitial, &manifest); err != nil {
		return nil, fmt.Errorf("%w: decode manifest %s: %w", ErrCorruptPayload, manifestDigest, err)
	}
	configDigest := string(manifest.Config.Digest)
	if !ValidDigest(configDigest) {
		return nil, fmt.Errorf("%w: invalid config digest %q in manifest %s", ErrCorruptPayload, configDigest, manifestDigest)
	}

	leaseDigests := make([]string, 0, 2+len(manifest.Layers))
	leaseDigests = append(leaseDigests, manifestDigest, configDigest)
	layerDigests := make([]string, 0, len(manifest.Layers))
	for _, l := range manifest.Layers {
		ld := string(l.Digest)
		if !ValidDigest(ld) {
			return nil, fmt.Errorf("%w: invalid layer digest %q in manifest %s", ErrCorruptPayload, ld, manifestDigest)
		}
		layerDigests = append(layerDigests, ld)
		leaseDigests = append(leaseDigests, ld)
	}

	manifestMediaType := ocispec.MediaTypeImageManifest
	if st, stErr := backend.GetRecordStatus(ctx, manifestDigest); stErr == nil && st.Record.MediaType != "" {
		manifestMediaType = st.Record.MediaType
	} else if manifest.MediaType != "" {
		manifestMediaType = manifest.MediaType
	}

	ttl := opts.LeaseTTL
	if ttl <= 0 {
		ttl = defaultExportLeaseTTL
	}

	var result *OCIExportResult
	err = backend.WithReadLease(ctx, 0, "", ttl, leaseDigests, func(leaseCtx context.Context) error {
		if opts.OnLeaseAcquired != nil {
			if err := opts.OnLeaseAcquired(leaseCtx); err != nil {
				return err
			}
		}

		// Re-verify manifest inside active read lease.
		manifestBytes, err := backend.FetchVerified(leaseCtx, manifestDigest)
		if err != nil {
			return err
		}

		if err := os.MkdirAll(filepath.Dir(outAbs), 0o755); err != nil {
			return err
		}
		tmpOut, err := os.CreateTemp(filepath.Dir(outAbs), ".oci-export-*.tar")
		if err != nil {
			return err
		}
		tmpName := tmpOut.Name()
		cleanup := true
		defer func() {
			_ = tmpOut.Close()
			if cleanup {
				_ = os.Remove(tmpName)
			}
		}()

		tarHasher := sha256.New()
		tw := tar.NewWriter(io.MultiWriter(tmpOut, tarHasher))
		fixedTime := time.Unix(0, 0).UTC()

		writeBytesEntry := func(entryName string, payload []byte) error {
			if err := tw.WriteHeader(&tar.Header{
				Name:     entryName,
				Typeflag: tar.TypeReg,
				Mode:     0o644,
				Size:     int64(len(payload)),
				ModTime:  fixedTime,
			}); err != nil {
				return err
			}
			_, err := tw.Write(payload)
			return err
		}

		streamVerifiedBlobEntry := func(dgst string, expectedSize int64) error {
			rc, err := backend.OpenBlob(leaseCtx, dgst)
			if err != nil {
				return err
			}
			// Buffer to temp file when size is unknown so tar header gets exact byte count.
			blobSize := expectedSize
			var reader io.Reader = rc
			var stageTmp *os.File
			if blobSize < 0 {
				stageTmp, err = os.CreateTemp(filepath.Dir(outAbs), ".oci-blob-*")
				if err != nil {
					_ = rc.Close()
					return err
				}
				stagePath := stageTmp.Name()
				defer func() {
					_ = stageTmp.Close()
					_ = os.Remove(stagePath)
				}()
				h := sha256.New()
				n, copyErr := io.Copy(io.MultiWriter(stageTmp, h), rc)
				closeErr := rc.Close()
				if copyErr != nil {
					return copyErr
				}
				if closeErr != nil {
					return closeErr
				}
				if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); got != dgst {
					return fmt.Errorf("%w: blob %s hashed to %s", ErrCorruptPayload, dgst, got)
				}
				if _, err := stageTmp.Seek(0, io.SeekStart); err != nil {
					return err
				}
				blobSize = n
				reader = stageTmp
			} else {
				defer rc.Close()
			}

			entryName := "blobs/sha256/" + strings.TrimPrefix(dgst, "sha256:")
			if err := tw.WriteHeader(&tar.Header{
				Name:     entryName,
				Typeflag: tar.TypeReg,
				Mode:     0o644,
				Size:     blobSize,
				ModTime:  fixedTime,
			}); err != nil {
				return err
			}
			h := sha256.New()
			n, copyErr := io.Copy(io.MultiWriter(tw, h), reader)
			if copyErr != nil {
				return copyErr
			}
			if stageTmp == nil {
				if closeErr := rc.Close(); closeErr != nil {
					return closeErr
				}
			}
			if n != blobSize {
				return fmt.Errorf("%w: exported blob %s size %d != %d", ErrCorruptPayload, dgst, n, blobSize)
			}
			if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); got != dgst {
				return fmt.Errorf("%w: exported blob %s hashed to %s", ErrCorruptPayload, dgst, got)
			}
			return nil
		}

		layoutBytes, _ := json.Marshal(ocispec.ImageLayout{Version: ocispec.ImageLayoutVersion})
		if err := writeBytesEntry(ocispec.ImageLayoutFile, layoutBytes); err != nil {
			return err
		}

		annotations := map[string]string{}
		if refName != "" {
			annotations[ocispec.AnnotationRefName] = refName
			annotations["io.containerd.image.name"] = refName
		}
		if s := strings.TrimSpace(opts.RefOrDigest); s != "" {
			annotations[annotationRequestedRef] = s
		}
		if len(annotations) == 0 {
			annotations = nil
		}

		indexObj := ocispec.Index{
			Versioned: specs.Versioned{SchemaVersion: 2},
			MediaType: ocispec.MediaTypeImageIndex,
			Manifests: []ocispec.Descriptor{
				{
					MediaType:   manifestMediaType,
					Digest:      digest.Digest(manifestDigest),
					Size:        int64(len(manifestBytes)),
					Annotations: annotations,
				},
			},
		}
		indexBytes, err := json.MarshalIndent(indexObj, "", "  ")
		if err != nil {
			return err
		}
		indexBytes = append(indexBytes, '\n')
		if err := writeBytesEntry(ocispec.ImageIndexFile, indexBytes); err != nil {
			return err
		}

		emittedBlobs := make(map[string]bool, 2+len(manifest.Layers))
		if err := streamVerifiedBlobEntry(configDigest, manifest.Config.Size); err != nil {
			return err
		}
		emittedBlobs[configDigest] = true

		for _, layer := range manifest.Layers {
			ld := string(layer.Digest)
			if emittedBlobs[ld] {
				continue
			}
			if err := streamVerifiedBlobEntry(ld, layer.Size); err != nil {
				return err
			}
			emittedBlobs[ld] = true
		}

		if !emittedBlobs[manifestDigest] {
			if err := writeBytesEntry("blobs/sha256/"+strings.TrimPrefix(manifestDigest, "sha256:"), manifestBytes); err != nil {
				return err
			}
			emittedBlobs[manifestDigest] = true
		}

		if err := tw.Close(); err != nil {
			return err
		}
		fi, err := tmpOut.Stat()
		if err != nil {
			return err
		}
		archiveSize := fi.Size()
		if err := tmpOut.Close(); err != nil {
			return err
		}
		if err := os.Rename(tmpName, outAbs); err != nil {
			return err
		}
		cleanup = false

		result = &OCIExportResult{
			OutputPath:     outAbs,
			ManifestDigest: manifestDigest,
			ConfigDigest:   configDigest,
			LayerDigests:   layerDigests,
			RefName:        refName,
			ArchiveSHA256:  "sha256:" + hex.EncodeToString(tarHasher.Sum(nil)),
			ArchiveSize:    archiveSize,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// BuildImageLock represents the pinned identity in tools/bashy-build/image.lock.
type BuildImageLock struct {
	BashyVersion    string            `json:"bashy_version"`
	BashySHA256     string            `json:"bashy_sha256,omitempty"`
	PreviousVersion string            `json:"previous_version,omitempty"`
	BuiltWithDigest string            `json:"built_with_digest,omitempty"`
	AptSnapshot     string            `json:"apt_snapshot"`
	GoVersion       string            `json:"go_version"`
	GoSHA256AMD64   string            `json:"go_sha256_amd64"`
	GoSHA256ARM64   string            `json:"go_sha256_arm64"`
	BaseImage       string            `json:"base_image"`
	BaseDigest      string            `json:"base_digest"`
	Image           string            `json:"image"`
	ImageDigest     string            `json:"image_digest"`
	SourceCommits   map[string]string `json:"source_commits,omitempty"`
	Extra           map[string]string `json:"extra,omitempty"`
}

// Validate checks that required pins and digests in BuildImageLock are well-formed.
func (l *BuildImageLock) Validate() error {
	if l == nil {
		return errors.New("artifact: nil build image lock")
	}
	if strings.TrimSpace(l.BashyVersion) == "" {
		return errors.New("artifact: image.lock missing bashy_version")
	}
	if strings.TrimSpace(l.GoVersion) == "" {
		return errors.New("artifact: image.lock missing go_version")
	}
	if !hex64RE.MatchString(l.GoSHA256AMD64) {
		return fmt.Errorf("artifact: image.lock invalid go_sha256_amd64 %q", l.GoSHA256AMD64)
	}
	if !hex64RE.MatchString(l.GoSHA256ARM64) {
		return fmt.Errorf("artifact: image.lock invalid go_sha256_arm64 %q", l.GoSHA256ARM64)
	}
	if strings.TrimSpace(l.AptSnapshot) == "" {
		return errors.New("artifact: image.lock missing apt_snapshot")
	}
	if strings.TrimSpace(l.BaseImage) == "" || !ValidDigest(l.BaseDigest) {
		return fmt.Errorf("artifact: image.lock invalid base_image/base_digest (%q / %q)", l.BaseImage, l.BaseDigest)
	}
	if strings.TrimSpace(l.Image) == "" || !ValidDigest(l.ImageDigest) {
		return fmt.Errorf("artifact: image.lock invalid image/image_digest (%q / %q)", l.Image, l.ImageDigest)
	}
	return nil
}

// ParseBuildImageLock parses a key=value tools/bashy-build/image.lock file.
func ParseBuildImageLock(lockPath string) (*BuildImageLock, error) {
	f, err := os.Open(lockPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	lock := &BuildImageLock{
		SourceCommits: make(map[string]string),
		Extra:         make(map[string]string),
	}
	sc := bufio.NewScanner(f)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("artifact: malformed line %d in %s: %q", lineNo, lockPath, line)
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		switch k {
		case "bashy_version":
			lock.BashyVersion = v
		case "bashy_sha256":
			lock.BashySHA256 = v
		case "previous_version":
			lock.PreviousVersion = v
		case "built_with_digest":
			lock.BuiltWithDigest = v
		case "apt_snapshot":
			lock.AptSnapshot = v
		case "go_version":
			lock.GoVersion = v
		case "go_sha256_amd64":
			lock.GoSHA256AMD64 = v
		case "go_sha256_arm64":
			lock.GoSHA256ARM64 = v
		case "base_image":
			lock.BaseImage = v
		case "base_digest":
			lock.BaseDigest = v
		case "image":
			lock.Image = v
		case "image_digest":
			lock.ImageDigest = v
		default:
			if repoKey, isSrc := strings.CutPrefix(k, "source_"); isSrc {
				lock.SourceCommits[strings.TrimSuffix(repoKey, "_commit")] = v
			} else {
				lock.Extra[k] = v
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(lock.SourceCommits) == 0 {
		lock.SourceCommits = nil
	}
	if len(lock.Extra) == 0 {
		lock.Extra = nil
	}
	if err := lock.Validate(); err != nil {
		return nil, err
	}
	return lock, nil
}

// BuildImageLockStatus reports whether the pinned digests and inputs of BuildImageLock
// are present in the local artifact store.
type BuildImageLockStatus struct {
	Lock               BuildImageLock `json:"lock"`
	BaseImagePresent   bool           `json:"base_image_present"`
	BuildImagePresent  bool           `json:"build_image_present"`
	GoToolchainPresent bool           `json:"go_toolchain_present"`
	AptArchivePresent  bool           `json:"apt_archive_present"`
	Ready              bool           `json:"ready"`
}

// InspectBuildImageLockStatus checks backend (if non-nil) for the pinned base image,
// build image, Go toolchain seed, and apt snapshot archive declared in lock.
func InspectBuildImageLockStatus(ctx context.Context, backend Backend, lock *BuildImageLock) (*BuildImageLockStatus, error) {
	if err := lock.Validate(); err != nil {
		return nil, err
	}
	out := &BuildImageLockStatus{Lock: *lock}
	if backend == nil {
		return out, nil
	}
	if st, err := backend.GetRecordStatus(ctx, lock.BaseDigest); err == nil && st.PayloadPresent {
		out.BaseImagePresent = true
	}
	if st, err := backend.GetRecordStatus(ctx, lock.ImageDigest); err == nil && st.PayloadPresent {
		out.BuildImagePresent = true
	}
	if st, err := backend.LookupLogical(ctx, "gotoolchain/go", lock.GoVersion); err == nil && st.PayloadPresent {
		out.GoToolchainPresent = true
	}
	if st, err := backend.LookupLogical(ctx, "apt-archive/"+lock.AptSnapshot, lock.AptSnapshot); err == nil && st.PayloadPresent {
		out.AptArchivePresent = true
	}
	out.Ready = out.BaseImagePresent && out.BuildImagePresent && out.GoToolchainPresent && out.AptArchivePresent
	return out, nil
}

// HelperCaptureOptions configures CaptureVMAndHelperAssets.
type HelperCaptureOptions struct {
	Platform       string            // default CurrentPlatform()
	Version        string            // default "v1"
	HelperPaths    map[string]string // logical helper name (gvproxy, vfkit, win-sshproxy, podman-static, vm-rootfs, machine-config) -> file or dir path
	AptArchivePath string            // optional dir or .tar of pinned apt .deb/Packages metadata
	AptSnapshot    string            // e.g. "20260821T190000Z"
}

// HelperCaptureResult reports captured Podman VM/helper and apt-archive protected roots.
type HelperCaptureResult struct {
	HelperRoot *RootDescriptor  `json:"helper_root,omitempty"`
	AptRoot    *RootDescriptor  `json:"apt_root,omitempty"`
	Roots      []RootDescriptor `json:"roots"`
}

// HelperRestoreOptions configures RestoreVMAndHelperAssets.
type HelperRestoreOptions struct {
	Version       string
	HelperDigest  string
	HelperDestDir string
	AptSnapshot   string
	AptDigest     string
	AptDestDir    string
}

// HelperRestoreResult reports restored Podman VM/helper and apt-archive paths.
type HelperRestoreResult struct {
	HelperFiles []string `json:"helper_files,omitempty"`
	AptFiles    []string `json:"apt_files,omitempty"`
}

type tarSourceEntry struct {
	archivePath string
	diskPath    string
	mode        int64
	size        int64
}

func buildDeterministicTarBytes(ctx context.Context, entries []tarSourceEntry, headerJSON map[string]any) ([]byte, error) {
	slices.SortFunc(entries, func(a, b tarSourceEntry) int {
		return strings.Compare(a.archivePath, b.archivePath)
	})
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	fixedTime := time.Unix(0, 0).UTC()

	if headerJSON != nil {
		metaBytes, err := json.MarshalIndent(headerJSON, "", "  ")
		if err != nil {
			return nil, err
		}
		metaBytes = append(metaBytes, '\n')
		if err := tw.WriteHeader(&tar.Header{
			Name:     "bashy-asset-metadata.json",
			Typeflag: tar.TypeReg,
			Mode:     0o644,
			Size:     int64(len(metaBytes)),
			ModTime:  fixedTime,
		}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(metaBytes); err != nil {
			return nil, err
		}
	}

	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !SafeLogicalPath(e.archivePath) {
			return nil, fmt.Errorf("artifact: unsafe archive path %q", e.archivePath)
		}
		if err := tw.WriteHeader(&tar.Header{
			Name:     e.archivePath,
			Typeflag: tar.TypeReg,
			Mode:     e.mode,
			Size:     e.size,
			ModTime:  fixedTime,
		}); err != nil {
			return nil, err
		}
		in, err := os.Open(e.diskPath)
		if err != nil {
			return nil, err
		}
		_, copyErr := io.Copy(tw, in)
		closeErr := in.Close()
		if copyErr != nil {
			return nil, copyErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func collectPathTarEntries(prefix, srcPath string) ([]tarSourceEntry, error) {
	fi, err := os.Stat(srcPath)
	if err != nil {
		return nil, err
	}
	if fi.Mode().IsRegular() {
		mode := int64(0o644)
		if fi.Mode()&0o111 != 0 {
			mode = 0o755
		}
		name := prefix
		if name == "" {
			name = filepath.Base(srcPath)
		}
		return []tarSourceEntry{{
			archivePath: name,
			diskPath:    srcPath,
			mode:        mode,
			size:        fi.Size(),
		}}, nil
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("artifact: %s is neither regular file nor directory", srcPath)
	}
	var out []tarSourceEntry
	err = filepath.WalkDir(srcPath, func(p string, d fs.DirEntry, walkErr error) error {
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
			return nil
		}
		rel, err := filepath.Rel(srcPath, p)
		if err != nil {
			return err
		}
		relSlash := filepath.ToSlash(rel)
		archName := relSlash
		if prefix != "" {
			archName = path.Join(prefix, relSlash)
		}
		mode := int64(0o644)
		if info.Mode()&0o111 != 0 {
			mode = 0o755
		}
		out = append(out, tarSourceEntry{
			archivePath: archName,
			diskPath:    p,
			mode:        mode,
			size:        info.Size(),
		})
		return nil
	})
	return out, err
}

// CaptureVMAndHelperAssets packages Podman engine/VM helpers (gvproxy, vfkit,
// win-sshproxy, podman-static, VM rootfs/config) and pinned apt package archives
// into CAS as RetentionProtectedRoot so VM/image recreation never fetches online.
func CaptureVMAndHelperAssets(ctx context.Context, backend Backend, opts HelperCaptureOptions) (*HelperCaptureResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if backend == nil {
		return nil, errors.New("artifact: nil backend")
	}
	if len(opts.HelperPaths) == 0 && strings.TrimSpace(opts.AptArchivePath) == "" {
		return nil, errors.New("artifact: CaptureVMAndHelperAssets requires HelperPaths and/or AptArchivePath")
	}

	platform := strings.TrimSpace(opts.Platform)
	if platform == "" {
		platform = CurrentPlatform()
	}
	version := strings.TrimSpace(opts.Version)
	if version == "" {
		version = "v1"
	}

	res := &HelperCaptureResult{}

	if len(opts.HelperPaths) > 0 {
		names := make([]string, 0, len(opts.HelperPaths))
		for k := range opts.HelperPaths {
			names = append(names, k)
		}
		slices.Sort(names)

		var entries []tarSourceEntry
		for _, name := range names {
			cleanKey := normalizeOCILogicalRepo(name)
			subEntries, err := collectPathTarEntries(cleanKey, opts.HelperPaths[name])
			if err != nil {
				return nil, fmt.Errorf("artifact: capture helper %q: %w", name, err)
			}
			entries = append(entries, subEntries...)
		}

		tarBytes, err := buildDeterministicTarBytes(ctx, entries, map[string]any{
			"schema":   "bashy-podman-vm-helpers-v1",
			"platform": platform,
			"version":  version,
			"helpers":  names,
		})
		if err != nil {
			return nil, err
		}
		dgst := DigestBytes(tarBytes)
		if _, err := backend.PutArtifact(ctx, CatalogRecordV1{
			SchemaVersion:  CatalogRecordSchemaV1,
			Digest:         dgst,
			Size:           int64(len(tarBytes)),
			MediaType:      MediaTypePodmanVMAsset,
			Platform:       platform,
			LogicalName:    "podman-vm/helpers",
			LogicalVersion: version,
			Provenance:     "podman-vm-capture",
			Retention:      RetentionProtectedRoot,
			RetentionRoots: []string{"podman-vm:helpers@" + dgst},
		}, bytes.NewReader(tarBytes)); err != nil {
			return nil, err
		}
		hr := RootDescriptor{
			Kind:      "podman-vm",
			Name:      "podman-vm/helpers",
			Version:   version,
			Platform:  platform,
			MediaType: MediaTypePodmanVMAsset,
			Digest:    dgst,
			Size:      int64(len(tarBytes)),
		}
		if err := hr.Validate(); err != nil {
			return nil, err
		}
		res.HelperRoot = &hr
		res.Roots = append(res.Roots, hr)
	}

	if aptPath := strings.TrimSpace(opts.AptArchivePath); aptPath != "" {
		aptSnap := strings.TrimSpace(opts.AptSnapshot)
		if aptSnap == "" {
			aptSnap = version
		}
		cleanSnap := normalizeOCILogicalRepo(aptSnap)

		var aptTarBytes []byte
		fi, err := os.Stat(aptPath)
		if err != nil {
			return nil, err
		}
		if fi.Mode().IsRegular() && strings.HasSuffix(strings.ToLower(aptPath), ".tar") {
			aptTarBytes, err = os.ReadFile(aptPath)
			if err != nil {
				return nil, err
			}
		} else {
			entries, err := collectPathTarEntries("", aptPath)
			if err != nil {
				return nil, err
			}
			aptTarBytes, err = buildDeterministicTarBytes(ctx, entries, map[string]any{
				"schema":       "bashy-apt-archive-v1",
				"apt_snapshot": aptSnap,
			})
			if err != nil {
				return nil, err
			}
		}

		dgst := DigestBytes(aptTarBytes)
		if _, err := backend.PutArtifact(ctx, CatalogRecordV1{
			SchemaVersion:  CatalogRecordSchemaV1,
			Digest:         dgst,
			Size:           int64(len(aptTarBytes)),
			MediaType:      MediaTypeAptArchive,
			LogicalName:    "apt-archive/" + cleanSnap,
			LogicalVersion: aptSnap,
			Provenance:     "apt-archive-capture:" + aptSnap,
			Retention:      RetentionProtectedRoot,
			RetentionRoots: []string{"apt-archive:" + cleanSnap + "@" + dgst},
		}, bytes.NewReader(aptTarBytes)); err != nil {
			return nil, err
		}
		ar := RootDescriptor{
			Kind:      "apt-archive",
			Name:      "apt-archive/" + cleanSnap,
			Version:   aptSnap,
			MediaType: MediaTypeAptArchive,
			Digest:    dgst,
			Size:      int64(len(aptTarBytes)),
		}
		if err := ar.Validate(); err != nil {
			return nil, err
		}
		res.AptRoot = &ar
		res.Roots = append(res.Roots, ar)
	}

	return res, nil
}

func extractAssetTarBytesToDir(tarBytes []byte, dstDir string) ([]string, error) {
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return nil, err
	}
	cleanDst := filepath.Clean(dstDir)
	tr := tar.NewReader(bytes.NewReader(tarBytes))
	var extracted []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			continue
		}
		rel, err := cleanArchiveMemberPath(hdr.Name)
		if err != nil {
			return nil, err
		}
		if rel == "" || rel == "bashy-asset-metadata.json" {
			continue
		}
		outPath := filepath.Join(cleanDst, filepath.FromSlash(rel))
		if outPath != cleanDst && !strings.HasPrefix(outPath, cleanDst+string(filepath.Separator)) {
			return nil, fmt.Errorf("artifact: asset archive path escapes destination: %q", hdr.Name)
		}
		if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
			return nil, err
		}
		mode := os.FileMode(hdr.Mode)&0o777 | 0o600
		f, err := os.OpenFile(outPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
		if err != nil {
			return nil, err
		}
		_, copyErr := io.Copy(f, tr)
		closeErr := f.Close()
		if copyErr != nil {
			return nil, copyErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		_ = os.Chmod(outPath, mode)
		extracted = append(extracted, outPath)
	}
	return extracted, nil
}

// RestoreVMAndHelperAssets restores protected Podman helper/VM assets and/or
// pinned apt package archives from CAS under an active read lease.
func RestoreVMAndHelperAssets(ctx context.Context, backend Backend, opts HelperRestoreOptions) (*HelperRestoreResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if backend == nil {
		return nil, errors.New("artifact: nil backend")
	}

	var helperDigest, aptDigest string
	var leaseDigests []string

	if strings.TrimSpace(opts.HelperDestDir) != "" {
		helperDigest = strings.TrimSpace(opts.HelperDigest)
		if helperDigest == "" {
			ver := strings.TrimSpace(opts.Version)
			if ver == "" {
				ver = "v1"
			}
			st, err := backend.LookupLogical(ctx, "podman-vm/helpers", ver)
			if err != nil {
				return nil, err
			}
			helperDigest = st.Record.Digest
		}
		leaseDigests = append(leaseDigests, helperDigest)
	}

	if strings.TrimSpace(opts.AptDestDir) != "" {
		aptDigest = strings.TrimSpace(opts.AptDigest)
		if aptDigest == "" {
			snap := strings.TrimSpace(opts.AptSnapshot)
			if snap == "" {
				return nil, errors.New("artifact: RestoreVMAndHelperAssets requires AptSnapshot or AptDigest when AptDestDir is set")
			}
			cleanSnap := normalizeOCILogicalRepo(snap)
			st, err := backend.LookupLogical(ctx, "apt-archive/"+cleanSnap, snap)
			if err != nil {
				return nil, err
			}
			aptDigest = st.Record.Digest
		}
		leaseDigests = append(leaseDigests, aptDigest)
	}

	if len(leaseDigests) == 0 {
		return &HelperRestoreResult{}, nil
	}

	res := &HelperRestoreResult{}
	err := backend.WithReadLease(ctx, 0, "", defaultExportLeaseTTL, leaseDigests, func(leaseCtx context.Context) error {
		if helperDigest != "" {
			raw, err := backend.FetchVerified(leaseCtx, helperDigest)
			if err != nil {
				return err
			}
			files, err := extractAssetTarBytesToDir(raw, opts.HelperDestDir)
			if err != nil {
				return err
			}
			res.HelperFiles = files
		}
		if aptDigest != "" {
			raw, err := backend.FetchVerified(leaseCtx, aptDigest)
			if err != nil {
				return err
			}
			files, err := extractAssetTarBytesToDir(raw, opts.AptDestDir)
			if err != nil {
				return err
			}
			res.AptFiles = files
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// LanguagePackCaptureOptions configures CaptureLanguagePacks.
type LanguagePackCaptureOptions struct {
	Profile  ProfileV1
	Version  string
	Platform string
	PackDeps map[string][]string // language -> dependency digests in CAS
}

// CaptureLanguagePacks records protected lang-pack roots in CAS for mandatory
// (go, bashsharp) and any configured packs in opts.Profile.Languages.
func CaptureLanguagePacks(ctx context.Context, backend Backend, opts LanguagePackCaptureOptions) ([]RootDescriptor, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if backend == nil {
		return nil, errors.New("artifact: nil backend")
	}

	platform := strings.TrimSpace(opts.Platform)
	if platform == "" {
		platform = strings.TrimSpace(opts.Profile.Platform)
	}
	if platform == "" {
		platform = CurrentPlatform()
	}
	version := strings.TrimSpace(opts.Version)
	if version == "" {
		version = "gen-1"
	}

	seen := make(map[string]bool)
	var langs []string
	for _, l := range append(slices.Clone(MandatoryLanguagePacks), opts.Profile.Languages...) {
		trimmed := strings.ToLower(strings.TrimSpace(l))
		if trimmed == "" || seen[trimmed] {
			continue
		}
		if !SupportedLanguagePack(trimmed) {
			return nil, fmt.Errorf("artifact: unsupported language pack %q", trimmed)
		}
		seen[trimmed] = true
		langs = append(langs, trimmed)
	}

	var roots []RootDescriptor
	for _, lang := range langs {
		deps := slices.Clone(opts.PackDeps[lang])
		for _, dep := range deps {
			if !ValidDigest(dep) {
				return nil, fmt.Errorf("artifact: invalid dependency digest %q for lang-pack %q", dep, lang)
			}
			if err := verifyBlobInBackend(ctx, backend, dep, -1, false); err != nil {
				return nil, fmt.Errorf("artifact: missing dependency %s for lang-pack %q: %w", dep, lang, err)
			}
		}
		lpBytes, err := json.Marshal(map[string]any{
			"schema":   "bashy-lang-pack-v1",
			"language": lang,
			"version":  version,
			"platform": platform,
			"deps":     deps,
		})
		if err != nil {
			return nil, err
		}
		lpDigest := DigestBytes(lpBytes)
		if _, err := backend.PutArtifact(ctx, CatalogRecordV1{
			SchemaVersion:  CatalogRecordSchemaV1,
			Digest:         lpDigest,
			Size:           int64(len(lpBytes)),
			MediaType:      MediaTypeLangPack,
			Platform:       platform,
			LogicalName:    "lang-pack/" + lang,
			LogicalVersion: version,
			Dependencies:   deps,
			Provenance:     "lang-pack-capture",
			Retention:      RetentionProtectedRoot,
			RetentionRoots: []string{"lang-pack:" + lang + "@" + version},
		}, bytes.NewReader(lpBytes)); err != nil {
			return nil, err
		}
		roots = append(roots, RootDescriptor{
			Kind:      "lang-pack",
			Name:      lang,
			Version:   version,
			Platform:  platform,
			MediaType: MediaTypeLangPack,
			Digest:    lpDigest,
			Size:      int64(len(lpBytes)),
			Deps:      deps,
		})
	}
	return roots, nil
}

// VerifyLanguagePacks checks whether every mandatory and profile-configured
// language pack (and its dependency blobs) is present and intact in backend.
func VerifyLanguagePacks(ctx context.Context, backend Backend, profile ProfileV1, version string) ([]string, []MissingArtifact, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	seen := make(map[string]bool)
	var required []string
	for _, l := range append(slices.Clone(MandatoryLanguagePacks), profile.Languages...) {
		trimmed := strings.ToLower(strings.TrimSpace(l))
		if trimmed != "" && !seen[trimmed] {
			seen[trimmed] = true
			required = append(required, trimmed)
		}
	}

	if backend == nil {
		var missing []MissingArtifact
		for _, lang := range required {
			missing = append(missing, MissingArtifact{
				Kind:   "lang-pack",
				Name:   lang,
				Reason: "artifact catalog unavailable",
			})
		}
		return nil, missing, nil
	}

	var recs []RecordStatus
	if rl, ok := backend.(interface {
		ListRecords(context.Context) ([]RecordStatus, error)
	}); ok {
		recs, _ = rl.ListRecords(ctx)
	}

	var verified []string
	var missing []MissingArtifact
	for _, lang := range required {
		var found *RecordStatus
		if strings.TrimSpace(version) != "" {
			if st, err := backend.LookupLogical(ctx, "lang-pack/"+lang, version); err == nil {
				found = &st
			}
		}
		if found == nil {
			for i := range recs {
				rs := &recs[i]
				if rs.Record.LogicalName == "lang-pack/"+lang && rs.PayloadPresent {
					if found == nil || rs.Record.CreatedAt.After(found.Record.CreatedAt) {
						found = rs
					}
				}
			}
		}
		if found == nil || !found.PayloadPresent {
			missing = append(missing, MissingArtifact{
				Kind:    "lang-pack",
				Name:    lang,
				Version: version,
				Reason:  fmt.Sprintf("language pack %q not present in artifact store", lang),
			})
			continue
		}
		if err := verifyBlobInBackend(ctx, backend, found.Record.Digest, found.Record.Size, true); err != nil {
			missing = append(missing, MissingArtifact{
				Kind:    "lang-pack",
				Name:    lang,
				Version: found.Record.LogicalVersion,
				Digest:  found.Record.Digest,
				Reason:  err.Error(),
			})
			continue
		}
		depMissing := false
		for _, dep := range found.Record.Dependencies {
			if err := verifyBlobInBackend(ctx, backend, dep, -1, true); err != nil {
				missing = append(missing, MissingArtifact{
					Kind:    "lang-pack-dep",
					Name:    lang,
					Version: found.Record.LogicalVersion,
					Digest:  dep,
					Reason:  err.Error(),
				})
				depMissing = true
			}
		}
		if !depMissing {
			verified = append(verified, lang)
		}
	}
	return verified, missing, nil
}
