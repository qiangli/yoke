package fleet

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// ACP ADAPTERS — how a tool that does not speak ACP itself gets onto the ACP
// rung anyway, without bashy pretending otherwise.
//
// claude and codex speak their own protocols; the ACP bridges for them are
// separate npm packages (`@agentclientprotocol/claude-agent-acp`,
// `@agentclientprotocol/codex-acp`) that the Zed/JetBrains ACP registry
// distributes. gemini speaks ACP itself behind `--acp`. Either way the process
// launched on the ACP rung is named here, pinned to one release and one
// tarball digest, so the operator can see exactly what runs and where it came
// from. Nothing is vendored: the adapter is installed by the operator (or
// fetched with npx by an ACP client) and bashy checks that what it finds is
// the pinned release before driving it.
//
// The same declaration is the tool's ACP-registry entry: ACPRegistryManifest
// renders the registry's own agent.schema.json shape from it, so the manifest
// bashy publishes can never disagree with what bashy launches.

// ToolACPAdapter pins the ACP agent process for a tool's acp_exec.
type ToolACPAdapter struct {
	// ID is the ACP-registry agent id (lowercase, hyphens), e.g. claude-acp.
	ID string `yaml:"id" json:"id" doc:"ACP-registry agent id"`
	// Name and Description are the registry's display fields.
	Name        string `yaml:"name,omitempty" json:"name,omitempty" doc:"registry display name"`
	Description string `yaml:"description,omitempty" json:"description,omitempty" doc:"registry one-line description"`
	// Repository is the adapter's source repository.
	Repository string   `yaml:"repository,omitempty" json:"repository,omitempty" doc:"adapter source repository URL"`
	Authors    []string `yaml:"authors,omitempty" json:"authors,omitempty" doc:"adapter authors"`
	// Source is where the release is published. Only npm is distributed by
	// the registry for these three today.
	Source string `yaml:"source" json:"source" doc:"where the release is published: npm"`
	// Package is the npm package name, without a version.
	Package string `yaml:"package" json:"package" doc:"npm package name, unversioned"`
	// Version is the pinned release. Never a dist-tag.
	Version string `yaml:"version" json:"version" doc:"pinned adapter release (X.Y.Z)"`
	// License is the adapter's own licence (SPDX), and LicenseURL its text.
	License    string `yaml:"license" json:"license" doc:"adapter licence (SPDX id or terms)"`
	LicenseURL string `yaml:"license_url" json:"license_url" doc:"where the adapter licence is published"`
	// Bin is the executable the package installs and acp_exec runs. When it
	// equals the tool's own binary the tool speaks ACP natively (gemini --acp)
	// and the adapter is the tool's release itself.
	Bin string `yaml:"bin" json:"bin" doc:"executable the package installs (the ACP agent process)"`
	// Args are the arguments acp_exec passes after Bin; they are repeated in
	// the registry entry so an ACP client launches the same agent.
	Args []string `yaml:"args,omitempty" json:"args,omitempty" doc:"arguments after bin, as in acp_exec"`
	// Requires are host commands the adapter needs on PATH (node).
	Requires []string `yaml:"requires,omitempty" json:"requires,omitempty" doc:"host commands the adapter needs on PATH"`
	// Tarball is the published release artifact and its digests.
	Tarball ToolACPAdapterTarball `yaml:"tarball" json:"tarball" doc:"the pinned release tarball and its digests"`
}

// ToolACPAdapterTarball is the release artifact a pinned adapter must match.
type ToolACPAdapterTarball struct {
	URL string `yaml:"url" json:"url" doc:"release tarball URL"`
	// SHA256 was computed over the downloaded tarball.
	SHA256 string `yaml:"sha256" json:"sha256" doc:"hex sha256 of the tarball"`
	// Integrity is the registry's own SRI digest (sha512-…), which npm and
	// npx verify on install; the sha256 above was cross-checked against it.
	Integrity string `yaml:"integrity,omitempty" json:"integrity,omitempty" doc:"registry SRI digest (sha512-base64) npm verifies on install"`
}

var (
	acpRegistryID = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	semverPin     = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
)

// Validate checks an adapter block is a pin: a registry id, a known source, an
// exact release, a licence, the executable, and a tarball with a well-formed
// sha256 (plus a well-formed sha512 SRI when given). A nil adapter is valid.
func (a *ToolACPAdapter) Validate(tool string) error {
	if a == nil {
		return nil
	}
	var errs []error
	bad := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("fleet: tool %q acp_adapter: "+format, append([]any{tool}, args...)...))
	}
	if !acpRegistryID.MatchString(a.ID) {
		bad("id %q is not an ACP-registry id (lowercase, hyphens)", a.ID)
	}
	if a.Source != "npm" {
		bad("source %q is not supported (npm)", a.Source)
	}
	if strings.TrimSpace(a.Package) == "" || strings.Contains(strings.TrimPrefix(a.Package, "@"), "@") {
		bad("package %q must be an unversioned npm package name", a.Package)
	}
	if !semverPin.MatchString(a.Version) {
		bad("version %q is not an X.Y.Z pin", a.Version)
	}
	if strings.TrimSpace(a.License) == "" || strings.TrimSpace(a.LicenseURL) == "" {
		bad("license and license_url are required")
	}
	if strings.TrimSpace(a.Bin) == "" || strings.ContainsAny(a.Bin, " \t") {
		bad("bin %q must be one executable name", a.Bin)
	}
	if strings.TrimSpace(a.Tarball.URL) == "" {
		bad("tarball url is required")
	} else if !strings.Contains(a.Tarball.URL, a.Version) {
		bad("tarball url %q does not name version %s", a.Tarball.URL, a.Version)
	}
	if !hexSHA256.MatchString(a.Tarball.SHA256) {
		bad("tarball sha256 %q is not 64 lowercase hex", a.Tarball.SHA256)
	}
	if sri := a.Tarball.Integrity; sri != "" {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(sri, "sha512-"))
		if !strings.HasPrefix(sri, "sha512-") || err != nil || len(raw) != 64 {
			bad("tarball integrity %q is not a sha512 SRI digest", sri)
		}
	}
	return errors.Join(errs...)
}

