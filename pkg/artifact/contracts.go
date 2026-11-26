// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

// Package artifact defines the versioned contracts, retention/lease rules,
// publication identity, tier-floor placement, and reuse boundaries for
// self-contained offline bashy (Sprint 654).
package artifact

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

const (
	ProfileSchemaV1       = "bashy-offline-profile-v1"
	ManifestSchemaV1      = "bashy-artifact-manifest-v1"
	CatalogRecordSchemaV1 = "bashy-artifact-record-v1"
	PublicationSchemaV1   = "bashy-module-publication-v1"
	AlertJournalSchemaV1  = "bashy-offline-alert-v1"
)

var (
	ErrOfflineFloorUnavailable      = errors.New("artifact: no eligible tier available at or above configured offline floor")
	ErrConfinementViolation         = errors.New("artifact: fallback tier would violate workload confinement")
	ErrSealedNetworkDenied          = errors.New("artifact: public network acquisition denied in sealed mode")
	ErrImmutablePublicationConflict = errors.New("artifact: module version already published with different content")
	ErrMissingRecoveryInputs        = errors.New("artifact: cannot reconstruct evicted payload from retained inputs")
	ErrProtectedArtifact            = errors.New("artifact: protected artifact or active lease forbids eviction")
	ErrNotFound                     = errors.New("artifact: not found")
	ErrPayloadEvicted               = errors.New("artifact: payload evicted from CAS; regenerate from recipe")
	ErrCorruptPayload               = errors.New("artifact: payload missing or sha256 mismatch")
	ErrStoreBusy                    = errors.New("artifact: catalog database locked by another writer")
	ErrNonLoopbackBind              = errors.New("artifact: non-loopback bind requires explicit mesh configuration")
	ErrSharingViolation             = errors.New("artifact: blob has open reader or file sharing lock")
)

var sha256DigestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// ValidDigest reports whether d is a canonical lowercase sha256:<64hex> digest.
func ValidDigest(d string) bool {
	return sha256DigestRE.MatchString(d)
}

// DigestBytes returns sha256:<64hex> of data.
func DigestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// SafeLogicalPath rejects empty paths, backslashes, NULs, absolute paths, and
// .. traversal segments in portable bundle or logical artifact names.
func SafeLogicalPath(p string) bool {
	if p == "" || strings.ContainsAny(p, "\\\x00") || strings.HasPrefix(p, "/") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return path.Clean(p) == p
}

// Tier names one of the six canonical dhnt execution tiers.
type Tier string

const (
	TierCloud     Tier = "cloud"
	TierCluster   Tier = "cluster"
	TierSphere    Tier = "sphere"
	TierSandbox   Tier = "sandbox"
	TierWorkplace Tier = "workplace"
	TierUserland  Tier = "userland"
)

// DefaultTierOrder is the initial preference order (highest to lowest).
func DefaultTierOrder() []Tier {
	return []Tier{TierCloud, TierCluster, TierSphere, TierSandbox, TierWorkplace, TierUserland}
}

// Valid reports whether t is in the closed six-tier vocabulary.
func (t Tier) Valid() bool {
	switch t {
	case TierCloud, TierCluster, TierSphere, TierSandbox, TierWorkplace, TierUserland:
		return true
	default:
		return false
	}
}

// RetentionClass governs whether the artifact store may reclaim a payload blob.
type RetentionClass string

const (
	// RetentionProtectedRoot marks bootstrap roots, transitive build/runtime
	// inputs, published Loom source, pinned model weights without a policy
	// replica, and non-reproducible images. Never auto-deleted.
	RetentionProtectedRoot RetentionClass = "protected"
	// RetentionReconstructible marks derived outputs whose exact digest can be
	// deterministically regenerated from retained inputs via a verified recipe.
	RetentionReconstructible RetentionClass = "reconstructible"
	// RetentionScratch marks disposable compilation/runtime scratch projections.
	RetentionScratch RetentionClass = "scratch"
)

func (r RetentionClass) Valid() bool {
	switch r {
	case RetentionProtectedRoot, RetentionReconstructible, RetentionScratch:
		return true
	default:
		return false
	}
}

