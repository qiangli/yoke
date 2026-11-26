// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

const (
	sealStateFile    = "seal.json"
	savedProfileFile = "profile.json"
)

func nearestExistingDir(p string) (string, error) {
	cleaned := strings.TrimSpace(p)
	if cleaned == "" {
		cleaned = "."
	}
	abs, err := filepath.Abs(cleaned)
	if err != nil {
		return "", fmt.Errorf("artifact: resolve path %q: %w", p, err)
	}
	cur := abs
	for {
		fi, err := os.Stat(cur)
		if err == nil {
			if !fi.IsDir() {
				return filepath.Dir(cur), nil
			}
			return cur, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("artifact: stat %s: %w", cur, err)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return cur, nil
		}
		cur = parent
	}
}

// CheckCapacityReserve verifies that available free space at path covers
// additionalClosureBytes + reserveBytes, returning ErrInsufficientCapacity on shortfall.
func CheckCapacityReserve(path string, additionalClosureBytes uint64, reserveBytes uint64, diskFreeFn func(string) (uint64, error)) (uint64, error) {
	if diskFreeFn == nil {
		diskFreeFn = DiskFreeBytes
	}
	target := strings.TrimSpace(path)
	if target == "" {
		target = DefaultRoot()
	}
	freeBytes, err := diskFreeFn(target)
	if err != nil {
		return 0, err
	}
	required := additionalClosureBytes + reserveBytes
	if required < additionalClosureBytes {
		required = ^uint64(0)
	}
	if freeBytes < required {
		return freeBytes, fmt.Errorf("%w: free=%d bytes < required=%d bytes (closure=%d + reserve=%d) at %s",
			ErrInsufficientCapacity, freeBytes, required, additionalClosureBytes, reserveBytes, target)
	}
	return freeBytes, nil
}

// SealStateV1 records the durable sealed offline activation state at <root>/seal.json.
type SealStateV1 struct {
	SchemaVersion      string `json:"schema_version"`
	Sealed             bool   `json:"sealed"`
	SealedAt           string `json:"sealed_at,omitempty"`
	ActiveGeneration   uint64 `json:"active_generation,omitempty"`
	BaselineGeneration uint64 `json:"baseline_generation,omitempty"`
	ProfileName        string `json:"profile_name,omitempty"`
	ProfileDigest      string `json:"profile_digest,omitempty"`
	ManifestDigest     string `json:"manifest_digest,omitempty"`
	Platform           string `json:"platform,omitempty"`
	GoToolchainVersion string `json:"go_toolchain_version,omitempty"`
	ProductOverlaySHA  string `json:"product_overlay_sha,omitempty"`
}

func resolveRoot(root string) string {
	r := strings.TrimSpace(root)
	if r == "" {
		r = DefaultRoot()
	}
	if abs, err := filepath.Abs(r); err == nil {
		return abs
	}
	return r
}

func defaultLoomDirForRoot(root string) string {
	if d := strings.TrimSpace(os.Getenv("BASHY_LOOM_DIR")); d != "" {
		if abs, err := filepath.Abs(d); err == nil {
			return abs
		}
		return d
	}
	return filepath.Join(resolveRoot(root), "loom", "modules.git")
}

// LoadSealState reads <root>/seal.json if present.
func LoadSealState(root string) (SealStateV1, bool, error) {
	p := filepath.Join(resolveRoot(root), sealStateFile)
	raw, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return SealStateV1{}, false, nil
		}
		return SealStateV1{}, false, err
	}
	var st SealStateV1
	if err := json.Unmarshal(raw, &st); err != nil {
		return SealStateV1{}, false, fmt.Errorf("artifact: decode %s: %w", p, err)
	}
	if st.SchemaVersion != "" && st.SchemaVersion != SealStateSchemaV1 {
		return SealStateV1{}, false, fmt.Errorf("artifact: unsupported seal schema %q", st.SchemaVersion)
	}
	if st.SchemaVersion == "" {
		st.SchemaVersion = SealStateSchemaV1
	}
	return st, true, nil
}

