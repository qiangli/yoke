package providerselector

import (
	"os"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
)

func TestRouteOrderAndOverride(t *testing.T) {
	records := []fleet.Command{
		{Name: "api", Route: "api", Provides: []string{"image.generate"}},
		{Name: "browser", Route: "browser", Provides: []string{"image.generate"}},
		{Name: "oss", Route: "oss", Provides: []string{"image.generate"}},
		{Name: "vendor", Route: "vendor", Provides: []string{"image.generate"}},
	}
	opt := Options{Role: "image.generate", Request: map[string]any{}}
	got, err := Select(Project(records, nil, opt), opt)
	if err != nil || got.Name != "oss" {
		t.Fatalf("auto = %v, %v", got, err)
	}
	for _, tc := range []struct{ route, want string }{{"vendor", "vendor"}, {"browser", "browser"}, {"api", "api"}} {
		opt.Route = tc.route
		got, err = Select(Project(records, nil, opt), opt)
		if err != nil || got.Name != tc.want {
			t.Fatalf("route %s = %v, %v", tc.route, got, err)
		}
	}
	opt.Route = "oss"
	opt.Provider = "api"
	got, err = Select(Project(records, nil, opt), opt)
	if err != nil || got.Name != "api" {
		t.Fatalf("override = %v, %v", got, err)
	}
}
func TestRefusalAndAmbiguity(t *testing.T) {
	records := []fleet.Command{{Name: "paid", Route: "api", Provides: []string{"email.send"}, Effects: []string{"spend"}}, {Name: "sending", Route: "api", Provides: []string{"email.send"}, Effects: []string{"send"}}}
	opt := Options{Role: "email.send"}
	candidates := Project(records, nil, opt)
	for _, c := range candidates {
		if !strings.Contains(c.Rejected, "--yes") {
			t.Fatalf("unguarded: %+v", c)
		}
	}
	if _, err := Select(candidates, opt); err == nil {
		t.Fatal("selected without --yes")
	}
	opt.Yes = true
	if _, err := Select(Project(records, nil, opt), opt); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguity = %v", err)
	}
	opt.Provider = "paid"
	got, err := Select(Project(records, nil, opt), opt)
	if err != nil || got.Name != "paid" {
		t.Fatalf("explicit = %v, %v", got, err)
	}
}
func TestToolCommandProjection(t *testing.T) {
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	tools := []fleet.Tool{{Name: "claude", CLI: fleet.ToolCLI{Binary: bin}, Commands: []fleet.ToolCommand{{Name: "plan"}}}}
	opt := Options{Role: "plan"}
	got, err := Select(Project(nil, tools, opt), opt)
	if err != nil || got.Name != "claude:plan" || got.Route != "vendor" || got.Kind != "tool-command" {
		t.Fatalf("tool = %+v, %v", got, err)
	}
}

func TestToolCommandSpendSendAndAbsentCLI(t *testing.T) {
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, effect := range []string{"spend", "send"} {
		tool := fleet.Tool{Name: "vendor", CLI: fleet.ToolCLI{Binary: bin}, Commands: []fleet.ToolCommand{{Name: "email.send", Effects: []string{effect}}}}
		opt := Options{Role: "email.send"}
		candidates := Project(nil, []fleet.Tool{tool}, opt)
		if len(candidates) != 1 || !strings.Contains(candidates[0].Rejected, "--yes") {
			t.Fatalf("effect %s not refused: %+v", effect, candidates)
		}
		opt.Yes = true
		if got, err := Select(Project(nil, []fleet.Tool{tool}, opt), opt); err != nil || got.Name != "vendor:email.send" {
			t.Fatalf("effect %s with --yes = %+v, %v", effect, got, err)
		}
	}
	missing := fleet.Tool{Name: "absent", CLI: fleet.ToolCLI{Binary: "no-such-provider-selector-test-binary"}, Commands: []fleet.ToolCommand{{Name: "plan"}}}
	opt := Options{Role: "plan", Provider: "absent:plan"}
	candidates := Project(nil, []fleet.Tool{missing}, opt)
	if len(candidates) != 1 || !strings.Contains(candidates[0].Rejected, "not installed") {
		t.Fatalf("absent CLI = %+v", candidates)
	}
	if _, err := Select(candidates, opt); err == nil {
		t.Fatal("selected absent CLI")
	}
}

func TestToolCommandDottedRoleMatchesHeadAndProviderPrefix(t *testing.T) {
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	tools := []fleet.Tool{{Name: "agy", CLI: fleet.ToolCLI{Binary: bin}, Commands: []fleet.ToolCommand{{Name: "video"}}}}
	opt := Options{Role: "video.understand"}
	got, err := Select(Project(nil, tools, opt), opt)
	if err != nil || got.Name != "agy:video" || got.Role != "video.understand" || got.Route != "vendor" {
		t.Fatalf("dotted role match = %+v, %v", got, err)
	}
	opt.Provider = "agy"
	got, err = Select(Project(nil, tools, opt), opt)
	if err != nil || got.Name != "agy:video" {
		t.Fatalf("provider prefix = %+v, %v", got, err)
	}
	opt.Provider = "agy:video"
	got, err = Select(Project(nil, tools, opt), opt)
	if err != nil || got.Name != "agy:video" {
		t.Fatalf("full provider = %+v, %v", got, err)
	}
}
