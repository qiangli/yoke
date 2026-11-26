// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/mem"

	"github.com/qiangli/yoke/pkg/prune"
)

const (
	ModelEvalSchemaV1    = "bashy-model-eval-v1"
	OllamaPackSchemaV1   = "bashy-ollama-pack-v1"
	OllamaRunnerSchemaV1 = "bashy-ollama-runner-v1"

	MediaTypeOllamaPackV1   = "application/vnd.bashy.ollama.pack.v1+json"
	MediaTypeOllamaRunnerV1 = "application/vnd.bashy.ollama.runner.v1+json"
	MediaTypeOllamaEvalV1   = "application/vnd.bashy.ollama.eval.v1+json"

	defaultOllamaRegistryHost = "registry.ollama.ai"
	defaultOllamaNamespace    = "library"
	defaultOllamaTag          = "latest"
)

var (
	ErrHardwareNotReady = errors.New("artifact: host hardware insufficient for model profile")
	ErrModelEvalFailed  = errors.New("artifact: local model capability evaluation failed")
)

// OllamaRuntimePolicy captures the offline-safe runtime defaults for managed Ollama.
type OllamaRuntimePolicy struct {
	ContextLength   int  `json:"context_length"`
	MaxConcurrency  int  `json:"max_concurrency"`
	MaxLoadedModels int  `json:"max_loaded_models"`
	NoCloud         bool `json:"no_cloud"`
	NoAutoUpdate    bool `json:"no_auto_update"`
	NoAutoPull      bool `json:"no_auto_pull"`
	NoPrune         bool `json:"no_prune"`
}

// DefaultOllamaRuntimePolicy returns the locked offline-safe Ollama runtime configuration.
func DefaultOllamaRuntimePolicy() OllamaRuntimePolicy {
	return OllamaRuntimePolicy{
		ContextLength:   4096,
		MaxConcurrency:  1,
		MaxLoadedModels: 1,
		NoCloud:         true,
		NoAutoUpdate:    true,
		NoAutoPull:      true,
		NoPrune:         true,
	}
}

// WorkstationProfileV1 returns the workstation/server offline profile configuring
// larger coding candidates alongside the mandatory local embedding model.
func WorkstationProfileV1(platform string) ProfileV1 {
	if strings.TrimSpace(platform) == "" {
		platform = CurrentPlatform()
	}
	return ProfileV1{
		SchemaVersion: ProfileSchemaV1,
		Name:          "workstation",
		Platform:      platform,
		TierOrder:     DefaultTierOrder(),
		OfflineFloor:  TierUserland,
		Languages:     []string{"go", "bashsharp"},
		Models: []ModelProfileSpec{
			{
				Name:            "qwen3.5:9b",
				Role:            "coding",
				ContextLength:   4096,
				MaxConcurrency:  1,
				MaxLoadedModels: 1,
				RequiredEvals:   []string{"tool-call", "code-edit", "repair"},
				Required:        true,
			},
			{
				Name:            "qwen3-coder-next",
				Role:            "coding",
				ContextLength:   4096,
				MaxConcurrency:  1,
				MaxLoadedModels: 1,
				RequiredEvals:   []string{"tool-call", "code-edit", "repair"},
				Required:        false,
			},
			{
				Name:            "qwen3-embedding:0.6b",
				Role:            "embedding",
				ContextLength:   4096,
				MaxConcurrency:  1,
				MaxLoadedModels: 1,
				RequiredEvals:   []string{"retrieval"},
				Required:        true,
			},
		},
		Capacity:  CapacityPolicy{RebuildReserveBytes: 8 << 30},
		Resources: DefaultResourcePolicy(),
	}
}

// OllamaModelRef is a parsed, traversal-safe Ollama model reference.
type OllamaModelRef struct {
	Host      string `json:"host"`
	Namespace string `json:"namespace"`
	Model     string `json:"model"`
	Tag       string `json:"tag"`
}

func validModelSegment(seg string, allowColon bool) bool {
	if seg == "" || seg == "." || seg == ".." {
		return false
	}
	if strings.ContainsAny(seg, "/\\\x00") {
		return false
	}
	if !allowColon && strings.Contains(seg, ":") {
		return false
	}
	return true
}

// ParseOllamaModelRef parses an Ollama model tag (such as "qwen3.5:4b",
// "qwen3-coder-next", or "ns/model:tag") into canonical host/namespace/model/tag parts.
func ParseOllamaModelRef(raw string) (OllamaModelRef, error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "ollama/")
	if s == "" || strings.ContainsAny(s, "\\\x00") {
		return OllamaModelRef{}, fmt.Errorf("artifact: invalid ollama model reference %q", raw)
	}
	// Strip optional @digest suffix if present after tag/model.
	if at := strings.IndexByte(s, '@'); at >= 0 {
		s = s[:at]
	}

	tag := defaultOllamaTag
	lastSlash := strings.LastIndexByte(s, '/')
	lastColon := strings.LastIndexByte(s, ':')
	if lastColon > lastSlash {
		if lastColon == len(s)-1 {
			return OllamaModelRef{}, fmt.Errorf("artifact: empty tag in ollama model reference %q", raw)
		}
		tag = s[lastColon+1:]
		s = s[:lastColon]
	}

	parts := strings.Split(s, "/")
	ref := OllamaModelRef{
		Host:      defaultOllamaRegistryHost,
		Namespace: defaultOllamaNamespace,
		Tag:       tag,
	}
	switch len(parts) {
	case 1:
		ref.Model = parts[0]
	case 2:
		ref.Namespace = parts[0]
		ref.Model = parts[1]
	case 3:
		ref.Host = parts[0]
		ref.Namespace = parts[1]
		ref.Model = parts[2]
	default:
		return OllamaModelRef{}, fmt.Errorf("artifact: unsupported ollama model path %q", raw)
	}

	if !validModelSegment(ref.Host, true) ||
		!validModelSegment(ref.Namespace, false) ||
		!validModelSegment(ref.Model, false) ||
		!validModelSegment(ref.Tag, false) {
		return OllamaModelRef{}, fmt.Errorf("artifact: unsafe segment in ollama model reference %q", raw)
	}
	return ref, nil
}

// Canonical returns the short canonical model reference (e.g. "qwen3.5:4b").
func (r OllamaModelRef) Canonical() string {
	if r.Host == defaultOllamaRegistryHost && r.Namespace == defaultOllamaNamespace {
		return r.Model + ":" + r.Tag
	}
	if r.Host == defaultOllamaRegistryHost {
		return r.Namespace + "/" + r.Model + ":" + r.Tag
	}
	return r.Host + "/" + r.Namespace + "/" + r.Model + ":" + r.Tag
}

// ManifestRelPath returns the slash-separated manifest path relative to <OLLAMA_MODELS>.
func (r OllamaModelRef) ManifestRelPath() string {
	hostSeg := strings.ReplaceAll(r.Host, ":", "_")
	return "manifests/" + hostSeg + "/" + r.Namespace + "/" + r.Model + "/" + r.Tag
}

// SanitizedModelName returns the traversal-safe logical model suffix without tag.
func (r OllamaModelRef) SanitizedModelName() string {
	if r.Host == defaultOllamaRegistryHost && r.Namespace == defaultOllamaNamespace {
		return r.Model
	}
	if r.Host == defaultOllamaRegistryHost {
		return r.Namespace + "/" + r.Model
	}
	return strings.ReplaceAll(r.Host, ":", "_") + "/" + r.Namespace + "/" + r.Model
}