// SaveSealState atomically writes <root>/seal.json with 0600 permissions.
func SaveSealState(root string, st SealStateV1) error {
	r := resolveRoot(root)
	if err := os.MkdirAll(r, 0o700); err != nil {
		return err
	}
	if st.SchemaVersion == "" {
		st.SchemaVersion = SealStateSchemaV1
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeFileAtomic(filepath.Join(r, sealStateFile), data, 0o600)
}

// IsSealed reports whether offline/sealed mode is active via BASHY_OFFLINE=1
// or a persisted <root>/seal.json with Sealed == true.
func IsSealed(root string) bool {
	if strings.TrimSpace(os.Getenv("BASHY_OFFLINE")) == "1" {
		return true
	}
	st, ok, err := LoadSealState(root)
	return err == nil && ok && st.Sealed
}

// SaveProfile persists p atomically at <root>/profile.json so subsequent
// status/verify/seal calls resolve custom profiles deterministically.
func SaveProfile(root string, p ProfileV1) error {
	if err := p.Validate(); err != nil {
		return err
	}
	r := resolveRoot(root)
	if err := os.MkdirAll(r, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeFileAtomic(filepath.Join(r, savedProfileFile), data, 0o600)
}

// LoadSavedProfile reads <root>/profile.json if present.
func LoadSavedProfile(root string) (ProfileV1, bool, error) {
	p := filepath.Join(resolveRoot(root), savedProfileFile)
	raw, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ProfileV1{}, false, nil
		}
		return ProfileV1{}, false, err
	}
	var prof ProfileV1
	if err := json.Unmarshal(raw, &prof); err != nil {
		return ProfileV1{}, false, err
	}
	if err := prof.Validate(); err != nil {
		return ProfileV1{}, false, err
	}
	return prof, true, nil
}

// ResolveProfile resolves a profile name ("default", "full", "workstation",
// "self-host", "self-host-v1") or JSON file path for platform.
func ResolveProfile(spec, platform string) (ProfileV1, error) {
	if strings.TrimSpace(platform) == "" {
		platform = CurrentPlatform()
	}
	switch strings.TrimSpace(spec) {
	case "", "default", ProfileScopeFull:
		p := DefaultProfileV1(platform)
		return p, p.Validate()
	case "workstation", "workstation-v1":
		p := WorkstationProfileV1(platform)
		return p, p.Validate()
	case ProfileScopeSelfHost, "self-host-v1":
		p := SelfHostProfileV1(platform)
		return p, p.Validate()
	default:
		raw, err := os.ReadFile(spec)
		if err != nil {
			return ProfileV1{}, fmt.Errorf("artifact: read profile %q: %w", spec, err)
		}
		var p ProfileV1
		if err := json.Unmarshal(raw, &p); err != nil {
			return ProfileV1{}, fmt.Errorf("artifact: parse profile %q: %w", spec, err)
		}
		if strings.TrimSpace(p.Platform) == "" {
			p.Platform = platform
		}
		return p, p.Validate()
	}
}

func resolveEffectiveProfile(root string, explicit *ProfileV1, profileSpec string, manifest *ManifestV1, sealState *SealStateV1) (ProfileV1, error) {
	platform := CurrentPlatform()
	if manifest != nil && validPlatform(manifest.Platform) {
		platform = manifest.Platform
	} else if sealState != nil && validPlatform(sealState.Platform) {
		platform = sealState.Platform
	}
	if explicit != nil {
		cp := *explicit
		if strings.TrimSpace(cp.Platform) == "" {
			cp.Platform = platform
		}
		return cp, cp.Validate()
	}
	if strings.TrimSpace(profileSpec) != "" {
		return ResolveProfile(profileSpec, platform)
	}
	if saved, ok, err := LoadSavedProfile(root); err == nil && ok {
		if manifest == nil || manifest.ProfileName == "" || saved.Name == manifest.ProfileName {
			return saved, nil
		}
	}
	name := ""
	if manifest != nil && manifest.ProfileName != "" {
		name = manifest.ProfileName
	} else if sealState != nil && sealState.ProfileName != "" {
		name = sealState.ProfileName
	}
	switch name {
	case "self-host-v1", ProfileScopeSelfHost:
		return SelfHostProfileV1(platform), nil
	case "workstation", "workstation-v1":
		return WorkstationProfileV1(platform), nil
	default:
		return DefaultProfileV1(platform), nil
	}
}

// MissingArtifact describes one absent, evicted, or corrupted artifact capability or blob.
type MissingArtifact struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	Digest  string `json:"digest,omitempty"`
	Reason  string `json:"reason"`
}

