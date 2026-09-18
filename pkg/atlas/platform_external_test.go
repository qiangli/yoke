// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package atlas_test

import (
	"testing"

	"github.com/qiangli/yoke/external/act"
	"github.com/qiangli/yoke/external/gh"
	"github.com/qiangli/yoke/external/kopia"
	"github.com/qiangli/yoke/external/loom"
	"github.com/qiangli/yoke/external/mise"
	"github.com/qiangli/yoke/external/rclone"
	"github.com/qiangli/yoke/external/registry"
	"github.com/qiangli/yoke/external/seaweedfs"
	"github.com/qiangli/yoke/external/zot"
	"github.com/qiangli/yoke/pkg/atlas"
	"github.com/qiangli/yoke/pkg/binmgr"
)

// TestEveryExternalDeclaresPlatforms: a bin-managed external is never
// defaulted to "everywhere" — the tables panic at init without a row, and the
// declarative-registry CLIs (not in the tables) are checked here.
func TestEveryExternalDeclaresPlatforms(t *testing.T) {
	for _, n := range registry.Names() {
		d, ok := atlas.ExternalPlatforms(n)
		if !ok {
			t.Errorf("registry CLI %q has no platform declaration", n)
			continue
		}
		if len(d.OS) == 0 {
			t.Errorf("registry CLI %q declares no platforms", n)
		}
	}
	for _, n := range append(atlas.ToolNames(), atlas.VerbNames()...) {
		e, _ := atlas.Lookup(n)
		if e.Origin != atlas.OriginExternal || e.AliasOf != "" {
			continue
		}
		d, ok := atlas.ExternalPlatforms(n)
		if !ok {
			t.Errorf("external %q has no declaration", n)
			continue
		}
		onWindows := e.SupportedOn(atlas.OSWindows)
		if onWindows && d.WindowsAsset == "" {
			t.Errorf("%s: claims windows support but cites no windows asset", n)
		}
		if !onWindows && d.WindowsAsset != "" {
			t.Errorf("%s: cites a windows asset but is not declared for windows", n)
		}
	}
}

// TestWindowsAssetCitationsMatchTheResolvers feeds each cited windows asset
// name back to the package's own AssetMatch: the citation is then not prose
// but the string the resolver would pick on a windows host. (Whether the
// upstream still publishes it is a network question, answered by the
// provisioning path, not here.)
func TestWindowsAssetCitationsMatchTheResolvers(t *testing.T) {
	specs := map[string]binmgr.GitHubSpec{
		"act":       act.Spec(""),
		"gh":        gh.Spec(""),
		"kopia":     kopia.Spec(""),
		"loom":      loom.Spec(""),
		"mise":      mise.Spec(""),
		"rclone":    rclone.Spec(""),
		"seaweedfs": seaweedfs.Spec(""),
		"zot":       zot.Spec(""),
	}
	for name, spec := range specs {
		d, ok := atlas.ExternalPlatforms(name)
		if !ok {
			t.Errorf("%s: undeclared", name)
			continue
		}
		match := spec.AssetMatch
		if match == nil {
			match = binmgr.DefaultAssetMatch
		}
		if !match(d.WindowsAsset, "windows", "amd64") {
			t.Errorf("%s: cited windows asset %q is not what its resolver would pick", name, d.WindowsAsset)
		}
		if match(d.WindowsAsset, "linux", "amd64") {
			t.Errorf("%s: cited windows asset %q also matches linux — the citation is not windows-specific", name, d.WindowsAsset)
		}
	}
}
