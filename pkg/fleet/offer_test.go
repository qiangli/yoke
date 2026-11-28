package fleet_test

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/offer"
	"github.com/qiangli/yoke/pkg/toolcmd"
)

func TestOfferDefinitions(t *testing.T) {
	c := fleet.New(fleet.WithRoot(t.TempDir()), fleet.WithoutCloudOverlay())
	schema := &fleet.CommandSchema{Flags: []fleet.CommandFlag{{Name: "count", Type: "int", Required: true}}}
	cmd := fleet.ToolCommand{Name: "convert", Description: "Convert a sample", Args: schema, Capability: "audio", Unit: "seconds", Effects: []string{"read"}, Mode: "print", Slash: "/convert {args}"}
	for _, err := range []error{
		c.SaveTool(fleet.Tool{Name: "fixture-engine", Commands: []fleet.ToolCommand{cmd}}),
		c.SaveModel(fleet.Model{Name: "fixture-model"}),
		c.SaveAgent(fleet.Agent{Name: "fixture-agent", Tool: "fixture-engine", Model: "fixture-model"}),
		c.SaveCommand(fleet.Command{Name: "fixture-counter", Synopsis: "Count samples", Exec: []string{"fixture-counter"}, Effects: []string{"read"}, Args: schema}),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ name, class, unit, field string }{
		{"fixture-model", "model", "token", "prompt"}, {"fixture-agent", "agent", "token", "instruction"},
		{"fixture-engine:convert", "tool", "seconds", "count"}, {"fixture-counter", "tool", "call", "count"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := offer.Offerable(c, tc.name)
			if err != nil {
				t.Fatal(err)
			}
			props := d.Input["properties"].(map[string]any)
			if d.Name != tc.name || d.Class != tc.class || d.Unit != tc.unit || d.Description == "" || len(d.Effects) == 0 || props[tc.field] == nil || d.Output.Type == "" {
				t.Fatalf("definition %+v", d)
			}
			digest, err := d.Digest()
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(d)
			var canonical map[string]any
			if err := json.Unmarshal(raw, &canonical); err != nil {
				t.Fatal(err)
			}
			raw, _ = json.Marshal(canonical)
			if want := fmt.Sprintf("%x", sha256.Sum256(raw)); digest != want {
				t.Fatalf("digest %s, want %s", digest, want)
			}
			if tc.class == "tool" {
				return
			}
			if d.Output.Properties["usage"] == nil || len(props[tc.class].(map[string]any)["enum"].([]any)) != 1 {
				t.Fatal("missing usage or fixed target")
			}
			args := map[string]any{tc.field: "hello"}
			if tc.class == "agent" {
				args["token_cap"] = 12
			}
			argv, err := d.OneShotArgv(args)
			if err != nil || !slices.Contains(argv, tc.name) {
				t.Fatalf("bound args %q: %v", argv, err)
			}
			if tc.class == "agent" && !strings.Contains(strings.Join(argv, " "), "--max-tokens 12") {
				t.Fatal(argv)
			}
			args[tc.class] = "different"
			if _, err = d.OneShotArgv(args); err == nil {
				t.Fatal("redirected fixed target")
			}
		})
	}
	d, err := offer.Offerable(c, "fixture-engine")
	if err != nil {
		t.Fatal(err)
	}
	if !d.HasTag("audio") {
		t.Fatal("capability missing from tags")
	}
	original, _ := d.Digest()
	tagged := d.WithTags("private,video", "audio")
	reordered := d.WithTags("video", "private")
	listing := []offer.ToolDefinition{d, tagged}
	if got := offer.FilterByTag(listing, "private"); len(got) != 1 || !got[0].HasTag("video") {
		t.Fatalf("filtered listing %+v", got)
	}
	if got := offer.TagsInUse(listing); !slices.Equal(got, []string{"audio", "private", "video"}) {
		t.Fatalf("listing tags %q", got)
	}
	first, _ := tagged.Digest()
	second, _ := reordered.Digest()
	if first == original || first != second || !tagged.HasTag("video") || d.HasTag("video") {
		t.Fatal("tag filtering or canonical digest")
	}
	admission := func(d offer.ToolDefinition) error {
		return toolcmd.Admit(fleet.ToolCommand{Effects: d.Effects}, toolcmd.Options{})
	}
	if admission(tagged) != nil {
		t.Fatal("tag affected admission")
	}
	cmd.Effects = []string{"spend"}
	d.Effects = cmd.Effects
	if !errors.Is(admission(d.WithTags("free")), toolcmd.ErrNeedsConfirmation) {
		t.Fatal("tag bypassed effect admission")
	}
	d, err = offer.Offerable(c, "fixture-engine")
	if err != nil {
		t.Fatal(err)
	}
	d.Description += " changed"
	modified, _ := d.Digest()
	if modified == original {
		t.Fatal("description absent from digest")
	}
	d, err = offer.Offerable(c, "fixture-engine")
	if err != nil {
		t.Fatal(err)
	}
	d.Input["properties"].(map[string]any)["count"] = map[string]any{"type": "boolean"}
	modified, _ = d.Digest()
	if modified == original {
		t.Fatal("schema absent from digest")
	}
}