// SystemStatus summarizes offline readiness, seal state, generations, capacity, and missing closure items.
type SystemStatus struct {
	SchemaVersion       string            `json:"schema_version"`
	ArtifactRoot        string            `json:"artifact_root"`
	LoomDir             string            `json:"loom_dir"`
	Platform            string            `json:"platform"`
	Sealed              bool              `json:"sealed"`
	OfflineEnv          bool              `json:"offline_env"`
	SealState           *SealStateV1      `json:"seal_state,omitempty"`
	Profile             ProfileV1         `json:"profile"`
	ProfileDigest       string            `json:"profile_digest"`
	ActiveGeneration    uint64            `json:"active_generation,omitempty"`
	BaselineGeneration  uint64            `json:"baseline_generation,omitempty"`
	LatestGeneration    uint64            `json:"latest_generation,omitempty"`
	Manifest            *ManifestV1       `json:"manifest,omitempty"`
	ManifestDigest      string            `json:"manifest_digest,omitempty"`
	PublicationCount    int               `json:"publication_count"`
	RecordCount         int               `json:"record_count"`
	EvictedCount        int               `json:"evicted_count"`
	FreeBytes           uint64            `json:"free_bytes"`
	RebuildReserveBytes uint64            `json:"rebuild_reserve_bytes"`
	CapacityOK          bool              `json:"capacity_ok"`
	CapacityError       string            `json:"capacity_error,omitempty"`
	ClosureComplete     bool              `json:"closure_complete"`
	VerifiedDigests     bool              `json:"verified_digests"`
	MissingArtifacts    []MissingArtifact `json:"missing_artifacts"`
}

// InspectStatusOptions configures InspectStatus.
type InspectStatusOptions struct {
	Root          string
	LoomDir       string
	Backend       Backend
	Profile       *ProfileV1
	ProfileSpec   string
	Generation    uint64
	VerifyDigests bool
	DiskFreeFunc  func(string) (uint64, error)
}

// VerifyOptions configures VerifyClosure.
type VerifyOptions struct {
	Root          string
	LoomDir       string
	Backend       Backend
	Profile       *ProfileV1
	ProfileSpec   string
	Generation    uint64
	VerifyDigests bool
	DiskFreeFunc  func(string) (uint64, error)
}

// VerifyReport reports offline closure and cryptographic digest verification results.
type VerifyReport struct {
	Status          SystemStatus      `json:"status"`
	Complete        bool              `json:"complete"`
	VerifiedBlobs   int               `json:"verified_blobs"`
	VerifiedDigests bool              `json:"verified_digests"`
	Missing         []MissingArtifact `json:"missing"`
}

// SealOptions configures Seal.
type SealOptions struct {
	Root         string
	LoomDir      string
	Backend      Backend
	Profile      *ProfileV1
	ProfileSpec  string
	Generation   uint64
	Now          func() time.Time
	DiskFreeFunc func(string) (uint64, error)
}

func openReadBackendIfPresent(ctx context.Context, root string) (Backend, bool, error) {
	if client, ok := TryConnectService(ctx, root); ok {
		return client, true, nil
	}
	dbPath := filepath.Join(root, "catalog.db")
	fi, err := os.Stat(dbPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if fi.IsDir() {
		return nil, false, nil
	}
	st, err := OpenStore(ctx, StoreOptions{Root: root, ReadOnly: true})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return st, true, nil
}

func verifyBlobInBackend(ctx context.Context, backend Backend, dgst string, expectedSize int64, verifyDigests bool) error {
	if !ValidDigest(dgst) {
		return fmt.Errorf("invalid digest %q", dgst)
	}
	if verifyDigests {
		if expectedSize >= 0 && expectedSize <= 16<<20 {
			_, err := backend.FetchVerified(ctx, dgst)
			return err
		}
		rc, err := backend.OpenBlob(ctx, dgst)
		if err != nil {
			return err
		}
		defer rc.Close()
		h := sha256.New()
		n, err := io.Copy(h, rc)
		if err != nil {
			return err
		}
		if expectedSize >= 0 && n != expectedSize {
			return fmt.Errorf("%w: %s size %d != %d", ErrCorruptPayload, dgst, n, expectedSize)
		}
		if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); got != dgst {
			return fmt.Errorf("%w: %s hashed to %s", ErrCorruptPayload, dgst, got)
		}
		return nil
	}
	rc, err := backend.OpenBlob(ctx, dgst)
	if err != nil {
		return err
	}
	return rc.Close()
}

