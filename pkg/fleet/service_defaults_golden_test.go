package fleet

import (
	"testing"

	"github.com/qiangli/yoke/pkg/assetring"
)

// Sprint 437 (story a4300f7a000a): golden table of every embedded baseline
// service unit's expected `enabled` default. Before Sprint 418, outpost
// defaulted podman, files, otel (+ otel pool) and warm on; yoke b18612f7
// seeded every in-process capability unit `enabled: false` in one blanket
// move with no recorded decision. Sprint 436 restored ollama, ollama-pool
// and shard; this story restores otel, warm, files and podman. Everything
// else stays opt-in by history.
//
// The test fails when (a) a baseline unit has no golden entry, (b) a golden
// entry names no baseline unit, or (c) a default differs from the golden
// value, so adding a unit without recording its default breaks the build.
type serviceDefaultGolden struct {
	enabled bool
	reason  string
}

var serviceDefaultsGolden = map[string]serviceDefaultGolden{
	"actrunner":     {false, "opt-in: the runner joins only when the operator points it at work"},
	"apps":          {true, "default-on: paired hosts need the human front door after an upgrade"},
	"events":        {false, "opt-in: the event bus has no subscriber until the operator wires one"},
	"files":         {true, "default-on: pre-418 FilesOn defaulted on; embedded file manager is base host"},
	"headlamp":      {false, "opt-in: the cluster dashboard needs an operator-attached cluster"},
	"kopia":         {false, "opt-in: backups need an operator-configured destination"},
	"lan-inference": {false, "opt-in: explicit by history; publishing inference to the LAN is the operator's call"},
	"llm":           {false, "opt-in: false since first commit; model routing is operator-configured"},
	"loom":          {false, "opt-in: false since first commit; background loom is opt-in"},
	"meet":          {true, "default-on: paired hosts need meeting access after an upgrade"},
	"ollama":        {true, "default-on (S436): pre-418 OllamaOn defaulted on; paired hosts run local inference"},
	"ollama-pool":   {true, "default-on (S436): the pool push runs alongside the local proxy by default"},
	"otel":          {true, "default-on: pre-418 OtelOn defaulted on; otel is required for DKS"},
	"otel-stack":    {true, "default-on: local signed OTel stores and gateway are required for DKS telemetry"},
	"podman":        {true, "default-on: pre-418 PodmanOn defaulted on; socket passthrough is base host"},
	"sandbox":       {false, "opt-in: explicit by history; container execution limits are the operator's call"},
	"schedule":      {false, "opt-in: the scheduler runs only with operator-defined jobs"},
	"sdlc":          {false, "opt-in: the pipeline runner is enabled per repo by the operator"},
	"seaweedfs":     {false, "opt-in: false since first commit; distributed storage is opt-in"},
	"shard":         {true, "default-on (S436): pre-418 ShardOn was on for paired hosts; mesh formation needs it"},
	"warm":          {true, "default-on: pre-418 warm followed OllamaOn; keeps preferred models resident"},
	"ycode":         {false, "opt-in: ycode sharing is enabled per host by the operator"},
	"zot":           {false, "opt-in: false since first commit; the local registry is opt-in"},
}

// flippedToDefaultOn are the four units this story flips back to
// on-by-default; the override tests below exercise exactly these.
var flippedToDefaultOn = []string{"otel", "warm", "files", "podman"}

func TestServiceDefaultsGolden(t *testing.T) {
	cat := New(serviceOpts(t.TempDir())...)
	units, errs := cat.Services()
	if len(errs) != 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	seen := map[string]bool{}
	for _, s := range units {
		if s.Ring != assetring.RingEmbedded {
			t.Errorf("%s ring = %v, want embedded (fresh host has no overlays)", s.Name, s.Ring)
			continue
		}
		g, ok := serviceDefaultsGolden[s.Name]
		if !ok {
			t.Errorf("baseline unit %s has no golden entry; record its default in serviceDefaultsGolden", s.Name)
			continue
		}
		seen[s.Name] = true
		if s.Enabled != g.enabled {
			t.Errorf("%s enabled = %v, want %v (%s)", s.Name, s.Enabled, g.enabled, g.reason)
		}
	}
	for name, g := range serviceDefaultsGolden {
		if !seen[name] {
			t.Errorf("golden entry %s names no baseline unit (%s); drop the stale entry", name, g.reason)
		}
	}
}

// An explicit operator opt-out beats the default-on seed: the disable lands
// as a local-ring overlay, and removing it returns to the enabled seed.
func TestServiceDefaultsGoldenOptOut(t *testing.T) {
	opts := serviceOpts(t.TempDir())
	for _, name := range flippedToDefaultOn {
		mustSvc(t, opts, "", "disable", name)
		s, ok := New(opts...).Service(name)
		if !ok {
			t.Fatalf("%s seed missing after disable", name)
		}
		if s.Enabled {
			t.Errorf("%s still enabled after operator disable", name)
		}
		if s.Ring != assetring.RingLocal {
			t.Errorf("%s ring = %v after disable, want local (explicit operator overlay)", name, s.Ring)
		}
	}
	for _, name := range flippedToDefaultOn {
		mustSvc(t, opts, "", "rm", name)
		s, _ := New(opts...).Service(name)
		if s.Ring.String() != "embedded" || !s.Enabled {
			t.Errorf("%s after rm = %+v, want the enabled embedded seed", name, s)
		}
	}
}

// The generic set/enable verbs still work on the flipped units: a settings
// write keeps the unit enabled, and enable after a disable returns to on.
func TestServiceDefaultsGoldenGenericVerbs(t *testing.T) {
	opts := serviceOpts(t.TempDir())
	mustSvc(t, opts, "", "set", "warm", "--set", "settings.budget_frac=0.50")
	if s, _ := New(opts...).Service("warm"); !s.Enabled || s.Settings["budget_frac"] != "0.50" {
		t.Fatalf("warm after set = %+v, want enabled with budget_frac 0.50", s)
	}
	mustSvc(t, opts, "", "set", "otel", "--set", "settings.pool=false")
	if s, _ := New(opts...).Service("otel"); !s.Enabled || s.Settings["pool"] != "false" {
		t.Fatalf("otel after set = %+v, want enabled with pool false", s)
	}
	// enable is a no-op success on an already-enabled seed.
	mustSvc(t, opts, "", "enable", "files")
	mustSvc(t, opts, "", "disable", "podman")
	mustSvc(t, opts, "", "enable", "podman")
	if s, _ := New(opts...).Service("podman"); !s.Enabled {
		t.Fatalf("podman after disable/enable = %+v, want enabled", s)
	}
}
