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

// Sprint #626 S5: email.send never selects a read-only command, whether it is
// the seeded claude:email.read or an overlay named for the bare service.
func TestSendRoleNeverSelectsReadOnlyCommand(t *testing.T) {
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	claude, ok := fleet.New(fleet.WithRoot(t.TempDir())).Tool("claude")
	if !ok {
		t.Fatal("baseline claude tool missing")
	}
	claude.CLI.Binary = bin
	overlay := fleet.Tool{Name: "vendor", CLI: fleet.ToolCLI{Binary: bin}, Commands: []fleet.ToolCommand{{Name: "email", Effects: []string{"read", "net"}}, {Name: "drive"}}}
	tools := []fleet.Tool{claude, overlay}
	for _, role := range []string{"email.send", "drive.write"} {
		for _, yes := range []bool{false, true} {
			opt := Options{Role: role, Yes: yes}
			candidates := Project(nil, tools, opt)
			for _, c := range candidates {
				if c.Rejected == "" {
					t.Errorf("%s (yes=%v): read-only %s is selectable", role, yes, c.Name)
				}
			}
			if got, err := Select(candidates, opt); err == nil {
				t.Errorf("%s (yes=%v) selected %+v", role, yes, got)
			}
			for _, p := range []string{"claude", "claude:email.read", "vendor", "vendor:email", "vendor:drive"} {
				opt.Provider = p
				if got, err := Select(Project(nil, tools, opt), opt); err == nil {
					t.Errorf("%s --provider %s selected %+v", role, p, got)
				}
			}
		}
	}
	for role, want := range map[string]string{"email.read": "claude:email.read", "drive.read": "claude:drive.read"} {
		opt := Options{Role: role, Provider: "claude"}
		if got, err := Select(Project(nil, tools, opt), opt); err != nil || got.Name != want {
			t.Errorf("%s = %+v, %v; want %s", role, got, err, want)
		}
	}
	// A bare-service command that declares the send effect may send, with --yes.
	sender := fleet.Tool{Name: "mailer", CLI: fleet.ToolCLI{Binary: bin}, Commands: []fleet.ToolCommand{{Name: "email", Effects: []string{"net", "send"}}}}
	opt := Options{Role: "email.send", Yes: true}
	if got, err := Select(Project(nil, []fleet.Tool{sender}, opt), opt); err != nil || got.Name != "mailer:email" {
		t.Errorf("declared sender = %+v, %v", got, err)
	}
}