// ValidateACPAdapter checks the tool's adapter pin and that acp_exec runs
// exactly what the pin names: `bin args...`. An acp_exec that drifted from its
// pin would launch one process while the registry manifest advertised another.
func (t Tool) ValidateACPAdapter() error {
	a := t.CLI.Launch.ACPAdapter
	if a == nil {
		return nil
	}
	if err := a.Validate(t.Name); err != nil {
		return err
	}
	want := strings.Join(append([]string{a.Bin}, a.Args...), " ")
	if got := strings.Join(strings.Fields(t.CLI.Launch.ACPExec), " "); got != want {
		return fmt.Errorf("fleet: tool %q acp_adapter: acp_exec %q does not run the pinned adapter %q", t.Name, got, want)
	}
	return nil
}

// PackageSpec is the versioned npm spec an installer or npx runs.
func (a ToolACPAdapter) PackageSpec() string { return a.Package + "@" + a.Version }

// ACPRegistryAgent is one agent entry in the ACP registry's agent.schema.json
// shape (agentclientprotocol/registry, registry v1). Field order and names
// follow the schema so the rendered JSON drops straight into the registry.
type ACPRegistryAgent struct {
	ID           string                  `json:"id"`
	Name         string                  `json:"name"`
	Version      string                  `json:"version"`
	Description  string                  `json:"description"`
	Repository   string                  `json:"repository,omitempty"`
	Authors      []string                `json:"authors,omitempty"`
	License      string                  `json:"license,omitempty"`
	LicenseURL   string                  `json:"license_url"`
	Distribution ACPRegistryDistribution `json:"distribution"`
}

// ACPRegistryDistribution carries the launch recipes; bashy pins npm only.
type ACPRegistryDistribution struct {
	NPX *ACPRegistryPackage `json:"npx,omitempty"`
}

// ACPRegistryPackage is the registry's packageDistribution.
type ACPRegistryPackage struct {
	Package string   `json:"package"`
	Args    []string `json:"args,omitempty"`
}

// ACPRegistryIndex is the aggregated registry.schema.json document.
type ACPRegistryIndex struct {
	Version string             `json:"version"`
	Agents  []ACPRegistryAgent `json:"agents"`
}

// ACPRegistryVersion is the registry schema version the manifest targets.
const ACPRegistryVersion = "1.0.0"

// ACPRegistryEntry renders the tool's adapter pin as a registry entry. It
// reports false for a tool with no ACP launch or no adapter pin.
func (t Tool) ACPRegistryEntry() (ACPRegistryAgent, bool) {
	a := t.CLI.Launch.ACPAdapter
	if a == nil || strings.TrimSpace(t.CLI.Launch.ACPExec) == "" {
		return ACPRegistryAgent{}, false
	}
	name := a.Name
	if name == "" {
		name = t.Display
	}
	return ACPRegistryAgent{
		ID:          a.ID,
		Name:        name,
		Version:     a.Version,
		Description: a.Description,
		Repository:  a.Repository,
		Authors:     append([]string(nil), a.Authors...),
		License:     a.License,
		LicenseURL:  a.LicenseURL,
		Distribution: ACPRegistryDistribution{NPX: &ACPRegistryPackage{
			Package: a.PackageSpec(),
			Args:    append([]string(nil), a.Args...),
		}},
	}, true
}

// ACPRegistryManifest renders every pinned, valid adapter among tools as one
// registry index, id-sorted, indented, with a trailing newline — the exact
// bytes bashy publishes. Retired tools are included: the adapter pin is a
// fact about the vendor, not about the roster.
func ACPRegistryManifest(tools []Tool) ([]byte, error) {
	idx := ACPRegistryIndex{Version: ACPRegistryVersion, Agents: []ACPRegistryAgent{}}
	for _, t := range tools {
		if err := t.ValidateACPAdapter(); err != nil {
			return nil, err
		}
		if e, ok := t.ACPRegistryEntry(); ok {
			idx.Agents = append(idx.Agents, e)
		}
	}
	sort.Slice(idx.Agents, func(i, j int) bool { return idx.Agents[i].ID < idx.Agents[j].ID })
	b, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