// LogicalName returns the catalog logical name for the model pack ("ollama/<model>").
func (r OllamaModelRef) LogicalName() string {
	return "ollama/" + r.SanitizedModelName()
}

// EvalLogicalName returns the catalog logical name for the model's evaluation record ("ollama-eval/<model>").
func (r OllamaModelRef) EvalLogicalName() string {
	return "ollama-eval/" + r.SanitizedModelName()
}

// OllamaLogicalName returns the canonical catalog logical name for modelRef.
func OllamaLogicalName(modelRef string) string {
	ref, err := ParseOllamaModelRef(modelRef)
	if err != nil {
		clean := strings.Trim(strings.ReplaceAll(strings.TrimSpace(modelRef), ":", "/"), "/")
		return "ollama/" + clean
	}
	return ref.LogicalName()
}

// OllamaEvalLogicalName returns the canonical evaluation logical name for modelRef.
func OllamaEvalLogicalName(modelRef string) string {
	ref, err := ParseOllamaModelRef(modelRef)
	if err != nil {
		clean := strings.Trim(strings.ReplaceAll(strings.TrimSpace(modelRef), ":", "/"), "/")
		return "ollama-eval/" + clean
	}
	return ref.EvalLogicalName()
}

// OllamaBlobFilename converts sha256:<64hex> to Ollama's on-disk blob filename sha256-<64hex>.
func OllamaBlobFilename(dgst string) (string, error) {
	if !ValidDigest(dgst) {
		return "", fmt.Errorf("artifact: invalid ollama blob digest %q", dgst)
	}
	return "sha256-" + strings.TrimPrefix(dgst, "sha256:"), nil
}

// OllamaManifestLayer describes one config or layer entry in an Ollama OCI manifest.
type OllamaManifestLayer struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	From      string `json:"from,omitempty"`
	Name      string `json:"name,omitempty"`
}

// OllamaDiskManifest matches the on-disk JSON manifest stored under <OLLAMA_MODELS>/manifests/...
type OllamaDiskManifest struct {
	SchemaVersion int                   `json:"schemaVersion"`
	MediaType     string                `json:"mediaType"`
	Config        OllamaManifestLayer   `json:"config"`
	Layers        []OllamaManifestLayer `json:"layers"`
}

// OllamaRunnerMetadataV1 pins the local inference runtime metadata alongside model weights.
type OllamaRunnerMetadataV1 struct {
	SchemaVersion   string `json:"schema_version"`
	Platform        string `json:"platform"`
	Backend         string `json:"backend"`
	RunnerDigest    string `json:"runner_digest,omitempty"`
	ContextLength   int    `json:"context_length"`
	MaxConcurrency  int    `json:"max_concurrency"`
	MaxLoadedModels int    `json:"max_loaded_models"`
	NoCloud         bool   `json:"no_cloud"`
	NoAutoUpdate    bool   `json:"no_auto_update"`
	NoAutoPull      bool   `json:"no_auto_pull"`
}

// OllamaPackV1 is the canonical protected model-bundle descriptor stored in CAS.
type OllamaPackV1 struct {
	SchemaVersion   string                `json:"schema_version"`
	Model           string                `json:"model"`
	ManifestRelPath string                `json:"manifest_rel_path"`
	ManifestRaw     []byte                `json:"manifest_raw"`
	ManifestDigest  string                `json:"manifest_digest"`
	Config          OllamaManifestLayer   `json:"config"`
	Layers          []OllamaManifestLayer `json:"layers"`
	RunnerDigest    string                `json:"runner_digest,omitempty"`
	WeightBytes     int64                 `json:"weight_bytes"`
	TotalBlobBytes  int64                 `json:"total_blob_bytes"`
}

func isOllamaWeightMediaType(mediaType string) bool {
	mt := strings.TrimSpace(mediaType)
	return strings.HasPrefix(mt, "application/vnd.ollama.image.model") ||
		mt == "application/vnd.ollama.image.tensor"
}

func verifyDiskBlob(blobPath string, expectedDigest string, expectedSize int64) (int64, error) {
	if !ValidDigest(expectedDigest) {
		return 0, fmt.Errorf("%w: invalid layer digest %q", ErrCorruptPayload, expectedDigest)
	}
	f, err := os.Open(blobPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, fmt.Errorf("%w: missing ollama blob %s", ErrIncompleteClosure, blobPath)
		}
		return 0, err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	if !fi.Mode().IsRegular() {
		return 0, fmt.Errorf("%w: %s is not a regular file", ErrCorruptPayload, blobPath)
	}
	if expectedSize > 0 && fi.Size() != expectedSize {
		return 0, fmt.Errorf("%w: %s size %d != manifest size %d", ErrCorruptPayload, blobPath, fi.Size(), expectedSize)
	}

	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return 0, err
	}
	if expectedSize > 0 && n != expectedSize {
		return 0, fmt.Errorf("%w: %s read %d != %d bytes", ErrCorruptPayload, blobPath, n, expectedSize)
	}
	got := "sha256:" + hex.EncodeToString(h.Sum(nil))
	if got != expectedDigest {
		return 0, fmt.Errorf("%w: %s hashed to %s, want %s", ErrCorruptPayload, blobPath, got, expectedDigest)
	}
	return n, nil
}

