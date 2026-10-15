package snippet

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type quotaCfg struct {
	Limit Value[int]    `yaml:"limit" json:"limit"`
	Note  Value[string] `yaml:"note" json:"note"`
}

func TestValueLiterals(t *testing.T) {
	var cfg quotaCfg
	if err := yaml.Unmarshal([]byte("limit: 42\nnote: hi\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Limit.IsCode() || cfg.Note.IsCode() {
		t.Fatalf("literals decoded as code: %+v", cfg)
	}
	got, err := cfg.Limit.Resolve(context.Background())
	if err != nil || got != 42 {
		t.Fatalf("limit=%d err=%v", got, err)
	}
	note, err := cfg.Note.Resolve(context.Background())
	if err != nil || note != "hi" {
		t.Fatalf("note=%q err=%v", note, err)
	}
	var j quotaCfg
	if err := json.Unmarshal([]byte(`{"limit":7,"note":"yo"}`), &j); err != nil {
		t.Fatal(err)
	}
	if j.Limit.IsCode() {
		t.Fatal("JSON literal decoded as code")
	}
	if v, err := j.Limit.Resolve(context.Background()); err != nil || v != 7 {
		t.Fatalf("limit=%d err=%v", v, err)
	}
}

func TestValueYAMLCodeForms(t *testing.T) {
	var cfg struct {
		Scalar Value[int] `yaml:"scalar"`
		Long   Value[int] `yaml:"long"`
	}
	doc := "scalar: !bsh 'jq .x ~/quota.json'\n" +
		"long: !bsh\n  lang: bsh\n  body: echo 7\n  effects: [read]\n  timeout: 20s\n"
	if err := yaml.Unmarshal([]byte(doc), &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.Scalar.IsCode() || cfg.Scalar.Code == nil || cfg.Scalar.Code.Body != "jq .x ~/quota.json" {
		t.Fatalf("scalar: %+v", cfg.Scalar)
	}
	if cfg.Scalar.Code.Lang != "bsh" {
		t.Fatalf("scalar lang: %q", cfg.Scalar.Code.Lang)
	}
	if !cfg.Long.IsCode() || cfg.Long.Code.Body != "echo 7" || cfg.Long.Code.Timeout != 20e9 {
		t.Fatalf("long: %+v", cfg.Long.Code)
	}
	if !reflect.DeepEqual(cfg.Long.Code.Effects, []string{"read"}) {
		t.Fatalf("long effects: %v", cfg.Long.Code.Effects)
	}
}

func TestValueJSONCodeForms(t *testing.T) {
	var cfg struct {
		Short Value[int] `json:"short"`
		Long  Value[int] `json:"long"`
	}
	in := `{"short":{"$bsh":"echo 3"},"long":{"$bsh":{"lang":"bsh","body":"echo 4","effects":["read"],"timeout":"20s"}}}`
	if err := json.Unmarshal([]byte(in), &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.Short.IsCode() || cfg.Short.Code.Body != "echo 3" {
		t.Fatalf("short: %+v", cfg.Short)
	}
	if !cfg.Long.IsCode() || cfg.Long.Code.Body != "echo 4" || !reflect.DeepEqual(cfg.Long.Code.Effects, []string{"read"}) {
		t.Fatalf("long: %+v", cfg.Long.Code)
	}
}

func TestValueMarshalRoundTrip(t *testing.T) {
	type wrapper struct {
		V Value[int] `yaml:"v" json:"v"`
	}
	for _, tc := range []struct {
		name string
		in   wrapper
	}{
		{"lit", wrapper{V: Value[int]{Lit: 9, set: true}}},
		{"code", wrapper{V: Value[int]{Code: &Snippet{Lang: "bsh", Body: "echo 9"}, set: true}}},
		{"code-long", wrapper{V: Value[int]{Code: &Snippet{Lang: "bsh", Body: "echo 9", Effects: []string{"read"}, Timeout: 20e9}, set: true}}},
	} {
		y, err := yaml.Marshal(tc.in)
		if err != nil {
			t.Fatal(err)
		}
		var backY wrapper
		if err := yaml.Unmarshal(y, &backY); err != nil {
			t.Fatalf("%s: %v\n%s", tc.name, err, y)
		}
		if !reflect.DeepEqual(tc.in, backY) {
			t.Fatalf("%s yaml round-trip: %+v != %+v\n%s", tc.name, tc.in, backY, y)
		}
		j, err := json.Marshal(tc.in)
		if err != nil {
			t.Fatal(err)
		}
		var backJ wrapper
		if err := json.Unmarshal(j, &backJ); err != nil {
			t.Fatalf("%s: %v (%s)", tc.name, err, j)
		}
		if !reflect.DeepEqual(tc.in, backJ) {
			t.Fatalf("%s json round-trip: %+v != %+v (%s)", tc.name, tc.in, backJ, j)
		}
	}
	// The code forms are spelled !bsh / $bsh on the wire.
	y, _ := yaml.Marshal(wrapper{V: Value[int]{Code: &Snippet{Body: "echo 1"}, set: true}})
	if !strings.Contains(string(y), "!bsh echo 1") {
		t.Fatalf("yaml does not re-emit !bsh:\n%s", y)
	}
	j, _ := json.Marshal(wrapper{V: Value[int]{Code: &Snippet{Body: "echo 1"}, set: true}})
	if string(j) != `{"v":{"$bsh":"echo 1"}}` {
		t.Fatalf("json does not re-emit $bsh: %s", j)
	}
}

func TestValueUnmarshalNeverRunsCode(t *testing.T) {
	ran := false
	SetRunner(func(_ context.Context, _ Snippet, _ IO) (Result, error) {
		ran = true
		return Result{Stdout: []byte("42\n")}, nil
	})
	defer SetRunner(nil)
	var cfg struct {
		Limit Value[int] `yaml:"limit"`
	}
	for _, doc := range []string{
		"limit: !bsh 'exit 1'\n",
		"limit: !bsh\n  body: exit 1\n  effects: [net]\n",
	} {
		if err := yaml.Unmarshal([]byte(doc), &cfg); err != nil {
			t.Fatal(err)
		}
		if ran {
			t.Fatalf("unmarshal ran code: %s", doc)
		}
		if !cfg.Limit.IsCode() {
			t.Fatalf("code not kept: %s", doc)
		}
	}
	var j struct {
		Limit Value[int] `json:"limit"`
	}
	if err := json.Unmarshal([]byte(`{"limit":{"$bsh":"exit 1"}}`), &j); err != nil {
		t.Fatal(err)
	}
	if ran || !j.Limit.IsCode() {
		t.Fatal("json unmarshal ran code or dropped it")
	}
}

func TestValueResolveParsesStdout(t *testing.T) {
	SetRunner(func(_ context.Context, _ Snippet, _ IO) (Result, error) {
		return Result{Stdout: []byte("  42 \n")}, nil
	})
	defer SetRunner(nil)
	v := Value[int]{Code: &Snippet{Body: "echo 42"}, set: true}
	got, err := v.Resolve(context.Background())
	if err != nil || got != 42 {
		t.Fatalf("got=%d err=%v", got, err)
	}
	// JSON stdout parses through the YAML superset.
	type rec struct {
		Name string `yaml:"name"`
		N    int    `yaml:"n"`
	}
	SetRunner(func(_ context.Context, _ Snippet, _ IO) (Result, error) {
		return Result{Stdout: []byte(`{"name":"x","n":3}`)}, nil
	})
	sv := Value[rec]{Code: &Snippet{Body: "emit json"}, set: true}
	r, err := sv.Resolve(context.Background())
	if err != nil || r.Name != "x" || r.N != 3 {
		t.Fatalf("got=%+v err=%v", r, err)
	}
	// Non-zero exit is an error, never a value.
	SetRunner(func(_ context.Context, _ Snippet, _ IO) (Result, error) {
		return Result{Stderr: []byte("boom"), Exit: 2}, nil
	})
	if _, err := v.Resolve(context.Background()); err == nil || !strings.Contains(err.Error(), "exit 2") {
		t.Fatalf("err=%v", err)
	}
}

func TestValueConsent(t *testing.T) {
	ran := false
	SetRunner(func(_ context.Context, _ Snippet, _ IO) (Result, error) {
		ran = true
		return Result{Stdout: []byte("1\n")}, nil
	})
	defer SetRunner(nil)
	net := Value[int]{Code: &Snippet{Body: "curl example.invalid", Effects: []string{"net"}}, set: true}
	if _, err := net.Resolve(context.Background()); !errors.Is(err, ErrConsentRequired) {
		t.Fatalf("err=%v", err)
	}
	if ran {
		t.Fatal("ran without consent")
	}
	if _, err := net.Resolve(WithConsent(context.Background())); err != nil {
		t.Fatalf("with consent: %v", err)
	}
	if !ran {
		t.Fatal("did not run with consent")
	}
	// pure and read never need consent.
	read := Value[int]{Code: &Snippet{Body: "cat quota", Effects: []string{"read"}}, set: true}
	if _, err := read.Resolve(context.Background()); err != nil {
		t.Fatalf("read needed consent: %v", err)
	}
}