func TestOfferRefusals(t *testing.T) {
	c := fleet.New(fleet.WithRoot(t.TempDir()), fleet.WithReservedNames(func(name string) (string, bool) { return "mounted verb", name == "custom-mounted" }))
	if err := c.SaveCommand(fleet.Command{Name: "legacy-example", Exec: []string{"example", "{args}"}}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"bash", "Bash#", "ls", "coreutils", "ast", "kb", "read", "write", "edit", "search", "todo", "custom-mounted", "legacy-example", "unknown-example"} {
		_, err := offer.Offerable(c, name)
		var refusal *fleet.OfferError
		if !errors.As(err, &refusal) || refusal.Name != name || refusal.Reason == "" {
			t.Fatalf("%q: %v", name, err)
		}
		if name == "legacy-example" && !strings.Contains(refusal.Reason, "args") {
			t.Fatal(err)
		}
		if name != "legacy-example" && name != "unknown-example" && strings.Contains(refusal.Reason, "no fleet") {
			t.Fatal(err)
		}
	}
}

func TestOfferTypedHTTPCommand(t *testing.T) {
	c := fleet.New(fleet.WithRoot(t.TempDir()))
	cmd := fleet.ToolCommand{Name: "render", Mode: "http", Capability: "image-generation", HTTP: &fleet.ToolCommandHTTPAction{
		Method: "POST", URL: "http://localhost", TokenRef: "fixture", Args: []fleet.ToolCommandHTTPArg{{Name: "size", Type: "integer", MaxLen: 10, Required: true}, {Name: "style", Enum: map[string]any{"ink": 1, "pencil": 2}}},
	}}
	t1 := fleet.Tool{Name: "fixture-renderer", Commands: []fleet.ToolCommand{cmd}}
	if err := c.SaveTool(t1); err != nil {
		t.Fatal(err)
	}
	d, err := offer.Offerable(c, t1.Name)
	if err != nil {
		t.Fatal(err)
	}
	props := d.Input["properties"].(map[string]any)
	if !d.HasTag(cmd.Capability) || props["size"].(map[string]any)["type"] != "integer" || !slices.Equal(props["style"].(map[string]any)["enum"].([]any), []any{"ink", "pencil"}) {
		t.Fatalf("definition %+v", d)
	}
	t1.Commands[0].HTTP.Args = nil
	if err := c.SaveTool(t1); err != nil {
		t.Fatal(err)
	}
	_, err = offer.Offerable(c, t1.Name)
	var refusal *fleet.OfferError
	if !errors.As(err, &refusal) || !strings.Contains(refusal.Reason, "args") {
		t.Fatalf("untyped HTTP command: %v", err)
	}
}

// A registered command offered as a tool keeps its declared unit (Sprint 739
// K5: transcribe meters seconds), and an unknown unit is refused at write.
func TestRegisteredOfferCommandUnit(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	c := fleet.New(fleet.WithRoot(t.TempDir()), fleet.WithoutCloudOverlay())
	rec := fleet.Command{Name: "unit-fixture", Exec: []string{"fixture"}, Synopsis: "fixture", Effects: []string{"read"}, Unit: "seconds",
		Args: &fleet.CommandSchema{Flags: []fleet.CommandFlag{{Name: "input", Required: true}}}}
	if err := c.SaveCommand(rec); err != nil {
		t.Fatal(err)
	}
	cmd, err := c.RegisteredOfferCommand(rec.Name)
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Unit != "seconds" {
		t.Fatalf("offered unit = %q, want seconds", cmd.Unit)
	}
	bad := rec
	bad.Name, bad.Unit = "unit-bad", "parsecs"
	if err := c.SaveCommand(bad); err == nil {
		t.Fatal("unit parsecs was accepted")
	}
}