func verifyLoomPublicationCommit(loomDir string, pub PublicationRecord) error {
	repo, err := gogit.PlainOpen(loomDir)
	if err != nil {
		return fmt.Errorf("open loom repo %s: %w", loomDir, err)
	}
	ref, err := repo.Reference(plumbing.ReferenceName(pub.LoomRef), true)
	if err != nil {
		return fmt.Errorf("missing loom ref %s: %w", pub.LoomRef, err)
	}
	if ref.Hash().String() != pub.Commit {
		return fmt.Errorf("loom ref %s points to %s, want %s", pub.LoomRef, ref.Hash().String(), pub.Commit)
	}
	c, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return fmt.Errorf("missing loom commit %s: %w", pub.Commit, err)
	}
	if _, err := c.Tree(); err != nil {
		return fmt.Errorf("missing loom tree for %s: %w", pub.Commit, err)
	}
	return nil
}

func enumerateProfileSkeletonMissing(profile ProfileV1) []MissingArtifact {
	missing := []MissingArtifact{
		{Kind: "manifest", Name: profile.Name, Reason: "no bootstrap manifest recorded in artifact catalog"},
		{Kind: "gotoolchain", Name: "go", Reason: "go toolchain not bootstrapped"},
		{Kind: "gooverlay", Name: "productgo", Reason: "product go startup-signal overlay not bootstrapped"},
		{Kind: "source", Name: "bashy", Reason: "source tree not bootstrapped"},
	}
	for _, lang := range profile.Languages {
		lang = strings.TrimSpace(lang)
		if lang != "" {
			missing = append(missing, MissingArtifact{
				Kind:   "lang-pack",
				Name:   lang,
				Reason: "mandatory language pack not bootstrapped",
			})
		}
	}
	for _, m := range profile.Models {
		if m.Required {
			missing = append(missing, MissingArtifact{
				Kind:    "ollama-model",
				Name:    m.Name,
				Version: m.Role,
				Reason:  fmt.Sprintf("required %s model %q not bootstrapped", m.Role, m.Name),
			})
		}
	}
	return missing
}

func modelMatchesRoot(modelName string, r RootDescriptor) bool {
	if r.Kind != "ollama-model" {
		return false
	}
	want := strings.TrimSpace(modelName)
	if r.Name == want || strings.TrimPrefix(r.Name, "ollama/") == want {
		return true
	}
	if r.Name+":"+r.Version == want || strings.TrimPrefix(r.Name, "ollama/")+":"+r.Version == want {
		return true
	}
	return false
}