// RecipeKind identifies a deterministic local regeneration mechanism.
// Non-deterministic generation (such as re-running an LLM) is never a recipe.
type RecipeKind string

const (
	RecipeGoModZipV1        RecipeKind = "gomod-zip-v1"
	RecipeOCIReproducibleV1 RecipeKind = "oci-reproducible-v1"
	RecipeDerivedCacheV1    RecipeKind = "derived-cache-v1"
)

// RegenerationRecipe binds an evictable derived artifact to the retained
// inputs and deterministic builder that reproduce its exact digest.
type RegenerationRecipe struct {
	Kind           RecipeKind        `json:"kind"`
	InputDigests   []string          `json:"input_digests,omitempty"`
	LoomRef        string            `json:"loom_ref,omitempty"`
	Commit         string            `json:"commit,omitempty"`
	Subdir         string            `json:"subdir,omitempty"`
	ExpectedDigest string            `json:"expected_digest"`
	ExpectedGoSum  string            `json:"expected_gosum,omitempty"`
	Params         map[string]string `json:"params,omitempty"`
}

func (r *RegenerationRecipe) Validate() error {
	if r == nil {
		return errors.New("artifact: nil regeneration recipe")
	}
	switch r.Kind {
	case RecipeGoModZipV1, RecipeOCIReproducibleV1, RecipeDerivedCacheV1:
	default:
		return fmt.Errorf("artifact: unsupported or non-deterministic recipe kind %q", r.Kind)
	}
	if !ValidDigest(r.ExpectedDigest) {
		return fmt.Errorf("artifact: recipe expected_digest %q must be sha256:<64hex>", r.ExpectedDigest)
	}
	for _, d := range r.InputDigests {
		if !ValidDigest(d) {
			return fmt.Errorf("artifact: recipe input digest %q must be sha256:<64hex>", d)
		}
	}
	if r.Kind == RecipeGoModZipV1 {
		if strings.TrimSpace(r.LoomRef) == "" || strings.TrimSpace(r.Commit) == "" || strings.TrimSpace(r.ExpectedGoSum) == "" {
			return errors.New("artifact: gomod-zip-v1 recipe requires loom_ref, commit, and expected_gosum")
		}
	}
	if r.Subdir != "" && !SafeLogicalPath(r.Subdir) {
		return fmt.Errorf("artifact: unsafe recipe subdir %q", r.Subdir)
	}
	return nil
}

// ModelProfileSpec declares a pinned local inference or embedding profile.
type ModelProfileSpec struct {
	Name             string   `json:"name"`
	Role             string   `json:"role"` // "coding", "general", "embedding"
	ContextLength    int      `json:"context_length"`
	MaxConcurrency   int      `json:"max_concurrency"`
	MaxLoadedModels  int      `json:"max_loaded_models"`
	RequiredEvals    []string `json:"required_evals,omitempty"`
	Required         bool     `json:"required"`
	ReplicaGuarantee bool     `json:"replica_guarantee,omitempty"`
}

// CapacityPolicy configures disk reserves required before sealing or admitting growth.
type CapacityPolicy struct {
	RebuildReserveBytes uint64 `json:"rebuild_reserve_bytes"`
}

// ResourcePolicy configures warning/critical thresholds and recovery hysteresis.
// Activated=false preserves advisory-only observation until explicitly enabled.
type ResourcePolicy struct {
	Activated             bool    `json:"activated"`
	DiskWarnUsedPct       float64 `json:"disk_warn_used_pct"`
	DiskCritUsedPct       float64 `json:"disk_crit_used_pct"`
	DiskFreeWarnBytes     uint64  `json:"disk_free_warn_bytes"`
	DiskFreeCritBytes     uint64  `json:"disk_free_crit_bytes"`
	DiskRecoverUsedPct    float64 `json:"disk_recover_used_pct"`
	DiskRecoverFreeBytes  uint64  `json:"disk_recover_free_bytes"`
	MemWarnPct            float64 `json:"mem_warn_pct"`
	MemCritPct            float64 `json:"mem_crit_pct"`
	CPUWarnPct            float64 `json:"cpu_warn_pct"`
	CPUCritPct            float64 `json:"cpu_crit_pct"`
	CPUSustainSeconds     int     `json:"cpu_sustain_seconds"`
	RecoverHealthySamples int     `json:"recover_healthy_samples"`
	RecoverMinSeconds     int     `json:"recover_min_seconds"`
}

