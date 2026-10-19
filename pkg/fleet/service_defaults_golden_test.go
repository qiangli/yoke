package fleet

import (
	"sort"
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
//
// Sprint 443 (story 0ad0d4f2eac6) extends the table with each unit's KEY
// settings — the values that decide what an enabled unit actually does
// (scope, write access, pool push, budget, role, resource limits). Release
// version pins are deliberately not pinned here; they move every bump.
type serviceDefaultGolden struct {
	enabled  bool
	reason   string
	settings map[string]string
}

type kv = map[string]string

var serviceDefaultsGolden = map[string]serviceDefaultGolden{
	"actrunner":     {false, "opt-in: the runner joins only when the operator points it at work", kv{"forge_unit": "loom", "labels": "host:host", "sandbox": "false", "docker_host": ""}},
	"apps":          {true, "default-on: paired hosts need the human front door after an upgrade", kv{"pair": "false", "launcher": ""}},
	"events":        {false, "opt-in: the event bus has no subscriber until the operator wires one", nil},
	"files":         {true, "default-on: pre-418 FilesOn defaulted on; embedded file manager is base host", kv{"scope": "", "allow_write": "false"}},
	"headlamp":      {false, "opt-in: the cluster dashboard needs an operator-attached cluster", kv{"kubeconfig": "", "namespace": "headlamp", "target": "svc/headlamp"}},
	"index":         {false, "opt-in: managed indexing requires explicit initialization", nil},
	"kopia":         {false, "opt-in: backups need an operator-configured destination", kv{"data_dir": ""}},
	"lan-inference": {false, "opt-in: explicit by history; publishing inference to the LAN is the operator's call", kv{"target": ""}},
	"llm":           {false, "opt-in: false since first commit; model routing is operator-configured", nil},
	"loom":          {false, "opt-in: false since first commit; background loom is opt-in", kv{"root_url": ""}},
	"meet":          {true, "default-on: paired hosts need meeting access after an upgrade", nil},
	"ollama":        {true, "default-on (S436): pre-418 OllamaOn defaulted on; paired hosts run local inference", kv{"url": ""}},
	"ollama-pool":   {true, "default-on (S436): the pool push runs alongside the local proxy by default", nil},
	"otel":          {true, "default-on: pre-418 OtelOn defaulted on; otel is required for DKS", kv{"pool": "true"}},
	"otel-stack":    {true, "default-on: local signed OTel stores and gateway are required for DKS telemetry", nil},
	"podman":        {true, "default-on: pre-418 PodmanOn defaulted on; socket passthrough is base host", kv{"socket": ""}},
	"sandbox":       {false, "opt-in: explicit by history; container execution limits are the operator's call", kv{"socket": "", "max_memory_mb": "0", "cpus": "0", "pids_limit": "0", "max_containers": "0", "allowed_images": ""}},
	"schedule":      {false, "opt-in: the scheduler runs only with operator-defined jobs", nil},
	"sdlc":          {false, "opt-in: the pipeline runner is enabled per repo by the operator", kv{"interval": "60s"}},
	"seaweedfs":     {false, "opt-in: false since first commit; distributed storage is opt-in", kv{"data_dir": ""}},
	"shard":         {true, "default-on (S436): pre-418 ShardOn was on for paired hosts; mesh formation needs it", kv{"role": "auto", "peers": "", "node_bytes": "0"}},
	"warm":          {true, "default-on: pre-418 warm followed OllamaOn; keeps preferred models resident", kv{"budget_frac": "0.33"}},
	"ycode":         {false, "opt-in: ycode sharing is enabled per host by the operator", kv{"share": "", "require_login": "false"}},
	"zot":           {false, "opt-in: false since first commit; the local registry is opt-in", kv{"data_dir": ""}},
}

// flippedToDefaultOn are the four units Sprint 437 flipped back to
// on-by-default; the golden test refuses a table edit that turns them off.
var flippedToDefaultOn = []string{"otel", "warm", "files", "podman"}

func TestServiceDefaultsGolden(t *testing.T) {
	for _, name := range flippedToDefaultOn {
		if !serviceDefaultsGolden[name].enabled {
			t.Errorf("%s must stay default-on (Sprint 437 a4300f7a)", name)
		}
	}
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
		for k, want := range g.settings {
			got, ok := s.Settings[k]
			if !ok {
				t.Errorf("%s settings.%s missing, want %q", s.Name, k, want)
			} else if got != want {
				t.Errorf("%s settings.%s = %q, want %q", s.Name, k, got, want)
			}
		}
	}
	for name, g := range serviceDefaultsGolden {
		if !seen[name] {
			t.Errorf("golden entry %s names no baseline unit (%s); drop the stale entry", name, g.reason)
		}
	}
}

// defaultOnUnits is every golden unit whose seed is enabled.
func defaultOnUnits() []string {
	var out []string
	for name, g := range serviceDefaultsGolden {
		if g.enabled {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// An explicit operator opt-out beats the default-on seed for EVERY default-on
// unit, and survives a fresh catalog load (a restart / upgrade): the disable
// lands as a local-ring overlay, never silently re-enabled, and removing it
// returns to the enabled seed. See docs/capability-change-policy.md.
func TestServiceDefaultsGoldenOptOut(t *testing.T) {
	opts := serviceOpts(t.TempDir())
	units := defaultOnUnits()
	for _, name := range units {
		mustSvc(t, opts, "", "disable", name)
	}
	for _, name := range units {
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
	// The opt-outs are independent: listing the whole catalog after a reload
	// still shows every one disabled.
	all, errs := New(opts...).Services()
	if len(errs) != 0 {
		t.Fatalf("parse errors after opt-outs: %v", errs)
	}
	for _, s := range all {
		if serviceDefaultsGolden[s.Name].enabled && s.Enabled {
			t.Errorf("%s re-enabled on reload despite explicit opt-out", s.Name)
		}
	}
	for _, name := range units {
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
