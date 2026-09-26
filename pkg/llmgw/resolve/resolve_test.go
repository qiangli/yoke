package resolve

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseAutoSignal(t *testing.T) {
	for _, tt := range []struct {
		name         string
		bodyModel    string
		headerAuto   string
		headerMode   string
		headerNoRes  string
		wantModel    string
		wantEnabled  bool
		wantModeName string
	}{
		{
			name:        "no flag - exact only",
			bodyModel:   "qwen2.5-coder:7b",
			wantModel:   "qwen2.5-coder:7b",
			wantEnabled: false,
		},
		{
			name:         "header true",
			bodyModel:    "qwen2.5-coder:7b",
			headerAuto:   "true",
			wantModel:    "qwen2.5-coder:7b",
			wantEnabled:  true,
			wantModeName: ModePreferExact,
		},
		{
			name:         "header on",
			bodyModel:    "x",
			headerAuto:   "on",
			wantEnabled:  true,
			wantModel:    "x",
			wantModeName: ModePreferExact,
		},
		{
			name:         "header 1",
			bodyModel:    "x",
			headerAuto:   "1",
			wantEnabled:  true,
			wantModel:    "x",
			wantModeName: ModePreferExact,
		},
		{
			name:         "suffix only",
			bodyModel:    "qwen2.5-coder:7b@auto",
			wantModel:    "qwen2.5-coder:7b",
			wantEnabled:  true,
			wantModeName: ModePreferExact,
		},
		{
			name:         "suffix plus best-warm mode",
			bodyModel:    "qwen2.5-coder:7b@auto",
			headerMode:   "best-warm",
			wantModel:    "qwen2.5-coder:7b",
			wantEnabled:  true,
			wantModeName: ModeBestWarm,
		},
		{
			name:        "no-resolve wins over suffix",
			bodyModel:   "qwen2.5-coder:7b@auto",
			headerNoRes: "true",
			wantModel:   "qwen2.5-coder:7b",
			wantEnabled: false,
		},
		{
			name:        "no-resolve wins over header",
			bodyModel:   "x",
			headerAuto:  "true",
			headerNoRes: "1",
			wantModel:   "x",
			wantEnabled: false,
		},
		{
			name:        "header garbage = off",
			bodyModel:   "x",
			headerAuto:  "maybe",
			wantModel:   "x",
			wantEnabled: false,
		},
		{
			name:        "suffix mid-name is not the signal",
			bodyModel:   "model@auto-tuned",
			wantModel:   "model@auto-tuned",
			wantEnabled: false,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
			if tt.headerAuto != "" {
				r.Header.Set(AutoHeader, tt.headerAuto)
			}
			if tt.headerMode != "" {
				r.Header.Set(AutoModeHeader, tt.headerMode)
			}
			if tt.headerNoRes != "" {
				r.Header.Set(NoResolveHeader, tt.headerNoRes)
			}
			got := ParseAutoSignal(r, tt.bodyModel)
			if got.Model != tt.wantModel {
				t.Errorf("Model=%q, want %q", got.Model, tt.wantModel)
			}
			if got.AutoEnabled != tt.wantEnabled {
				t.Errorf("AutoEnabled=%v, want %v", got.AutoEnabled, tt.wantEnabled)
			}
			if got.AutoEnabled && got.Mode != tt.wantModeName {
				t.Errorf("Mode=%q, want %q", got.Mode, tt.wantModeName)
			}
			if !got.AutoEnabled && got.Mode != "" {
				t.Errorf("Mode=%q on a non-auto request, want empty", got.Mode)
			}
		})
	}
}

func TestIsTruthy(t *testing.T) {
	for _, on := range []string{"1", "true", "TRUE", " yes ", "On"} {
		if !IsTruthy(on) {
			t.Errorf("IsTruthy(%q) = false, want true", on)
		}
	}
	for _, off := range []string{"", "0", "false", "maybe", "y"} {
		if IsTruthy(off) {
			t.Errorf("IsTruthy(%q) = true, want false", off)
		}
	}
}

func TestPatchModelField(t *testing.T) {
	for _, tt := range []struct {
		name     string
		body     string
		newModel string
		wantErr  bool
	}{
		{
			name:     "flat chat body",
			body:     `{"model":"qwen2.5-coder:7b","messages":[{"role":"user","content":"hi"}]}`,
			newModel: "llama3.1:8b",
		},
		{
			name:     "empty body",
			body:     `{}`,
			newModel: "x",
		},
		{
			name:     "preserves nested fields",
			body:     `{"model":"a","stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"max_tokens":256}`,
			newModel: "b",
		},
		{
			name:    "non-object body",
			body:    `"just a string"`,
			wantErr: true,
		},
		{
			name:    "garbage",
			body:    `not json at all`,
			wantErr: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			out, err := PatchModelField([]byte(tt.body), tt.newModel)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil; out=%s", out)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			// Decode out — verify model field equals newModel and the
			// other top-level keys survived.
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(out, &raw); err != nil {
				t.Fatalf("decode patched body: %v", err)
			}
			var got string
			if err := json.Unmarshal(raw["model"], &got); err != nil {
				t.Fatalf("decode model field: %v", err)
			}
			if got != tt.newModel {
				t.Errorf("model=%q, want %q", got, tt.newModel)
			}
			// Verify other top-level keys survived (when present in input).
			var orig map[string]json.RawMessage
			_ = json.Unmarshal([]byte(tt.body), &orig)
			for k := range orig {
				if k == "model" {
					continue
				}
				if _, ok := raw[k]; !ok {
					t.Errorf("patched body lost top-level key %q", k)
				}
			}
		})
	}
}

func TestParseCapabilitySpec(t *testing.T) {
	for _, tt := range []struct {
		in        string
		wantTier  int
		wantDoms  []string
		shouldHit bool
	}{
		{"tier:L2/coding", TierL2, []string{"coding"}, true},
		{"tier:L4/coding,reasoning", TierL4, []string{"coding", "reasoning"}, true},
		{"tier:L1/*", TierL1, AllDomains, true},
		{"tier:L1/", TierL1, AllDomains, true},
		{"tier:L9/coding", 0, nil, false},
		{"l2/coding", 0, nil, false},
		{"qwen2.5-coder:7b", 0, nil, false},
		{"", 0, nil, false},
	} {
		t.Run(tt.in, func(t *testing.T) {
			tier, doms, ok := ParseCapabilitySpec(tt.in)
			if ok != tt.shouldHit {
				t.Fatalf("ok=%v, want %v", ok, tt.shouldHit)
			}
			if !ok {
				return
			}
			if tier != tt.wantTier {
				t.Errorf("tier=%d, want %d", tier, tt.wantTier)
			}
			if len(doms) != len(tt.wantDoms) {
				t.Errorf("domains=%v, want %v", doms, tt.wantDoms)
			}
		})
	}
}

func TestPatchModelField_VisionPayloadPreserved(t *testing.T) {
	// Sanity: a multi-MiB vision body still round-trips identically
	// except for the model field. Tiny synthetic version of the
	// pattern.
	bigText := strings.Repeat("blob", 5000) // 20 KiB
	body := `{"model":"old","messages":[{"role":"user","content":[{"type":"text","text":"` + bigText + `"}]}]}`
	out, err := PatchModelField([]byte(body), "new")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), bigText) {
		t.Fatal("vision-like text payload missing after patch — re-encoder dropped a field")
	}
	if !strings.Contains(string(out), `"model":"new"`) {
		t.Fatal("model field not rewritten")
	}
}