// DefaultResourcePolicy matches the canonical S480 / Sprint 654 thresholds.
func DefaultResourcePolicy() ResourcePolicy {
	return ResourcePolicy{
		Activated:             false,
		DiskWarnUsedPct:       85,
		DiskCritUsedPct:       95,
		DiskFreeWarnBytes:     20 << 30,
		DiskFreeCritBytes:     10 << 30,
		DiskRecoverUsedPct:    80,
		DiskRecoverFreeBytes:  25 << 30,
		MemWarnPct:            85,
		MemCritPct:            95,
		CPUWarnPct:            85,
		CPUCritPct:            95,
		CPUSustainSeconds:     300,
		RecoverHealthySamples: 3,
		RecoverMinSeconds:     60,
	}
}

func (p ResourcePolicy) Validate() error {
	pctPair := func(name string, warn, crit float64) error {
		if warn <= 0 || warn >= 100 || crit <= 0 || crit > 100 || warn >= crit {
			return fmt.Errorf("artifact: invalid %s thresholds warn=%v crit=%v", name, warn, crit)
		}
		return nil
	}
	if err := pctPair("disk_used", p.DiskWarnUsedPct, p.DiskCritUsedPct); err != nil {
		return err
	}
	if err := pctPair("mem", p.MemWarnPct, p.MemCritPct); err != nil {
		return err
	}
	if err := pctPair("cpu", p.CPUWarnPct, p.CPUCritPct); err != nil {
		return err
	}
	if p.DiskFreeCritBytes == 0 || p.DiskFreeWarnBytes < p.DiskFreeCritBytes {
		return errors.New("artifact: disk_free_warn_bytes must be >= disk_free_crit_bytes > 0")
	}
	if p.DiskRecoverUsedPct <= 0 || p.DiskRecoverUsedPct >= p.DiskWarnUsedPct {
		return errors.New("artifact: disk_recover_used_pct must be in (0, disk_warn_used_pct)")
	}
	if p.DiskRecoverFreeBytes <= p.DiskFreeWarnBytes {
		return errors.New("artifact: disk_recover_free_bytes must exceed disk_free_warn_bytes")
	}
	if p.CPUSustainSeconds <= 0 || p.RecoverHealthySamples < 1 || p.RecoverMinSeconds < 1 {
		return errors.New("artifact: sustain and recovery hysteresis bounds must be positive")
	}
	return nil
}

// ProfileV1 is the operator/product input resolved by `bashy self bootstrap`.
type ProfileV1 struct {
	SchemaVersion string             `json:"schema_version"`
	Name          string             `json:"name"`
	Platform      string             `json:"platform"` // goos/goarch
	TierOrder     []Tier             `json:"tier_order"`
	OfflineFloor  Tier               `json:"offline_floor"`
	Languages     []string           `json:"languages"`
	Models        []ModelProfileSpec `json:"models"`
	Capacity      CapacityPolicy     `json:"capacity"`
	Resources     ResourcePolicy     `json:"resources"`
}

