package providerselector

import (
	"github.com/qiangli/yoke/pkg/fleet"
	"strings"
	"testing"
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
	tools := []fleet.Tool{{Name: "claude", Commands: []fleet.ToolCommand{{Name: "plan"}}}}
	opt := Options{Role: "plan"}
	got, err := Select(Project(nil, tools, opt), opt)
	if err != nil || got.Name != "claude:plan" || got.Route != "vendor" || got.Kind != "tool-command" {
		t.Fatalf("tool = %+v, %v", got, err)
	}
}