// CaptureOllamaModel verifies a complete on-disk Ollama model (manifest, config,
// GGUF/tensor weight layer(s), template/params/system/license/tokenizer layers,
// and runner metadata) and ingests all blobs into CAS as RetentionProtectedRoot.
func CaptureOllamaModel(ctx context.Context, backend Backend, modelsDir, modelRef string, runnerMeta *OllamaRunnerMetadataV1) (RootDescriptor, error) {
	if err := ctx.Err(); err != nil {
		return RootDescriptor{}, err
	}
	if backend == nil {
		return RootDescriptor{}, errors.New("artifact: nil backend for CaptureOllamaModel")
	}
	if strings.TrimSpace(modelsDir) == "" {
		return RootDescriptor{}, errors.New("artifact: empty ollama models directory")
	}

	ref, err := ParseOllamaModelRef(modelRef)
	if err != nil {
		return RootDescriptor{}, err
	}
	relPath := ref.ManifestRelPath()
	if !SafeLogicalPath(relPath) {
		return RootDescriptor{}, fmt.Errorf("artifact: unsafe ollama manifest relative path %q", relPath)
	}

	manifestDiskPath := filepath.Join(modelsDir, filepath.FromSlash(relPath))
	// Support fallback without ":latest" only via standard manifest path; also check raw host if needed.
	manifestRaw, err := os.ReadFile(manifestDiskPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return RootDescriptor{}, fmt.Errorf("%w: ollama manifest not found at %s", ErrNotFound, manifestDiskPath)
		}
		return RootDescriptor{}, err
	}
	if len(bytes.TrimSpace(manifestRaw)) == 0 {
		return RootDescriptor{}, fmt.Errorf("%w: empty ollama manifest at %s", ErrCorruptPayload, manifestDiskPath)
	}

	var parsed OllamaDiskManifest
	if err := json.Unmarshal(manifestRaw, &parsed); err != nil {
		return RootDescriptor{}, fmt.Errorf("%w: invalid ollama manifest JSON for %s: %v", ErrCorruptPayload, ref.Canonical(), err)
	}
	if !ValidDigest(parsed.Config.Digest) {
		return RootDescriptor{}, fmt.Errorf("%w: ollama manifest for %s missing valid config digest", ErrIncompleteClosure, ref.Canonical())
	}
	if len(parsed.Layers) == 0 {
		return RootDescriptor{}, fmt.Errorf("%w: ollama manifest for %s has no layers", ErrIncompleteClosure, ref.Canonical())
	}

	hasWeightLayer := false
	var weightBytes int64
	var totalBlobBytes int64

	type verifiedDiskLayer struct {
		layer    OllamaManifestLayer
		diskPath string
		size     int64
	}
	toIngest := make([]verifiedDiskLayer, 0, 1+len(parsed.Layers))

	cfgBlobName, err := OllamaBlobFilename(parsed.Config.Digest)
	if err != nil {
		return RootDescriptor{}, err
	}
	cfgDiskPath := filepath.Join(modelsDir, "blobs", cfgBlobName)
	cfgSize, err := verifyDiskBlob(cfgDiskPath, parsed.Config.Digest, parsed.Config.Size)
	if err != nil {
		return RootDescriptor{}, err
	}
	parsed.Config.Size = cfgSize
	totalBlobBytes += cfgSize
	toIngest = append(toIngest, verifiedDiskLayer{layer: parsed.Config, diskPath: cfgDiskPath, size: cfgSize})

	for i, l := range parsed.Layers {
		if !ValidDigest(l.Digest) {
			return RootDescriptor{}, fmt.Errorf("%w: layer[%d] of %s has invalid digest %q", ErrIncompleteClosure, i, ref.Canonical(), l.Digest)
		}
		blobName, err := OllamaBlobFilename(l.Digest)
		if err != nil {
			return RootDescriptor{}, err
		}
		blobPath := filepath.Join(modelsDir, "blobs", blobName)
		actualSize, err := verifyDiskBlob(blobPath, l.Digest, l.Size)
		if err != nil {
			return RootDescriptor{}, err
		}
		if actualSize == 0 && isOllamaWeightMediaType(l.MediaType) {
			return RootDescriptor{}, fmt.Errorf("%w: empty model weight blob %s for %s", ErrIncompleteClosure, l.Digest, ref.Canonical())
		}
		parsed.Layers[i].Size = actualSize
		totalBlobBytes += actualSize
		if isOllamaWeightMediaType(l.MediaType) {
			hasWeightLayer = true
			weightBytes += actualSize
		}
		toIngest = append(toIngest, verifiedDiskLayer{layer: parsed.Layers[i], diskPath: blobPath, size: actualSize})
	}
	if !hasWeightLayer {
		return RootDescriptor{}, fmt.Errorf("%w: manifest for %s has no model weight layer", ErrIncompleteClosure, ref.Canonical())
	}

	rootTag := "ollama-model:" + ref.Canonical()
	depSet := make(map[string]bool, len(toIngest)+1)

	for _, item := range toIngest {
		if err := ctx.Err(); err != nil {
			return RootDescriptor{}, err
		}
		mt := strings.TrimSpace(item.layer.MediaType)
		if mt == "" {
			mt = defaultMediaType
		}
		f, err := os.Open(item.diskPath)
		if err != nil {
			return RootDescriptor{}, err
		}
		rec := CatalogRecordV1{
			SchemaVersion:  CatalogRecordSchemaV1,
			Digest:         item.layer.Digest,
			Size:           item.size,
			MediaType:      mt,
			LogicalName:    "ollama-blob/" + strings.TrimPrefix(item.layer.Digest, "sha256:"),
			LogicalVersion: "v1",
			Provenance:     "ollama-models:" + ref.Canonical(),
			Retention:      RetentionProtectedRoot,
			RetentionRoots: []string{rootTag},
		}
		_, putErr := backend.PutArtifact(ctx, rec, f)
		_ = f.Close()
		if putErr != nil {
			return RootDescriptor{}, putErr
		}
		depSet[item.layer.Digest] = true
	}

	rm := OllamaRunnerMetadataV1{
		SchemaVersion:   OllamaRunnerSchemaV1,
		Platform:        CurrentPlatform(),
		Backend:         "embedded",
		ContextLength:   4096,
		MaxConcurrency:  1,
		MaxLoadedModels: 1,
		NoCloud:         true,
		NoAutoUpdate:    true,
		NoAutoPull:      true,
	}
	if runnerMeta != nil {
		rm = *runnerMeta
		if rm.SchemaVersion == "" {
			rm.SchemaVersion = OllamaRunnerSchemaV1
		}
		if strings.TrimSpace(rm.Platform) == "" {
			rm.Platform = CurrentPlatform()
		}
		if strings.TrimSpace(rm.Backend) == "" {
			rm.Backend = "embedded"
		}
		if rm.ContextLength <= 0 {
			rm.ContextLength = 4096
		}
		if rm.MaxConcurrency < 1 {
			rm.MaxConcurrency = 1
		}
		if rm.MaxLoadedModels < 1 {
			rm.MaxLoadedModels = 1
		}
	}
	rmBytes, err := json.Marshal(rm)
	if err != nil {
		return RootDescriptor{}, err
	}
	rmDigest := DigestBytes(rmBytes)
	if _, err := backend.PutArtifact(ctx, CatalogRecordV1{
		SchemaVersion:  CatalogRecordSchemaV1,
		Digest:         rmDigest,
		Size:           int64(len(rmBytes)),
		MediaType:      MediaTypeOllamaRunnerV1,
		Platform:       rm.Platform,
		LogicalName:    "ollama-runner/" + ref.SanitizedModelName(),
		LogicalVersion: ref.Tag,
		Provenance:     "ollama-runner",
		Retention:      RetentionProtectedRoot,
		RetentionRoots: []string{rootTag},
	}, bytes.NewReader(rmBytes)); err != nil {
		return RootDescriptor{}, err
	}
	depSet[rmDigest] = true

	allBlobDigests := make([]string, 0, len(depSet))
	for d := range depSet {
		allBlobDigests = append(allBlobDigests, d)
	}
	slices.Sort(allBlobDigests)

	pack := OllamaPackV1{
		SchemaVersion:   OllamaPackSchemaV1,
		Model:           ref.Canonical(),
		ManifestRelPath: relPath,
		ManifestRaw:     manifestRaw,
		ManifestDigest:  DigestBytes(manifestRaw),
		Config:          parsed.Config,
		Layers:          parsed.Layers,
		RunnerDigest:    rmDigest,
		WeightBytes:     weightBytes,
		TotalBlobBytes:  totalBlobBytes,
	}
	packBytes, err := json.Marshal(pack)
	if err != nil {
		return RootDescriptor{}, err
	}
	packDigest := DigestBytes(packBytes)

	if _, err := backend.PutArtifact(ctx, CatalogRecordV1{
		SchemaVersion:  CatalogRecordSchemaV1,
		Digest:         packDigest,
		Size:           int64(len(packBytes)),
		MediaType:      MediaTypeOllamaPackV1,
		Platform:       rm.Platform,
		LogicalName:    ref.LogicalName(),
		LogicalVersion: ref.Tag,
		Dependencies:   allBlobDigests,
		Provenance:     "ollama-models:" + ref.Canonical(),
		Retention:      RetentionProtectedRoot,
		RetentionRoots: []string{rootTag},
	}, bytes.NewReader(packBytes)); err != nil {
		return RootDescriptor{}, err
	}

	return RootDescriptor{
		Kind:      "ollama-model",
		Name:      ref.LogicalName(),
		Version:   ref.Tag,
		Platform:  rm.Platform,
		MediaType: MediaTypeOllamaPackV1,
		Digest:    packDigest,
		Size:      int64(len(packBytes)),
		Deps:      allBlobDigests,
	}, nil
}