func inspectClosureInternal(ctx context.Context, opts InspectStatusOptions, preferLatestForSeal bool) (*SystemStatus, int, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
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
		return nil, 0, err
	}

	backend := opts.Backend
	closeBackend := false
	if backend == nil {
		var opened bool
		backend, opened, err = openReadBackendIfPresent(ctx, root)
		if err != nil {
			return nil, 0, err
		}
		closeBackend = opened
	}
	defer func() {
		if closeBackend && backend != nil {
			_ = backend.Close()
		}
	}()

	var (
		storeStatus    StoreStatus
		targetManifest *ManifestV1
		pubs           []PublicationRecord
		hasGoModRecs   bool
	)

	if backend != nil {
		if st, stErr := backend.Status(ctx); stErr == nil {
			storeStatus = st
		}
		manifests, mErr := backend.ListManifests(ctx)
		if mErr != nil {
			return nil, 0, mErr
		}
		if len(manifests) > 0 {
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
			storeStatus.LatestGeneration = latest.Generation

			switch {
			case opts.Generation > 0:
				m, err := backend.GetManifest(ctx, opts.Generation)
				if err == nil {
					targetManifest = &m
				}
			case !preferLatestForSeal && hasSealSt && sealSt.Sealed && sealSt.ActiveGeneration > 0:
				if m, err := backend.GetManifest(ctx, sealSt.ActiveGeneration); err == nil {
					targetManifest = &m
				} else {
					targetManifest = &latest
				}
			default:
				targetManifest = &latest
			}
		}
		pubs, err = backend.ListPublications(ctx)
		if err != nil {
			return nil, 0, err
		}
		if rl, ok := backend.(interface {
			ListRecords(context.Context) ([]RecordStatus, error)
		}); ok {
			if recs, rErr := rl.ListRecords(ctx); rErr == nil {
				for _, rs := range recs {
					if strings.HasPrefix(rs.Record.LogicalName, "gomod-") {
						hasGoModRecs = true
						break
					}
				}
			}
		}
	}

	var sealPtr *SealStateV1
	if hasSealSt {
		cp := sealSt
		sealPtr = &cp
	}

	profile, err := resolveEffectiveProfile(root, opts.Profile, opts.ProfileSpec, targetManifest, sealPtr)
	if err != nil {
		return nil, 0, err
	}
	profDigest, err := profile.Digest()
	if err != nil {
		return nil, 0, err
	}

	reserveBytes := profile.Capacity.RebuildReserveBytes
	if targetManifest != nil && targetManifest.RebuildReserveBytes > reserveBytes {
		reserveBytes = targetManifest.RebuildReserveBytes
	}

	freeBytes, capErr := CheckCapacityReserve(root, 0, reserveBytes, opts.DiskFreeFunc)
	capOK := capErr == nil
	capErrStr := ""
	if capErr != nil {
		capErrStr = capErr.Error()
	}

	var missing []MissingArtifact
	verifiedBlobs := 0
	verifiedSet := make(map[string]bool)

	checkDigest := func(dgst string, size int64, kind, name, version string) {
		if backend == nil {
			missing = append(missing, MissingArtifact{
				Kind: kind, Name: name, Version: version, Digest: dgst,
				Reason: "artifact catalog unavailable",
			})
			return
		}
		if verifiedSet[dgst] {
			return
		}
		if err := verifyBlobInBackend(ctx, backend, dgst, size, opts.VerifyDigests); err != nil {
			missing = append(missing, MissingArtifact{
				Kind: kind, Name: name, Version: version, Digest: dgst,
				Reason: err.Error(),
			})
			return
		}
		verifiedSet[dgst] = true
		verifiedBlobs++
	}

	if targetManifest == nil {
		if opts.Generation > 0 {
			missing = append(missing, MissingArtifact{
				Kind:    "manifest",
				Name:    profile.Name,
				Version: fmt.Sprintf("gen-%d", opts.Generation),
				Reason:  fmt.Sprintf("manifest generation %d not found", opts.Generation),
			})
		} else {
			missing = append(missing, enumerateProfileSkeletonMissing(profile)...)
		}
	} else {
		if err := targetManifest.Validate(); err != nil {
			missing = append(missing, MissingArtifact{
				Kind:    "manifest",
				Name:    targetManifest.ProfileName,
				Version: fmt.Sprintf("gen-%d", targetManifest.Generation),
				Reason:  err.Error(),
			})
		}
		// Check mandatory root kinds against ProfileV1.
		hasToolchain := false
		hasOverlay := false
		hasSource := false
		hasGoMod := false
		langFound := make(map[string]bool)

		for _, r := range targetManifest.Roots {
			switch r.Kind {
			case "gotoolchain":
				hasToolchain = true
			case "gooverlay":
				hasOverlay = true
			case "source":
				hasSource = true
			case "gomod":
				hasGoMod = true
			case "lang-pack":
				langFound[r.Name] = true
				langFound[strings.TrimPrefix(r.Name, "lang-pack/")] = true
			}
		}

		if !hasToolchain {
			missing = append(missing, MissingArtifact{
				Kind: "gotoolchain", Name: "go", Version: targetManifest.GoToolchainVersion,
				Reason: "missing go toolchain root in manifest",
			})
		}
		if !hasOverlay {
			missing = append(missing, MissingArtifact{
				Kind: "gooverlay", Name: "productgo", Version: targetManifest.GoToolchainVersion,
				Reason: "missing product go startup-signal overlay root in manifest",
			})
		} else {
			checkDigest(targetManifest.ProductOverlaySHA, -1, "gooverlay", "productgo", targetManifest.GoToolchainVersion)
		}
		if !hasSource {
			missing = append(missing, MissingArtifact{
				Kind: "source", Name: "bashy",
				Reason: "missing source root in manifest",
			})
		}
		if (len(pubs) > 0 || hasGoModRecs) && !hasGoMod {
			missing = append(missing, MissingArtifact{
				Kind: "gomod", Name: "modules",
				Reason: "modules present in catalog without gomod roots in manifest",
			})
		}
		for _, lang := range profile.Languages {
			lang = strings.TrimSpace(lang)
			if lang != "" && !langFound[lang] {
				missing = append(missing, MissingArtifact{
					Kind: "lang-pack", Name: lang,
					Reason: fmt.Sprintf("mandatory language pack %q missing from manifest", lang),
				})
			}
		}
		for _, m := range profile.Models {
			if !m.Required {
				continue
			}
			var matchedRoot *RootDescriptor
			for i := range targetManifest.Roots {
				if modelMatchesRoot(m.Name, targetManifest.Roots[i]) {
					matchedRoot = &targetManifest.Roots[i]
					break
				}
			}
			if matchedRoot == nil {
				missing = append(missing, MissingArtifact{
					Kind:    "ollama-model",
					Name:    m.Name,
					Version: m.Role,
					Reason:  fmt.Sprintf("required %s model %q missing from manifest", m.Role, m.Name),
				})
			} else if evalReason := checkRequiredModelEval(ctx, backend, m, matchedRoot.Digest); evalReason != "" {
				missing = append(missing, MissingArtifact{
					Kind:    "ollama-eval",
					Name:    m.Name,
					Version: m.Role,
					Digest:  matchedRoot.Digest,
					Reason:  evalReason,
				})
			}
		}

		// Verify every manifest root and transitive dependency blob.
		for _, r := range targetManifest.Roots {
			checkDigest(r.Digest, r.Size, r.Kind, r.Name, r.Version)
			for _, dep := range r.Deps {
				checkDigest(dep, -1, r.Kind+"-dep", r.Name, r.Version)
			}
		}

		// Verify every PublicationRecord's .info/.mod and either .zip or intact Loom ref.
		for _, pub := range pubs {
			checkDigest(pub.InfoDigest, -1, "gomod-info", pub.Module, pub.Version)
			checkDigest(pub.ModDigest, -1, "gomod-mod", pub.Module, pub.Version)
			if backend != nil {
				zipErr := verifyBlobInBackend(ctx, backend, pub.ZipDigest, -1, opts.VerifyDigests)
				if zipErr == nil {
					if !verifiedSet[pub.ZipDigest] {
						verifiedSet[pub.ZipDigest] = true
						verifiedBlobs++
					}
				} else if errors.Is(zipErr, ErrPayloadEvicted) || errors.Is(zipErr, ErrNotFound) {
					if loomErr := verifyLoomPublicationCommit(loomDir, pub); loomErr != nil {
						missing = append(missing, MissingArtifact{
							Kind:    "gomod-publication",
							Name:    pub.Module,
							Version: pub.Version,
							Digest:  pub.ZipDigest,
							Reason:  fmt.Sprintf("evicted zip cannot be reconstructed from loom: %v", loomErr),
						})
					}
				} else {
					missing = append(missing, MissingArtifact{
						Kind:    "gomod-zip",
						Name:    pub.Module,
						Version: pub.Version,
						Digest:  pub.ZipDigest,
						Reason:  zipErr.Error(),
					})
				}
			}
		}
	}

	if missing == nil {
		missing = []MissingArtifact{}
	}

	var manifestDigest string
	if targetManifest != nil {
		manifestDigest, _ = targetManifest.Digest()
	}

	offlineEnv := strings.TrimSpace(os.Getenv("BASHY_OFFLINE")) == "1"
	sealed := offlineEnv || (hasSealSt && sealSt.Sealed)

	var activeGen, baselineGen uint64
	if hasSealSt {
		activeGen = sealSt.ActiveGeneration
		baselineGen = sealSt.BaselineGeneration
	}
	if activeGen == 0 && targetManifest != nil {
		activeGen = targetManifest.Generation
	}

	status := &SystemStatus{
		SchemaVersion:       OfflineStatusSchemaV1,
		ArtifactRoot:        root,
		LoomDir:             loomDir,
		Platform:            profile.Platform,
		Sealed:              sealed,
		OfflineEnv:          offlineEnv,
		SealState:           sealPtr,
		Profile:             profile,
		ProfileDigest:       profDigest,
		ActiveGeneration:    activeGen,
		BaselineGeneration:  baselineGen,
		LatestGeneration:    storeStatus.LatestGeneration,
		Manifest:            targetManifest,
		ManifestDigest:      manifestDigest,
		PublicationCount:    len(pubs),
		RecordCount:         storeStatus.RecordCount,
		EvictedCount:        storeStatus.EvictedCount,
		FreeBytes:           freeBytes,
		RebuildReserveBytes: reserveBytes,
		CapacityOK:          capOK,
		CapacityError:       capErrStr,
		ClosureComplete:     targetManifest != nil && len(missing) == 0,
		VerifiedDigests:     opts.VerifyDigests,
		MissingArtifacts:    missing,
	}
	return status, verifiedBlobs, nil
}

