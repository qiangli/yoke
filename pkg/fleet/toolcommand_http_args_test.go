package fleet

import (
	"strings"
	"testing"
)

const toolWithHTTPArgs = `name: svc
kind: cli
commands:
  - name: decide
    mode: http
    effects: [net, write]
    http:
      method: POST
      url: http://127.0.0.1:8787
      path: /decide
      token_ref: svc
      body:
        source: bashy
      args:
        - name: id
          field: request.id
          required: true
        - name: decision
          enum: {allow: approved, deny: rejected}
        - name: limit
          type: integer
          default: "10"
`

func TestToolCommandHTTPArgsParse(t *testing.T) {
	tl, err := ParseTool("svc", []byte(toolWithHTTPArgs), nil)
	if err != nil {
		t.Fatal(err)
	}
	if errs, _ := tl.ValidateCommands(); len(errs) != 0 {
		t.Fatalf("valid declaration refused: %v", errs)
	}
	c, _ := tl.Command("decide")
	args := c.HTTP.Args
	if len(args) != 3 || args[0].BodyField() != "request.id" || !args[0].Required || args[1].BodyField() != "decision" || args[2].Default != "10" {
		t.Fatalf("args = %+v", args)
	}
	if v, err := args[1].Project("deny"); err != nil || v != "rejected" {
		t.Fatalf("enum projection = %v, %v", v, err)
	}
	if _, err := args[1].Project("maybe"); err == nil || !strings.Contains(err.Error(), "allow|deny") {
		t.Fatalf("enum outside range: %v", err)
	}
	if v, err := args[2].Project("7"); err != nil || v != int64(7) {
		t.Fatalf("integer = %#v, %v", v, err)
	}
	if need, why := c.NeedsConfirmation(); !need || !strings.Contains(why, "write") {
		t.Fatalf("NeedsConfirmation = %v %q", need, why)
	}
}

func TestToolCommandHTTPArgsValidate(t *testing.T) {
	base := func(args []ToolCommandHTTPArg, body map[string]any) []ToolCommand {
		return []ToolCommand{{Name: "send", Mode: "http", HTTP: &ToolCommandHTTPAction{
			Method: "POST", URL: "http://127.0.0.1:1", TokenFile: "/tmp/token", Body: body, Args: args,
		}}}
	}
	cases := []struct {
		name    string
		args    []ToolCommandHTTPArg
		body    map[string]any
		wantErr string
	}{
		{"ok", []ToolCommandHTTPArg{{Name: "a"}, {Name: "b", Field: "x.b"}}, map[string]any{"x": map[string]any{"c": 1}}, ""},
		{"bad name", []ToolCommandHTTPArg{{Name: "1a"}}, nil, "name"},
		{"duplicate name", []ToolCommandHTTPArg{{Name: "a"}, {Name: "a", Field: "z"}}, nil, "duplicate"},
		{"same field", []ToolCommandHTTPArg{{Name: "a", Field: "f"}, {Name: "b", Field: "f"}}, nil, "overlaps"},
		{"prefix field", []ToolCommandHTTPArg{{Name: "a", Field: "f"}, {Name: "b", Field: "f.g"}}, nil, "overlaps"},
		{"bad field", []ToolCommandHTTPArg{{Name: "a", Field: "f..g"}}, nil, "dotted"},
		{"overwrites literal", []ToolCommandHTTPArg{{Name: "source"}}, map[string]any{"source": "bashy"}, "overwrite"},
		{"through scalar", []ToolCommandHTTPArg{{Name: "a", Field: "source.x"}}, map[string]any{"source": "bashy"}, "non-object"},
		{"mixes {args}", []ToolCommandHTTPArg{{Name: "a"}}, map[string]any{"text": "{args}"}, "{args}"},
		{"bad type", []ToolCommandHTTPArg{{Name: "a", Type: "float"}}, nil, "type"},
		{"type and enum", []ToolCommandHTTPArg{{Name: "a", Type: "string", Enum: map[string]any{"x": "y"}}}, nil, "not both"},
		{"enum object value", []ToolCommandHTTPArg{{Name: "a", Enum: map[string]any{"x": map[string]any{}}}}, nil, "want a string"},
		{"enum empty key", []ToolCommandHTTPArg{{Name: "a", Enum: map[string]any{"": "y"}}}, nil, "empty key"},
		{"required default", []ToolCommandHTTPArg{{Name: "a", Required: true, Default: "x"}}, nil, "exclusive"},
		{"bad default", []ToolCommandHTTPArg{{Name: "a", Type: "integer", Default: "ten"}}, nil, "default"},
		{"negative max_len", []ToolCommandHTTPArg{{Name: "a", MaxLen: -1}}, nil, "negative"},
	}
	tool := Tool{Name: "svc", Kind: ToolKindCLI}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tl := tool
			tl.Commands = base(tc.args, tc.body)
			errs, _ := tl.ValidateCommands()
			if tc.wantErr == "" {
				if len(errs) != 0 {
					t.Fatalf("want valid, got %v", errs)
				}
				return
			}
			if len(errs) == 0 || !strings.Contains(errs[0].Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, errs)
			}
		})
	}
}

func TestToolCommandNeedsConfirmation(t *testing.T) {
	for _, tc := range []struct {
		cmd  ToolCommand
		want bool
	}{
		{ToolCommand{}, false},
		{ToolCommand{Effects: []string{"net", "read"}}, false},
		{ToolCommand{Effects: []string{"net", "write"}}, true},
		{ToolCommand{Effects: []string{"spend"}}, true},
		{ToolCommand{Effects: []string{"destroy"}}, true},
		{ToolCommand{Confirm: true}, true},
	} {
		if got, _ := tc.cmd.NeedsConfirmation(); got != tc.want {
			t.Errorf("%+v NeedsConfirmation = %v, want %v", tc.cmd, got, tc.want)
		}
	}
}