// AppendManifestRoots appends or updates RootDescriptor entries onto the latest
// ManifestV1 in backend as a new additive generation when a manifest already exists.
// If backend has no manifests yet, it returns (nil, nil) without error.
func AppendManifestRoots(ctx context.Context, backend Backend, newRoots ...RootDescriptor) (*ManifestV1, error) {
	if len(newRoots) == 0 {
		return nil, nil
	}
	manifests, err := backend.ListManifests(ctx)
	if err != nil {
		return nil, err
	}
	if len(manifests) == 0 {
		return nil, nil
	}
	slices.SortFunc(manifests, func(a, b ManifestV1) int {
		if a.Generation < b.Generation {
			return -1
		}
		if a.Generation > b.Generation {
			return 1
		}
		return 0
	})
	latest := manifests[len(manifests)-1]
	mergedRoots := slices.Clone(latest.Roots)
	changed := false
	for _, nr := range newRoots {
		replaced := false
		for i, existing := range mergedRoots {
			if existing.Kind == nr.Kind && existing.Name == nr.Name && existing.Version == nr.Version {
				if existing.Digest != nr.Digest {
					mergedRoots[i] = nr
					changed = true
				}
				replaced = true
				break
			}
		}
		if !replaced {
			mergedRoots = append(mergedRoots, nr)
			changed = true
		}
	}
	if !changed {
		return &latest, nil
	}
	next := latest
	next.Generation = latest.Generation + 1
	next.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	next.Roots = mergedRoots
	if err := backend.PutManifest(ctx, next); err != nil {
		return nil, err
	}
	return &next, nil
}

// CanEvictOptionalModel enforces the Sprint 654 S5 model weight retention rule:
//   - Required model weights can NEVER be evicted (weights are not reconstructible from a model name).
//   - Optional (Required == false) model weights may ONLY be evicted when an explicit
//     policy-guaranteed accessible replica has been verified (verifiedAccessibleReplica == true).
func CanEvictOptionalModel(rec CatalogRecordV1, required bool, verifiedAccessibleReplica bool) (bool, string) {
	if required {
		return false, "required model weights are protected and never reconstructible from a model name"
	}
	if rec.Digest == "" || !ValidDigest(rec.Digest) {
		return false, "invalid catalog record digest"
	}
	if !verifiedAccessibleReplica {
		return false, "optional model eviction requires a policy-guaranteed accessible replica"
	}
	return true, "optional model has verified policy-guaranteed accessible replica"
}

// EvictOptionalModel evicts an optional (Required: false) Ollama model's blobs from a
// local *Store ONLY when verifiedAccessibleReplica == true and no required model or
// manifest generation depends on those blobs.
func EvictOptionalModel(ctx context.Context, store *Store, profile ProfileV1, modelRef string, verifiedAccessibleReplica bool) (prune.Counts, error) {
	if store == nil {
		return prune.Counts{}, errors.New("artifact: nil store for EvictOptionalModel")
	}
	ref, err := ParseOllamaModelRef(modelRef)
	if err != nil {
		return prune.Counts{}, err
	}

	required := false
	for _, m := range profile.Models {
		mRef, mErr := ParseOllamaModelRef(m.Name)
		if mErr == nil && mRef.Canonical() == ref.Canonical() && m.Required {
			required = true
			break
		}
	}

	packStatus, err := store.LookupLogical(ctx, ref.LogicalName(), ref.Tag)
	if err != nil {
		return prune.Counts{}, err
	}
	if ok, reason := CanEvictOptionalModel(packStatus.Record, required, verifiedAccessibleReplica); !ok {
		return prune.Counts{}, fmt.Errorf("%w: %s", ErrProtectedArtifact, reason)
	}

	// Determine blobs still referenced by any OTHER protected record or manifest root.
	allRecs, err := store.ListRecords(ctx)
	if err != nil {
		return prune.Counts{}, err
	}
	manifests, err := store.ListManifests(ctx)
	if err != nil {
		return prune.Counts{}, err
	}

	targetSet := make(map[string]bool)
	targetSet[packStatus.Record.Digest] = true
	for _, dep := range packStatus.Record.Dependencies {
		targetSet[dep] = true
	}

	stillProtected := make(map[string]bool)
	for _, m := range manifests {
		for _, r := range m.Roots {
			if r.Digest == packStatus.Record.Digest {
				// Pinned in a committed manifest generation -> protected by manifest closure.
				return prune.Counts{}, fmt.Errorf("%w: model %s is pinned in manifest generation %d", ErrProtectedArtifact, ref.Canonical(), m.Generation)
			}
			stillProtected[r.Digest] = true
			for _, dep := range r.Deps {
				stillProtected[dep] = true
			}
		}
	}
	rootTag := "ollama-model:" + ref.Canonical()
	for _, rs := range allRecs {
		if targetSet[rs.Record.Digest] {
			for _, rt := range rs.Record.RetentionRoots {
				if rt != rootTag {
					stillProtected[rs.Record.Digest] = true
				}
			}
			continue
		}
		if rs.Record.Retention == RetentionProtectedRoot {
			for _, dep := range rs.Record.Dependencies {
				stillProtected[dep] = true
			}
		}
	}

	// Demote exclusively-owned blobs to scratch in bbolt so ApplyPrune reclaims them cleanly.
	if err := store.demoteOptionalModelRecords(targetSet, stillProtected); err != nil {
		return prune.Counts{}, err
	}
	var pruneItems []prune.Item
	for dgst := range targetSet {
		if stillProtected[dgst] {
			continue
		}
		pruneItems = append(pruneItems, prune.Item{
			Noun:   "artifact",
			ID:     dgst,
			Reason: "optional ollama model evicted with verified accessible replica",
		})
	}
	slices.SortFunc(pruneItems, func(a, b prune.Item) int { return strings.Compare(a.ID, b.ID) })
	return store.ApplyPrune(ctx, pruneItems)
}

func (s *Store) demoteOptionalModelRecords(targetSet, stillProtected map[string]bool) error {
	if err := s.enterOp(); err != nil {
		return err
	}
	defer s.exitOp()
	if s.readOnly {
		return errors.New("artifact: store opened read-only")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.Begin(true)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	rb := tx.Bucket(bucketRecords)
	if rb == nil {
		return nil
	}
	for dgst := range targetSet {
		if stillProtected[dgst] {
			continue
		}
		raw := rb.Get([]byte(dgst))
		if raw == nil {
			continue
		}
		var st RecordStatus
		if err := json.Unmarshal(raw, &st); err != nil {
			return err
		}
		st.Record.Retention = RetentionScratch
		st.Record.RetentionRoots = nil
		st.Record.Dependencies = nil
		updated, err := json.Marshal(st)
		if err != nil {
			return err
		}
		if err := rb.Put([]byte(dgst), updated); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func writeStreamVerifiedAtomic(ctx context.Context, backend Backend, dgst string, expectedSize int64, dstPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, verifyErr := verifyDiskBlob(dstPath, dgst, expectedSize); verifyErr == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		return err
	}
	rc, err := backend.OpenBlob(ctx, dgst)
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(dstPath), ".restore-*")
	if err != nil {
		_ = rc.Close()
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}

	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(tmp, h), rc)
	closeRC := rc.Close()
	if copyErr != nil {
		cleanup()
		return copyErr
	}
	if closeRC != nil {
		cleanup()
		return closeRC
	}
	if expectedSize > 0 && n != expectedSize {
		cleanup()
		return fmt.Errorf("%w: restored %s size %d != %d", ErrCorruptPayload, dgst, n, expectedSize)
	}
	if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); got != dgst {
		cleanup()
		return fmt.Errorf("%w: restored %s hashed to %s", ErrCorruptPayload, dgst, got)
	}
	// Close handle before rename for Windows NTFS compatibility.
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, dstPath); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