// DefaultProfileV1 returns the standard offline profile for platform (goos/goarch).
func DefaultProfileV1(platform string) ProfileV1 {
	return ProfileV1{
		SchemaVersion: ProfileSchemaV1,
		Name:          "default",
		Platform:      platform,
		TierOrder:     DefaultTierOrder(),
		OfflineFloor:  TierUserland,
		Languages:     []string{"go", "bashsharp"},
		Models: []ModelProfileSpec{
			{
				Name:            "qwen3.5:4b",
				Role:            "coding",
				ContextLength:   4096,
				MaxConcurrency:  1,
				MaxLoadedModels: 1,
				RequiredEvals:   []string{"tool-call", "code-edit", "repair"},
				Required:        true,
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

func validPlatform(p string) bool {
	osName, arch, ok := strings.Cut(strings.TrimSpace(p), "/")
	return ok && osName != "" && arch != "" && !strings.Contains(arch, "/")
}

func (p ProfileV1) Validate() error {
	if p.SchemaVersion != ProfileSchemaV1 {
		return fmt.Errorf("artifact: unsupported profile schema %q (want %s)", p.SchemaVersion, ProfileSchemaV1)
	}
	if strings.TrimSpace(p.Name) == "" || !SafeLogicalPath(p.Name) {
		return fmt.Errorf("artifact: invalid profile name %q", p.Name)
	}
	if !validPlatform(p.Platform) {
		return fmt.Errorf("artifact: invalid platform %q (want goos/goarch)", p.Platform)
	}
	if len(p.TierOrder) == 0 {
		return errors.New("artifact: tier_order must not be empty")
	}
	seenTiers := map[Tier]bool{}
	for _, t := range p.TierOrder {
		if !t.Valid() {
			return fmt.Errorf("artifact: unknown tier %q", t)
		}
		if seenTiers[t] {
			return fmt.Errorf("artifact: duplicate tier %q in tier_order", t)
		}
		seenTiers[t] = true
	}
	if !p.OfflineFloor.Valid() {
		return fmt.Errorf("artifact: invalid offline_floor %q", p.OfflineFloor)
	}
	if p.OfflineFloor == TierCloud {
		return errors.New("artifact: offline_floor cannot be external cloud")
	}
	if !seenTiers[p.OfflineFloor] {
		return fmt.Errorf("artifact: offline_floor %q must appear in tier_order", p.OfflineFloor)
	}
	hasGo, hasBashSharp := false, false
	for _, lang := range p.Languages {
		switch strings.TrimSpace(lang) {
		case "go":
			hasGo = true
		case "bashsharp":
			hasBashSharp = true
		case "":
			return errors.New("artifact: empty language pack name")
		}
	}
	if !hasGo || !hasBashSharp {
		return errors.New("artifact: profile languages must include mandatory go and bashsharp packs")
	}
	hasCoding, hasEmbedding := false, false
	for _, m := range p.Models {
		if strings.TrimSpace(m.Name) == "" || m.ContextLength <= 0 || m.MaxConcurrency < 1 || m.MaxLoadedModels < 1 {
			return fmt.Errorf("artifact: invalid model profile %q", m.Name)
		}
		if m.Required && (m.Role == "coding" || m.Role == "general") {
			hasCoding = true
		}
		if m.Required && m.Role == "embedding" {
			hasEmbedding = true
		}
	}
	if !hasCoding || !hasEmbedding {
		return errors.New("artifact: profile requires at least one coding/general model and one embedding model")
	}
	if p.Capacity.RebuildReserveBytes == 0 {
		return errors.New("artifact: capacity.rebuild_reserve_bytes must be > 0")
	}
	return p.Resources.Validate()
}

// CanonicalBytes serializes ProfileV1 deterministically after validation.
func (p ProfileV1) CanonicalBytes() ([]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(p)
}

func (p ProfileV1) Digest() (string, error) {
	b, err := p.CanonicalBytes()
	if err != nil {
		return "", err
	}
	return DigestBytes(b), nil
}

// RootDescriptor names one protected artifact closure root in ManifestV1.
type RootDescriptor struct {
	Kind      string   `json:"kind"` // source, gotoolchain, gooverlay, gomod, binmgr, oci-image, podman-vm, ollama-model, lang-pack
	Name      string   `json:"name"`
	Version   string   `json:"version"`
	Platform  string   `json:"platform,omitempty"`
	MediaType string   `json:"media_type"`
	Digest    string   `json:"digest"`
	Size      int64    `json:"size"`
	Deps      []string `json:"deps,omitempty"`
}

func (r RootDescriptor) Validate() error {
	if strings.TrimSpace(r.Kind) == "" || !SafeLogicalPath(r.Name) || strings.TrimSpace(r.Version) == "" {
		return fmt.Errorf("artifact: root descriptor requires kind, safe name (%q), and version", r.Name)
	}
	if strings.TrimSpace(r.MediaType) == "" || !ValidDigest(r.Digest) || r.Size < 0 {
		return fmt.Errorf("artifact: invalid root descriptor digest/size for %s@%s", r.Name, r.Version)
	}
	if r.Platform != "" && !validPlatform(r.Platform) {
		return fmt.Errorf("artifact: invalid root platform %q", r.Platform)
	}
	for _, d := range r.Deps {
		if !ValidDigest(d) {
			return fmt.Errorf("artifact: invalid dependency digest %q on %s", d, r.Name)
		}
	}
	return nil
}

// ManifestV1 is the platform-resolved protected closure produced by bootstrap
// and verified before activation or portable import.
type ManifestV1 struct {
	SchemaVersion       string           `json:"schema_version"`
	Generation          uint64           `json:"generation"`
	CreatedAt           string           `json:"created_at"` // RFC3339 UTC
	Platform            string           `json:"platform"`
	ProfileName         string           `json:"profile_name"`
	ProfileDigest       string           `json:"profile_digest"`
	GoToolchainVersion  string           `json:"go_toolchain_version"`
	ProductOverlaySHA   string           `json:"product_overlay_sha"`
	BuildImageDigest    string           `json:"build_image_digest,omitempty"`
	RebuildReserveBytes uint64           `json:"rebuild_reserve_bytes"`
	Roots               []RootDescriptor `json:"roots"`
}

func (m ManifestV1) Validate() error {
	if m.SchemaVersion != ManifestSchemaV1 {
		return fmt.Errorf("artifact: unsupported manifest schema %q (want %s)", m.SchemaVersion, ManifestSchemaV1)
	}
	if m.Generation == 0 {
		return errors.New("artifact: manifest generation must be >= 1")
	}
	if _, err := time.Parse(time.RFC3339, m.CreatedAt); err != nil {
		return fmt.Errorf("artifact: created_at %q must be RFC3339: %w", m.CreatedAt, err)
	}
	if !validPlatform(m.Platform) {
		return fmt.Errorf("artifact: invalid manifest platform %q", m.Platform)
	}
	if !SafeLogicalPath(m.ProfileName) || !ValidDigest(m.ProfileDigest) {
		return errors.New("artifact: manifest requires safe profile_name and valid profile_digest")
	}
	if strings.TrimSpace(m.GoToolchainVersion) == "" || !ValidDigest(m.ProductOverlaySHA) {
		return errors.New("artifact: manifest requires go_toolchain_version and product_overlay_sha")
	}
	if m.BuildImageDigest != "" && !ValidDigest(m.BuildImageDigest) {
		return fmt.Errorf("artifact: invalid build_image_digest %q", m.BuildImageDigest)
	}
	if m.RebuildReserveBytes == 0 {
		return errors.New("artifact: manifest rebuild_reserve_bytes must be > 0")
	}
	if len(m.Roots) == 0 {
		return errors.New("artifact: manifest roots must not be empty")
	}
	for _, r := range m.Roots {
		if err := r.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func (m ManifestV1) CanonicalBytes() ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	cp := m
	cp.Roots = slices.Clone(m.Roots)
	slices.SortFunc(cp.Roots, func(a, b RootDescriptor) int {
		if c := strings.Compare(a.Kind, b.Kind); c != 0 {
			return c
		}
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		if c := strings.Compare(a.Version, b.Version); c != 0 {
			return c
		}
		return strings.Compare(a.Digest, b.Digest)
	})
	return json.Marshal(cp)
}

func (m ManifestV1) Digest() (string, error) {
	b, err := m.CanonicalBytes()
	if err != nil {
		return "", err
	}
	return DigestBytes(b), nil
}

// CatalogRecordV1 is the bbolt metadata record for one CAS payload or logical artifact.
type CatalogRecordV1 struct {
	SchemaVersion  string              `json:"schema_version"`
	Digest         string              `json:"digest"`
	Size           int64               `json:"size"`
	MediaType      string              `json:"media_type"`
	Platform       string              `json:"platform,omitempty"`
	LogicalName    string              `json:"logical_name"`
	LogicalVersion string              `json:"logical_version"`
	Dependencies   []string            `json:"dependencies,omitempty"`
	Provenance     string              `json:"provenance"`
	Retention      RetentionClass      `json:"retention"`
	RetentionRoots []string            `json:"retention_roots,omitempty"`
	Recipe         *RegenerationRecipe `json:"recipe,omitempty"`
	CreatedAt      time.Time           `json:"created_at"`
	LastAccessedAt time.Time           `json:"last_accessed_at"`
}

func (r CatalogRecordV1) Validate() error {
	if r.SchemaVersion != CatalogRecordSchemaV1 {
		return fmt.Errorf("artifact: unsupported record schema %q", r.SchemaVersion)
	}
	if !ValidDigest(r.Digest) || r.Size < 0 || strings.TrimSpace(r.MediaType) == "" {
		return errors.New("artifact: record requires valid digest, non-negative size, and media_type")
	}
	if !SafeLogicalPath(r.LogicalName) || strings.TrimSpace(r.LogicalVersion) == "" {
		return fmt.Errorf("artifact: invalid logical identity %q@%q", r.LogicalName, r.LogicalVersion)
	}
	if !r.Retention.Valid() {
		return fmt.Errorf("artifact: invalid retention %q", r.Retention)
	}
	for _, dep := range r.Dependencies {
		if !ValidDigest(dep) {
			return fmt.Errorf("artifact: invalid dependency digest %q", dep)
		}
	}
	if r.Retention == RetentionReconstructible {
		if err := r.Recipe.Validate(); err != nil {
			return fmt.Errorf("artifact: reconstructible record requires valid recipe: %w", err)
		}
		if r.Recipe.ExpectedDigest != r.Digest {
			return fmt.Errorf("artifact: recipe expected_digest %s does not match record digest %s", r.Recipe.ExpectedDigest, r.Digest)
		}
	}
	return nil
}

// Lease binds active read/build operations to the owning service epoch and process identity.
type Lease struct {
	ID            string    `json:"id"`
	HolderPID     int       `json:"holder_pid"`
	HolderStartID string    `json:"holder_start_id"`
	ServiceEpoch  uint64    `json:"service_epoch"`
	Digests       []string  `json:"digests"`
	ExpiresAt     time.Time `json:"expires_at"`
}

// ActiveAt reports whether l still holds authority at now under currentEpoch.
// A lease from a prior service epoch or a recycled PID loses authority immediately.
func (l Lease) ActiveAt(now time.Time, currentEpoch uint64, alive func(pid int, startID string) bool) bool {
	if l.ID == "" || l.ServiceEpoch == 0 || l.ServiceEpoch != currentEpoch {
		return false
	}
	if !l.ExpiresAt.IsZero() && !now.Before(l.ExpiresAt) {
		return false
	}
	if l.HolderPID > 0 && alive != nil && !alive(l.HolderPID, l.HolderStartID) {
		return false
	}
	return true
}

// CanEvict enforces the protected-root, dependency-closure, recipe-completeness,
// and active-lease GC rules before any CAS blob deletion.
func CanEvict(rec CatalogRecordV1, protectedClosure map[string]bool, retainedDigests map[string]bool, leases []Lease, currentEpoch uint64, now time.Time, alive func(int, string) bool) (bool, string) {
	if rec.Retention == RetentionProtectedRoot || len(rec.RetentionRoots) > 0 || protectedClosure[rec.Digest] {
		return false, "protected root or transitive build/runtime dependency"
	}
	for _, l := range leases {
		if !l.ActiveAt(now, currentEpoch, alive) {
			continue
		}
		if slices.Contains(l.Digests, rec.Digest) {
			return false, "held by active lease " + l.ID
		}
	}
	if rec.Retention == RetentionReconstructible {
		if err := rec.Recipe.Validate(); err != nil {
			return false, "invalid regeneration recipe: " + err.Error()
		}
		if rec.Recipe.ExpectedDigest != rec.Digest {
			return false, "recipe expected digest mismatch"
		}
		for _, in := range rec.Recipe.InputDigests {
			if !retainedDigests[in] && !protectedClosure[in] {
				return false, "missing retained recipe input " + in
			}
		}
		return true, "reconstructible via " + string(rec.Recipe.Kind)
	}
	if rec.Retention == RetentionScratch {
		return true, "inactive disposable scratch"
	}
	return false, "unknown retention class"
}

// PublicationRecord binds an immutable Go module version to its durable Loom
// source commit/ref and canonical packaging hashes.
type PublicationRecord struct {
	SchemaVersion string    `json:"schema_version"`
	Module        string    `json:"module"`
	Version       string    `json:"version"`
	Commit        string    `json:"commit"`
	Subdir        string    `json:"subdir,omitempty"`
	LoomRef       string    `json:"loom_ref"`
	InfoDigest    string    `json:"info_digest"`
	ModDigest     string    `json:"mod_digest"`
	ZipDigest     string    `json:"zip_digest"`
	GoSumH1       string    `json:"gosum_h1"`
	PublishedAt   time.Time `json:"published_at"`
}

// LoomPublicationRef returns the canonical immutable Git ref under which Loom
// retains the exact source commit of a published internal module version.
func LoomPublicationRef(modPath, version string) (string, error) {
	if err := module.Check(modPath, version); err != nil {
		return "", err
	}
	escPath, err := module.EscapePath(modPath)
	if err != nil {
		return "", err
	}
	escVer, err := module.EscapeVersion(version)
	if err != nil {
		return "", err
	}
	return "refs/bashy/published/" + escPath + "/" + escVer, nil
}

func (p PublicationRecord) Validate() error {
	if p.SchemaVersion != PublicationSchemaV1 {
		return fmt.Errorf("artifact: unsupported publication schema %q", p.SchemaVersion)
	}
	if !semver.IsValid(p.Version) || semver.Canonical(p.Version) != p.Version {
		return fmt.Errorf("artifact: non-canonical version %q", p.Version)
	}
	if err := module.Check(p.Module, p.Version); err != nil {
		return fmt.Errorf("artifact: invalid module version: %w", err)
	}
	if len(p.Commit) != 40 || !strings.HasPrefix(p.LoomRef, "refs/bashy/published/") {
		return errors.New("artifact: publication requires 40-hex commit and refs/bashy/published/* loom_ref")
	}
	if p.Subdir != "" && !SafeLogicalPath(p.Subdir) {
		return fmt.Errorf("artifact: unsafe publication subdir %q", p.Subdir)
	}
	if !ValidDigest(p.InfoDigest) || !ValidDigest(p.ModDigest) || !ValidDigest(p.ZipDigest) {
		return errors.New("artifact: publication requires sha256 digests for .info, .mod, and .zip")
	}
	if !strings.HasPrefix(p.GoSumH1, "h1:") || len(p.GoSumH1) < 10 {
		return fmt.Errorf("artifact: invalid go.sum h1 hash %q", p.GoSumH1)
	}
	return nil
}

// CheckImmutablePublication returns (true, nil) when existing and candidate are
// byte/hash-identical (idempotent re-publish), and ErrImmutablePublicationConflict
// if any content hash or source commit diverges.
func CheckImmutablePublication(existing, candidate PublicationRecord) (idempotent bool, err error) {
	if existing.Module != candidate.Module || existing.Version != candidate.Version {
		return false, nil
	}
	if existing.Commit == candidate.Commit &&
		existing.Subdir == candidate.Subdir &&
		existing.ModDigest == candidate.ModDigest &&
		existing.ZipDigest == candidate.ZipDigest &&
		existing.GoSumH1 == candidate.GoSumH1 {
		return true, nil
	}
	return false, fmt.Errorf("%w: %s@%s existing (%s, zip %s) != candidate (%s, zip %s)",
		ErrImmutablePublicationConflict, existing.Module, existing.Version,
		existing.GoSumH1, existing.ZipDigest, candidate.GoSumH1, candidate.ZipDigest)
}

// WorkloadRequirements declares confinement and platform needs that tier
// placement must never silently weaken on fallback.
type WorkloadRequirements struct {
	TargetOS       string `json:"target_os,omitempty"`
	TargetArch     string `json:"target_arch,omitempty"`
	RequireSandbox bool   `json:"require_sandbox,omitempty"`
	RequireGPU     bool   `json:"require_gpu,omitempty"`
	MemoryBytes    uint64 `json:"memory_bytes,omitempty"`
}

// TierStatus reports live capability and availability of one execution tier.
type TierStatus struct {
	Available            bool   `json:"available"`
	OS                   string `json:"os,omitempty"`
	Arch                 string `json:"arch,omitempty"`
	ProvidesSandbox      bool   `json:"provides_sandbox,omitempty"`
	ProvidesGPU          bool   `json:"provides_gpu,omitempty"`
	AvailableMemoryBytes uint64 `json:"available_memory_bytes,omitempty"`
	UnavailableReason    string `json:"unavailable_reason,omitempty"`
}

// PlacementError explains why every tier down to OfflineFloor was rejected.
type PlacementError struct {
	Floor      Tier
	Cause      error
	Rejections []string
}

func (e *PlacementError) Error() string {
	return fmt.Sprintf("%v (floor=%s; %s)", e.Cause, e.Floor, strings.Join(e.Rejections, "; "))
}

func (e *PlacementError) Unwrap() error { return e.Cause }

// SelectTier chooses the highest-preference tier in profile.TierOrder (stopping
// at profile.OfflineFloor) that satisfies sealed-mode rules and workload
// confinement/OS/hardware requirements.
func SelectTier(profile ProfileV1, sealed bool, req WorkloadRequirements, status map[Tier]TierStatus) (Tier, error) {
	if err := profile.Validate(); err != nil {
		return "", err
	}
	var rejections []string
	sawConfinementReject := false
	for _, tier := range profile.TierOrder {
		if sealed && tier == TierCloud {
			rejections = append(rejections, "cloud: excluded in sealed mode")
			if tier == profile.OfflineFloor {
				break
			}
			continue
		}
		st, ok := status[tier]
		switch {
		case !ok || !st.Available:
			why := st.UnavailableReason
			if why == "" {
				why = "unavailable"
			}
			rejections = append(rejections, string(tier)+": "+why)
		case req.RequireSandbox && !st.ProvidesSandbox:
			sawConfinementReject = true
			rejections = append(rejections, string(tier)+": lacks required sandbox confinement")
		case req.TargetOS != "" && st.OS != "" && st.OS != req.TargetOS:
			rejections = append(rejections, fmt.Sprintf("%s: target_os=%s != %s", tier, req.TargetOS, st.OS))
		case req.TargetArch != "" && st.Arch != "" && st.Arch != req.TargetArch:
			rejections = append(rejections, fmt.Sprintf("%s: target_arch=%s != %s", tier, req.TargetArch, st.Arch))
		case req.RequireGPU && !st.ProvidesGPU:
			rejections = append(rejections, string(tier)+": lacks required GPU")
		case req.MemoryBytes > 0 && st.AvailableMemoryBytes < req.MemoryBytes:
			rejections = append(rejections, fmt.Sprintf("%s: memory %d < required %d", tier, st.AvailableMemoryBytes, req.MemoryBytes))
		default:
			return tier, nil
		}
		if tier == profile.OfflineFloor {
			break
		}
	}
	cause := ErrOfflineFloorUnavailable
	if sawConfinementReject {
		cause = ErrConfinementViolation
	}
	return "", &PlacementError{Floor: profile.OfflineFloor, Cause: cause, Rejections: rejections}
}

// UpstreamComponent documents an approved permissive dependency reused by Sprint 654.
type UpstreamComponent struct {
	Concern   string `json:"concern"`
	Module    string `json:"module"`
	License   string `json:"license"`
	Mechanism string `json:"mechanism"`
}

// ApprovedReuseComponents returns the locked permissive reuse boundary table.
func ApprovedReuseComponents() []UpstreamComponent {
	return []UpstreamComponent{
		{Concern: "cas-blobs", Module: "oras.land/oras-go/v2", License: "Apache-2.0", Mechanism: "import (v2.7.0)"},
		{Concern: "catalog-db", Module: "go.etcd.io/bbolt", License: "MIT", Mechanism: "import (existing v1.5.0)"},
		{Concern: "go-proxy", Module: "github.com/goproxy/goproxy", License: "MIT", Mechanism: "import (v0.27.0)"},
		{Concern: "go-proxy-singleflight", Module: "golang.org/x/sync/singleflight", License: "BSD-3-Clause", Mechanism: "import (existing v0.25.0)"},
		{Concern: "gomod-zip-sum", Module: "golang.org/x/mod", License: "BSD-3-Clause", Mechanism: "import (existing v0.42.0)"},
	}
}