// InspectStatus evaluates the offline closure, seal state, and disk reserve without mutating state.
func InspectStatus(ctx context.Context, opts InspectStatusOptions) (*SystemStatus, error) {
	st, _, err := inspectClosureInternal(ctx, opts, false)
	return st, err
}

// VerifyClosure verifies that the active/latest manifest satisfies ProfileV1 and
// (when VerifyDigests is true) cryptographically verifies every referenced blob.
func VerifyClosure(ctx context.Context, opts VerifyOptions) (*VerifyReport, error) {
	st, verifiedBlobs, err := inspectClosureInternal(ctx, InspectStatusOptions(opts), false)
	if err != nil {
		return nil, err
	}
	return &VerifyReport{
		Status:          *st,
		Complete:        st.ClosureComplete && st.CapacityOK,
		VerifiedBlobs:   verifiedBlobs,
		VerifiedDigests: opts.VerifyDigests,
		Missing:         st.MissingArtifacts,
	}, nil
}

// Seal cryptographically verifies the closure and capacity reserve against ProfileV1
// and atomically persists <root>/seal.json, preserving BaselineGeneration across upgrades.
func Seal(ctx context.Context, opts SealOptions) (*SealStateV1, error) {
	st, _, err := inspectClosureInternal(ctx, InspectStatusOptions{
		Root:          opts.Root,
		LoomDir:       opts.LoomDir,
		Backend:       opts.Backend,
		Profile:       opts.Profile,
		ProfileSpec:   opts.ProfileSpec,
		Generation:    opts.Generation,
		VerifyDigests: true,
		DiskFreeFunc:  opts.DiskFreeFunc,
	}, true)
	if err != nil {
		return nil, err
	}
	if st.Manifest == nil || len(st.MissingArtifacts) > 0 {
		parts := make([]string, 0, len(st.MissingArtifacts))
		for _, m := range st.MissingArtifacts {
			label := m.Kind + ":" + m.Name
			if m.Version != "" {
				label += "@" + m.Version
			}
			parts = append(parts, fmt.Sprintf("%s (%s)", label, m.Reason))
		}
		if len(parts) == 0 {
			parts = append(parts, "no manifest")
		}
		return nil, fmt.Errorf("%w: %d missing artifact(s): %s", ErrIncompleteClosure, len(parts), strings.Join(parts, "; "))
	}
	if !st.CapacityOK {
		_, capErr := CheckCapacityReserve(st.ArtifactRoot, 0, st.RebuildReserveBytes, opts.DiskFreeFunc)
		if capErr != nil {
			return nil, capErr
		}
		return nil, ErrInsufficientCapacity
	}

	now := time.Now().UTC()
	if opts.Now != nil {
		now = opts.Now().UTC()
	}

	baselineGen := st.Manifest.Generation
	if existing, ok, err := LoadSealState(st.ArtifactRoot); err == nil && ok && existing.BaselineGeneration > 0 {
		baselineGen = existing.BaselineGeneration
	}

	newState := SealStateV1{
		SchemaVersion:      SealStateSchemaV1,
		Sealed:             true,
		SealedAt:           now.Format(time.RFC3339),
		ActiveGeneration:   st.Manifest.Generation,
		BaselineGeneration: baselineGen,
		ProfileName:        st.Profile.Name,
		ProfileDigest:      st.ProfileDigest,
		ManifestDigest:     st.ManifestDigest,
		Platform:           st.Manifest.Platform,
		GoToolchainVersion: st.Manifest.GoToolchainVersion,
		ProductOverlaySHA:  st.Manifest.ProductOverlaySHA,
	}
	if err := SaveProfile(st.ArtifactRoot, st.Profile); err != nil {
		return nil, err
	}
	if err := SaveSealState(st.ArtifactRoot, newState); err != nil {
		return nil, err
	}
	return &newState, nil
}