func restorePackFromDigest(ctx context.Context, backend Backend, modelsDir, packDigest string) (*OllamaPackV1, error) {
	packBytes, err := backend.FetchVerified(ctx, packDigest)
	if err != nil {
		return nil, err
	}
	var pack OllamaPackV1
	if err := json.Unmarshal(packBytes, &pack); err != nil {
		return nil, fmt.Errorf("%w: decode ollama pack %s: %v", ErrCorruptPayload, packDigest, err)
	}
	if pack.SchemaVersion != OllamaPackSchemaV1 {
		return nil, fmt.Errorf("artifact: unsupported ollama pack schema %q", pack.SchemaVersion)
	}
	if !SafeLogicalPath(pack.ManifestRelPath) || !strings.HasPrefix(pack.ManifestRelPath, "manifests/") {
		return nil, fmt.Errorf("%w: unsafe manifest_rel_path %q", ErrCorruptPayload, pack.ManifestRelPath)
	}
	if len(pack.ManifestRaw) == 0 || DigestBytes(pack.ManifestRaw) != pack.ManifestDigest {
		return nil, fmt.Errorf("%w: manifest_raw digest mismatch in pack %s", ErrCorruptPayload, packDigest)
	}

	leaseSet := map[string]bool{packDigest: true}
	if ValidDigest(pack.Config.Digest) {
		leaseSet[pack.Config.Digest] = true
	}
	for _, l := range pack.Layers {
		if ValidDigest(l.Digest) {
			leaseSet[l.Digest] = true
		}
	}
	if ValidDigest(pack.RunnerDigest) {
		leaseSet[pack.RunnerDigest] = true
	}
	leaseDigests := make([]string, 0, len(leaseSet))
	for d := range leaseSet {
		leaseDigests = append(leaseDigests, d)
	}
	slices.Sort(leaseDigests)

	restoreFn := func(rctx context.Context) error {
		blobsDir := filepath.Join(modelsDir, "blobs")
		if err := os.MkdirAll(blobsDir, 0o755); err != nil {
			return err
		}
		allLayers := append([]OllamaManifestLayer{pack.Config}, pack.Layers...)
		for _, l := range allLayers {
			blobName, err := OllamaBlobFilename(l.Digest)
			if err != nil {
				return err
			}
			dstBlob := filepath.Join(blobsDir, blobName)
			if err := writeStreamVerifiedAtomic(rctx, backend, l.Digest, l.Size, dstBlob); err != nil {
				return err
			}
		}
		if ValidDigest(pack.RunnerDigest) {
			if _, err := backend.FetchVerified(rctx, pack.RunnerDigest); err != nil {
				return err
			}
		}
		manifestPath := filepath.Join(modelsDir, filepath.FromSlash(pack.ManifestRelPath))
		if err := os.MkdirAll(filepath.Dir(manifestPath), 0o755); err != nil {
			return err
		}
		return writeFileAtomic(manifestPath, pack.ManifestRaw, 0o644)
	}

	// Acquire a read lease when backend supports writes; fall back cleanly on read-only Store.
	if err := backend.WithReadLease(ctx, 0, "", 5*time.Minute, leaseDigests, restoreFn); err != nil {
		if strings.Contains(err.Error(), "read-only") {
			if err := restoreFn(ctx); err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	}
	return &pack, nil
}

// RestoreOllamaModel reconstructs an Ollama model's blobs and manifest under
// modelsDir from protected CAS payloads without network access.
func RestoreOllamaModel(ctx context.Context, backend Backend, modelsDir, modelRef string) (*OllamaPackV1, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if backend == nil {
		return nil, errors.New("artifact: nil backend for RestoreOllamaModel")
	}
	if strings.TrimSpace(modelsDir) == "" {
		return nil, errors.New("artifact: empty ollama models directory")
	}
	ref, err := ParseOllamaModelRef(modelRef)
	if err != nil {
		return nil, err
	}

	if st, lErr := backend.LookupLogical(ctx, ref.LogicalName(), ref.Tag); lErr == nil {
		return restorePackFromDigest(ctx, backend, modelsDir, st.Record.Digest)
	}

	// Fallback: search committed ManifestV1 roots matching modelRef.
	manifests, err := backend.ListManifests(ctx)
	if err == nil {
		for i := len(manifests) - 1; i >= 0; i-- {
			for _, r := range manifests[i].Roots {
				if modelMatchesRoot(ref.Canonical(), r) || modelMatchesRoot(modelRef, r) {
					return restorePackFromDigest(ctx, backend, modelsDir, r.Digest)
				}
			}
		}
	}
	return nil, fmt.Errorf("%w: ollama model %s", ErrNotFound, ref.Canonical())
}

// RestoreAllPinnedModels restores every pinned Ollama model in backend (across
// committed ManifestV1 roots and protected ollama-model pack records) into modelsDir.
func RestoreAllPinnedModels(ctx context.Context, backend Backend, modelsDir string) ([]OllamaPackV1, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if backend == nil {
		return nil, errors.New("artifact: nil backend for RestoreAllPinnedModels")
	}
	if strings.TrimSpace(modelsDir) == "" {
		return nil, errors.New("artifact: empty ollama models directory")
	}

	packDigests := make(map[string]bool)
	manifests, err := backend.ListManifests(ctx)
	if err != nil {
		return nil, err
	}
	for _, m := range manifests {
		for _, r := range m.Roots {
			if r.Kind == "ollama-model" && ValidDigest(r.Digest) {
				packDigests[r.Digest] = true
			}
		}
	}
	if rl, ok := backend.(interface {
		ListRecords(context.Context) ([]RecordStatus, error)
	}); ok {
		if recs, rErr := rl.ListRecords(ctx); rErr == nil {
			for _, rs := range recs {
				if rs.PayloadPresent && rs.Record.MediaType == MediaTypeOllamaPackV1 && rs.Record.Retention == RetentionProtectedRoot {
					packDigests[rs.Record.Digest] = true
				}
			}
		}
	}

	sorted := make([]string, 0, len(packDigests))
	for d := range packDigests {
		sorted = append(sorted, d)
	}
	slices.Sort(sorted)

	restored := make([]OllamaPackV1, 0, len(sorted))
	for _, d := range sorted {
		pack, err := restorePackFromDigest(ctx, backend, modelsDir, d)
		if err != nil {
			return nil, err
		}
		restored = append(restored, *pack)
	}
	slices.SortFunc(restored, func(a, b OllamaPackV1) int {
		return strings.Compare(a.Model, b.Model)
	})
	return restored, nil
}

// HardwareInfo summarizes host memory, optional accelerator VRAM, and free disk bytes.
type HardwareInfo struct {
	TotalMemoryBytes     uint64 `json:"total_memory_bytes"`
	AvailableMemoryBytes uint64 `json:"available_memory_bytes"`
	VRAMBytes            uint64 `json:"vram_bytes,omitempty"`
	FreeDiskBytes        uint64 `json:"free_disk_bytes,omitempty"`
}

// HardwareProbe inspects host memory/VRAM/disk headroom for local model evaluation.
type HardwareProbe func(ctx context.Context, path string) (HardwareInfo, error)

// DefaultHardwareProbe queries host RAM via gopsutil and disk headroom via DiskFreeBytes.
func DefaultHardwareProbe(ctx context.Context, path string) (HardwareInfo, error) {
	var info HardwareInfo
	if vm, err := mem.VirtualMemoryWithContext(ctx); err == nil && vm != nil {
		info.TotalMemoryBytes = vm.Total
		info.AvailableMemoryBytes = vm.Available
		if info.AvailableMemoryBytes == 0 && vm.Total > 0 {
			info.AvailableMemoryBytes = vm.Total / 2
		}
	}
	if free, err := DiskFreeBytes(path); err == nil {
		info.FreeDiskBytes = free
	}
	return info, nil
}

// RequiredCapabilitiesForModel returns the mandatory evaluation capability names for m.
func RequiredCapabilitiesForModel(m ModelProfileSpec) []string {
	if len(m.RequiredEvals) > 0 {
		out := slices.Clone(m.RequiredEvals)
		slices.Sort(out)
		return slices.Compact(out)
	}
	switch strings.TrimSpace(m.Role) {
	case "embedding":
		return []string{"retrieval"}
	default:
		return []string{"code-edit", "repair", "tool-call"}
	}
}

// ModelEvalRecordV1 records hardware readiness and deterministic capability evals in CAS.
type ModelEvalRecordV1 struct {
	SchemaVersion      string            `json:"schema_version"`
	Model              string            `json:"model"`
	Role               string            `json:"role"`
	PackDigest         string            `json:"pack_digest,omitempty"`
	HardwareReady      bool              `json:"hardware_ready"`
	HardwareReason     string            `json:"hardware_reason,omitempty"`
	Hardware           HardwareInfo      `json:"hardware"`
	RequiredEvals      []string          `json:"required_evals"`
	PassedCapabilities []string          `json:"passed_capabilities"`
	FailedCapabilities map[string]string `json:"failed_capabilities,omitempty"`
	EvaluatedAt        string            `json:"evaluated_at"`
}

// PassedAll reports whether r passed hardware readiness and every required capability.
func (r ModelEvalRecordV1) PassedAll(requiredCaps []string, expectedPackDigest string) (bool, string) {
	if r.SchemaVersion != ModelEvalSchemaV1 {
		return false, fmt.Sprintf("unsupported eval schema %q", r.SchemaVersion)
	}
	if !r.HardwareReady {
		if r.HardwareReason != "" {
			return false, "hardware not ready: " + r.HardwareReason
		}
		return false, "hardware not ready"
	}
	if expectedPackDigest != "" && r.PackDigest != "" && r.PackDigest != expectedPackDigest {
		return false, fmt.Sprintf("eval pack_digest %s != manifest model digest %s", r.PackDigest, expectedPackDigest)
	}
	for _, capName := range requiredCaps {
		if !slices.Contains(r.PassedCapabilities, capName) {
			if why, ok := r.FailedCapabilities[capName]; ok && why != "" {
				return false, fmt.Sprintf("capability %q failed: %s", capName, why)
			}
			return false, fmt.Sprintf("missing required capability evaluation %q", capName)
		}
	}
	return true, ""
}

// EvaluateOllamaOptions configures EvaluateOllamaProfile.
type EvaluateOllamaOptions struct {
	Backend       Backend
	Profile       ProfileV1
	BaseURL       string
	ModelsDir     string
	ArtifactRoot  string
	HTTPClient    *http.Client
	HardwareProbe HardwareProbe
	Now           func() time.Time
}

func ollamaPostJSONDecode(ctx context.Context, client *http.Client, baseURL, apiPath string, reqBody any, respOut any) error {
	rawReq, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}
	url := strings.TrimRight(strings.TrimSpace(baseURL), "/") + apiPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(rawReq))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, apiPath, strings.TrimSpace(string(body)))
	}
	if respOut != nil && len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, respOut); err != nil {
			return fmt.Errorf("decode %s response: %w", apiPath, err)
		}
	}
	return nil
}

func evalToolCall(ctx context.Context, client *http.Client, baseURL, model string, numCtx int) error {
	if numCtx <= 0 {
		numCtx = 4096
	}
	payload := map[string]any{
		"model":  model,
		"stream": false,
		"options": map[string]any{
			"temperature": 0,
			"num_ctx":     numCtx,
		},
		"messages": []map[string]any{
			{"role": "user", "content": "Read the file main.go using the read_file tool."},
		},
		"tools": []map[string]any{
			{
				"type": "function",
				"function": map[string]any{
					"name":        "read_file",
					"description": "Read a file from the workspace",
					"parameters": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"path": map[string]any{"type": "string"},
						},
						"required": []string{"path"},
					},
				},
			},
		},
	}
	var resp struct {
		Message struct {
			ToolCalls []struct {
				Function struct {
					Name      string         `json:"name"`
					Arguments map[string]any `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
	}
	if err := ollamaPostJSONDecode(ctx, client, baseURL, "/api/chat", payload, &resp); err != nil {
		return err
	}
	if len(resp.Message.ToolCalls) == 0 {
		return errors.New("model returned zero tool_calls")
	}
	tc := resp.Message.ToolCalls[0].Function
	if strings.TrimSpace(tc.Name) == "" {
		return errors.New("tool_call missing function.name")
	}
	if len(tc.Arguments) == 0 {
		return errors.New("tool_call missing structured arguments")
	}
	return nil
}

func evalCodeEdit(ctx context.Context, client *http.Client, baseURL, model string, numCtx int) error {
	if numCtx <= 0 {
		numCtx = 4096
	}
	payload := map[string]any{
		"model":  model,
		"stream": false,
		"options": map[string]any{
			"temperature": 0,
			"num_ctx":     numCtx,
		},
		"prompt": "Patch this Go function so Add returns a + b instead of a - b: func Add(a, b int) int { return a - b }",
	}
	var resp struct {
		Response string `json:"response"`
	}
	if err := ollamaPostJSONDecode(ctx, client, baseURL, "/api/generate", payload, &resp); err != nil {
		return err
	}
	out := strings.TrimSpace(resp.Response)
	if out == "" || (!strings.Contains(out, "+") && !strings.Contains(out, "return")) {
		return fmt.Errorf("unexpected code-edit output %q", out)
	}
	return nil
}

func evalCodeRepair(ctx context.Context, client *http.Client, baseURL, model string, numCtx int) error {
	if numCtx <= 0 {
		numCtx = 4096
	}
	payload := map[string]any{
		"model":  model,
		"stream": false,
		"options": map[string]any{
			"temperature": 0,
			"num_ctx":     numCtx,
		},
		"prompt": "Repair Go compile error `undefined: fmt` in: package main\nfunc main() { fmt.Println(\"ready\") }",
	}
	var resp struct {
		Response string `json:"response"`
	}
	if err := ollamaPostJSONDecode(ctx, client, baseURL, "/api/generate", payload, &resp); err != nil {
		return err
	}
	out := strings.TrimSpace(resp.Response)
	if out == "" || (!strings.Contains(out, "import") && !strings.Contains(out, "\"fmt\"")) {
		return fmt.Errorf("unexpected repair output %q", out)
	}
	return nil
}

func cosineSimilarity(a, b []float64) (float64, error) {
	if len(a) == 0 || len(a) != len(b) {
		return 0, fmt.Errorf("invalid embedding lengths %d vs %d", len(a), len(b))
	}
	var dot, normA, normB float64
	for i := range a {
		dot += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}
	if normA == 0 || normB == 0 {
		return 0, errors.New("zero-magnitude embedding vector")
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB)), nil
}

func evalEmbeddingRetrieval(ctx context.Context, client *http.Client, baseURL, model string, numCtx int) error {
	if numCtx <= 0 {
		numCtx = 4096
	}
	payload := map[string]any{
		"model": model,
		"input": []string{
			"verify sha256 blob digest before committing offline artifact catalog",
			"content-addressed CAS verifies sha256 checksums for protected offline artifacts",
			"roast seasonal vegetables with olive oil and rosemary at 425 degrees",
		},
		"options": map[string]any{
			"num_ctx": numCtx,
		},
	}
	var resp struct {
		Embeddings [][]float64 `json:"embeddings"`
	}
	if err := ollamaPostJSONDecode(ctx, client, baseURL, "/api/embed", payload, &resp); err != nil {
		return err
	}
	if len(resp.Embeddings) < 3 {
		return fmt.Errorf("expected 3 embedding vectors, got %d", len(resp.Embeddings))
	}
	simMatch, err := cosineSimilarity(resp.Embeddings[0], resp.Embeddings[1])
	if err != nil {
		return err
	}
	simUnrelated, err := cosineSimilarity(resp.Embeddings[0], resp.Embeddings[2])
	if err != nil {
		return err
	}
	if !(simMatch > simUnrelated) {
		return fmt.Errorf("retrieval ranking failed: cosine(query, match)=%.4f <= cosine(query, unrelated)=%.4f", simMatch, simUnrelated)
	}
	return nil
}

// EvaluateOllamaProfile checks host hardware headroom and runs deterministic local
// capability evaluations (`tool-call`, `code-edit`, `repair`, `retrieval`) against
// opts.BaseURL, persisting a protected ModelEvalRecordV1 per evaluated model in CAS.
func EvaluateOllamaProfile(ctx context.Context, opts EvaluateOllamaOptions) ([]ModelEvalRecordV1, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opts.Backend == nil {
		return nil, errors.New("artifact: nil backend for EvaluateOllamaProfile")
	}
	if err := opts.Profile.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(opts.BaseURL) == "" {
		return nil, errors.New("artifact: empty ollama base URL for evaluation")
	}

	probe := opts.HardwareProbe
	if probe == nil {
		probe = DefaultHardwareProbe
	}
	probePath := strings.TrimSpace(opts.ArtifactRoot)
	if probePath == "" {
		probePath = strings.TrimSpace(opts.ModelsDir)
	}
	if probePath == "" {
		probePath = DefaultRoot()
	}
	hw, err := probe(ctx, probePath)
	if err != nil {
		return nil, fmt.Errorf("artifact: probe hardware: %w", err)
	}

	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Minute}
	}
	nowFn := time.Now
	if opts.Now != nil {
		nowFn = opts.Now
	}

	var results []ModelEvalRecordV1
	var evalErrs []error

	for _, m := range opts.Profile.Models {
		ref, err := ParseOllamaModelRef(m.Name)
		if err != nil {
			if m.Required {
				return results, err
			}
			continue
		}

		// Look up captured pack in CAS to bind PackDigest and weight byte requirements.
		var packDigest string
		var weightBytes int64
		if st, lErr := opts.Backend.LookupLogical(ctx, ref.LogicalName(), ref.Tag); lErr == nil {
			packDigest = st.Record.Digest
			if rawPack, fErr := opts.Backend.FetchVerified(ctx, packDigest); fErr == nil {
				var p OllamaPackV1
				if json.Unmarshal(rawPack, &p) == nil {
					weightBytes = p.WeightBytes
				}
			}
		} else if !m.Required {
			// Optional model not captured locally -> skip cleanly.
			continue
		}

		reqCaps := RequiredCapabilitiesForModel(m)
		rec := ModelEvalRecordV1{
			SchemaVersion:      ModelEvalSchemaV1,
			Model:              ref.Canonical(),
			Role:               m.Role,
			PackDigest:         packDigest,
			HardwareReady:      true,
			Hardware:           hw,
			RequiredEvals:      reqCaps,
			PassedCapabilities: []string{},
			EvaluatedAt:        nowFn().UTC().Format(time.RFC3339),
		}

		requiredMem := uint64(weightBytes) + uint64(weightBytes)/8
		if requiredMem == 0 {
			requiredMem = 1024
		}
		effectiveMem := hw.AvailableMemoryBytes
		if hw.VRAMBytes > effectiveMem {
			effectiveMem = hw.VRAMBytes
		}
		if effectiveMem < requiredMem {
			rec.HardwareReady = false
			rec.HardwareReason = fmt.Sprintf("available memory/VRAM %d bytes < required %d bytes for %s", effectiveMem, requiredMem, ref.Canonical())
		}

		failed := make(map[string]string)
		if rec.HardwareReady {
			for _, capName := range reqCaps {
				var cErr error
				switch capName {
				case "tool-call":
					cErr = evalToolCall(ctx, client, opts.BaseURL, m.Name, m.ContextLength)
				case "code-edit":
					cErr = evalCodeEdit(ctx, client, opts.BaseURL, m.Name, m.ContextLength)
				case "repair":
					cErr = evalCodeRepair(ctx, client, opts.BaseURL, m.Name, m.ContextLength)
				case "retrieval":
					cErr = evalEmbeddingRetrieval(ctx, client, opts.BaseURL, m.Name, m.ContextLength)
				default:
					cErr = fmt.Errorf("unknown capability %q", capName)
				}
				if cErr != nil {
					failed[capName] = cErr.Error()
				} else {
					rec.PassedCapabilities = append(rec.PassedCapabilities, capName)
				}
			}
		}
		if len(failed) > 0 {
			rec.FailedCapabilities = failed
		}
		slices.Sort(rec.PassedCapabilities)

		recBytes, err := json.Marshal(rec)
		if err != nil {
			return results, err
		}
		recDigest := DigestBytes(recBytes)
		var deps []string
		if ValidDigest(packDigest) {
			deps = []string{packDigest}
		}
		if _, err := opts.Backend.PutArtifact(ctx, CatalogRecordV1{
			SchemaVersion:  CatalogRecordSchemaV1,
			Digest:         recDigest,
			Size:           int64(len(recBytes)),
			MediaType:      MediaTypeOllamaEvalV1,
			LogicalName:    ref.EvalLogicalName(),
			LogicalVersion: ref.Tag,
			Dependencies:   deps,
			Provenance:     "ollama-eval:" + ref.Canonical(),
			Retention:      RetentionProtectedRoot,
			RetentionRoots: []string{"ollama-eval:" + ref.Canonical()},
		}, bytes.NewReader(recBytes)); err != nil {
			return results, err
		}

		results = append(results, rec)
		if m.Required {
			if !rec.HardwareReady {
				evalErrs = append(evalErrs, fmt.Errorf("%w: %s", ErrHardwareNotReady, rec.HardwareReason))
			} else if ok, why := rec.PassedAll(reqCaps, packDigest); !ok {
				evalErrs = append(evalErrs, fmt.Errorf("%w: %s: %s", ErrModelEvalFailed, ref.Canonical(), why))
			}
		}
	}

	if len(evalErrs) > 0 {
		return results, errors.Join(evalErrs...)
	}
	return results, nil
}

// checkRequiredModelEval verifies that backend holds an intact, passing ModelEvalRecordV1
// for m matching expectedPackDigest. Returns "" on pass, or a descriptive failure reason.
func checkRequiredModelEval(ctx context.Context, backend Backend, m ModelProfileSpec, expectedPackDigest string) string {
	if backend == nil {
		return "artifact catalog unavailable"
	}
	ref, err := ParseOllamaModelRef(m.Name)
	if err != nil {
		return err.Error()
	}
	st, err := backend.LookupLogical(ctx, ref.EvalLogicalName(), ref.Tag)
	if err != nil {
		return fmt.Sprintf("required %s model %q has not completed local hardware/capability evaluation", m.Role, m.Name)
	}
	raw, err := backend.FetchVerified(ctx, st.Record.Digest)
	if err != nil {
		return fmt.Sprintf("corrupt or unreadable model evaluation record: %v", err)
	}
	var evalRec ModelEvalRecordV1
	if err := json.Unmarshal(raw, &evalRec); err != nil {
		return fmt.Sprintf("invalid model evaluation JSON: %v", err)
	}
	if ok, why := evalRec.PassedAll(RequiredCapabilitiesForModel(m), expectedPackDigest); !ok {
		return why
	}
	return ""
}

// LoadedOllamaModel describes a model currently resident in Ollama RAM/VRAM from GET /api/ps.
type LoadedOllamaModel struct {
	Name     string `json:"name"`
	Model    string `json:"model"`
	Size     int64  `json:"size"`
	SizeVRAM int64  `json:"size_vram"`
}

// ListLoadedOllamaModels queries GET <baseURL>/api/ps for models currently loaded in memory.
func ListLoadedOllamaModels(ctx context.Context, baseURL string, httpClient *http.Client) ([]LoadedOllamaModel, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, errors.New("artifact: empty ollama base URL")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	url := strings.TrimRight(strings.TrimSpace(baseURL), "/") + "/api/ps"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama /api/ps: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var parsed struct {
		Models []LoadedOllamaModel `json:"models"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}
	return parsed.Models, nil
}

// UnloadOllamaModelMemory instructs Ollama to evict modelName from RAM/VRAM
// (`keep_alive: 0`) while leaving <OLLAMA_MODELS>/blobs and protected CAS roots intact.
func UnloadOllamaModelMemory(ctx context.Context, baseURL, modelName string, httpClient *http.Client) error {
	modelName = strings.TrimSpace(modelName)
	if modelName == "" {
		return errors.New("artifact: empty model name for memory unload")
	}
	if strings.TrimSpace(baseURL) == "" {
		return errors.New("artifact: empty ollama base URL")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}

	genErr := ollamaPostJSONDecode(ctx, httpClient, baseURL, "/api/generate", map[string]any{
		"model":      modelName,
		"keep_alive": 0,
		"stream":     false,
	}, nil)
	if genErr == nil {
		return nil
	}
	// Fallback for embedding-only models that reject /api/generate.
	if embErr := ollamaPostJSONDecode(ctx, httpClient, baseURL, "/api/embed", map[string]any{
		"model":      modelName,
		"input":      "",
		"keep_alive": 0,
	}, nil); embErr == nil {
		return nil
	}
	return genErr
}

// UnloadAllLoadedModels queries GET /api/ps and unloads every resident model from RAM/VRAM
// without deleting any on-disk or CAS weight blobs.
func UnloadAllLoadedModels(ctx context.Context, baseURL string, httpClient *http.Client) ([]string, error) {
	loaded, err := ListLoadedOllamaModels(ctx, baseURL, httpClient)
	if err != nil {
		return nil, err
	}
	unloaded := make([]string, 0, len(loaded))
	for _, m := range loaded {
		name := strings.TrimSpace(m.Name)
		if name == "" {
			name = strings.TrimSpace(m.Model)
		}
		if name == "" {
			continue
		}
		if err := UnloadOllamaModelMemory(ctx, baseURL, name, httpClient); err != nil {
			return unloaded, err
		}
		unloaded = append(unloaded, name)
	}
	return unloaded, nil
}

// TierPlacementDecision records the structured tier selection and fallback trace
// produced by ResolveWorkloadTier.
type TierPlacementDecision struct {
	SelectedTier  Tier                 `json:"selected_tier,omitempty"`
	Sealed        bool                 `json:"sealed"`
	Floor         Tier                 `json:"floor"`
	TierOrder     []Tier               `json:"tier_order"`
	ExcludedTiers []Tier               `json:"excluded_tiers,omitempty"`
	FallbackTrace []string             `json:"fallback_trace,omitempty"`
	Requirements  WorkloadRequirements `json:"requirements"`
}

// ResolveWorkloadTier evaluates workload tier placement across profile.TierOrder down
// to profile.OfflineFloor, excluding external cloud when sealed, preserving confinement,
// and failing explicitly with ErrOfflineFloorUnavailable or ErrConfinementViolation.
func ResolveWorkloadTier(profile ProfileV1, root string, req WorkloadRequirements, status map[Tier]TierStatus) (TierPlacementDecision, error) {
	if len(profile.TierOrder) == 0 {
		profile.TierOrder = DefaultTierOrder()
	}
	if profile.OfflineFloor == "" {
		profile.OfflineFloor = TierUserland
	}
	sealed := IsSealed(root)
	decision := TierPlacementDecision{
		Sealed:       sealed,
		Floor:        profile.OfflineFloor,
		TierOrder:    slices.Clone(profile.TierOrder),
		Requirements: req,
	}

	if err := profile.Validate(); err != nil {
		return decision, err
	}

	for _, tier := range profile.TierOrder {
		if sealed && tier == TierCloud {
			decision.ExcludedTiers = append(decision.ExcludedTiers, tier)
			decision.FallbackTrace = append(decision.FallbackTrace, "cloud: excluded in sealed mode")
			if tier == profile.OfflineFloor {
				break
			}
			continue
		}
		st, ok := status[tier]
		var rejectReason string
		switch {
		case !ok || !st.Available:
			why := st.UnavailableReason
			if why == "" {
				why = "unavailable"
			}
			rejectReason = string(tier) + ": " + why
		case req.RequireSandbox && !st.ProvidesSandbox:
			rejectReason = string(tier) + ": lacks required sandbox confinement"
		case req.TargetOS != "" && st.OS != "" && st.OS != req.TargetOS:
			rejectReason = fmt.Sprintf("%s: target_os=%s != %s", tier, req.TargetOS, st.OS)
		case req.TargetArch != "" && st.Arch != "" && st.Arch != req.TargetArch:
			rejectReason = fmt.Sprintf("%s: target_arch=%s != %s", tier, req.TargetArch, st.Arch)
		case req.RequireGPU && !st.ProvidesGPU:
			rejectReason = string(tier) + ": lacks required GPU"
		case req.MemoryBytes > 0 && st.AvailableMemoryBytes < req.MemoryBytes:
			rejectReason = fmt.Sprintf("%s: memory %d < required %d", tier, st.AvailableMemoryBytes, req.MemoryBytes)
		}
		if rejectReason == "" {
			break
		}
		decision.ExcludedTiers = append(decision.ExcludedTiers, tier)
		decision.FallbackTrace = append(decision.FallbackTrace, rejectReason)
		if tier == profile.OfflineFloor {
			break
		}
	}

	selected, err := SelectTier(profile, sealed, req, status)
	if err != nil {
		return decision, err
	}
	decision.SelectedTier = selected
	return decision, nil
}
