package dagrun

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"

	"github.com/qiangli/yoke/pkg/dag"
)

const (
	testDigestAMD64 = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	testDigestARM64 = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	testDigestMac   = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
	testRecord      = "sha256:4444444444444444444444444444444444444444444444444444444444444444"
)

// testInput is a one-shot render command with every field the skeleton
// carries; tests mutate a copy.
func testInput() GenerateInput {
	return GenerateInput{
		Command: CommandRef{
			Name:   "render",
			Digest: testRecord,
			Artifacts: map[string]string{
				"linux/amd64":  testDigestAMD64,
				"linux/arm64":  testDigestARM64,
				"darwin/arm64": testDigestMac,
			},
		},
		Argv:        []string{"render", "report.md", "-o", "out/report.pdf"},
		WorkspaceID: "ws-01jb6zexample0000000000",
		RunID:       "01JB7CEXAMPLE0000000000000",
		Cwd:         "~/docs",
		Env:         map[string]string{"LANG": "C.UTF-8", "TZ": "UTC"},
		Venue:       dag.VenueCluster,
		Match:       map[string]string{"kubernetes.io/os": "linux"},
		Run: RunSpec{
			Kind:         RunOneShot,
			Resources:    Resources{CPU: 2, Memory: "4Gi"},
			Egress:       Egress{Mode: EgressNone},
			Distribution: string(dag.DistributionSingle),
			Timeout:      30 * time.Minute,
			Outputs:      []string{"out/report.pdf"},
		},
	}
}

func TestGenerateIsByteIdenticalForSameInputs(t *testing.T) {
	first, err := Generate(testInput())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for i := 0; i < 20; i++ {
		// A fresh input each time: map iteration order differs between runs,
		// so only sorted output survives this loop.
		again, err := Generate(testInput())
		if err != nil {
			t.Fatalf("Generate #%d: %v", i, err)
		}
		if !bytes.Equal(first, again) {
			t.Fatalf("Generate #%d differs:\n--- first\n%s\n--- again\n%s", i, first, again)
		}
	}
}

func TestGenerateGolden(t *testing.T) {
	got, err := Generate(testInput())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	golden := filepath.Join("testdata", "generate", "one-shot.dag.md")
	if os.Getenv("DAGRUN_UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (DAGRUN_UPDATE_GOLDEN=1 to create): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("manifest differs from %s:\n--- got\n%s\n--- want\n%s", golden, got, want)
	}
}

// bodyArgv parses the single simple command in a dag body back into argv
// with the shell's own parser, so the round trip is judged by a shell, not by
// the quoting code under test.
func bodyArgv(t *testing.T, body string) []string {
	t.Helper()
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(body), "body")
	if err != nil {
		t.Fatalf("parse body %q: %v", body, err)
	}
	if len(f.Stmts) != 1 {
		t.Fatalf("body has %d statements, want 1: %q", len(f.Stmts), body)
	}
	call, ok := f.Stmts[0].Cmd.(*syntax.CallExpr)
	if !ok || len(call.Assigns) != 0 {
		t.Fatalf("body is not one plain simple command: %q", body)
	}
	// No env, no ReadDir: nothing expands but quote removal, as a shell
	// running the body with every word quoted would do.
	argv, err := expand.Fields(&expand.Config{}, call.Args...)
	if err != nil {
		t.Fatalf("expand words: %v", err)
	}
	return argv
}

func parseManifest(t *testing.T, m []byte) *dag.Task {
	t.Helper()
	doc, err := dag.Parse(bytes.NewReader(m), "manifest.dag.md")
	if err != nil {
		t.Fatalf("dag.Parse: %v\n%s", err, m)
	}
	if doc.FrontmatterYAMLErr != "" {
		t.Fatalf("frontmatter is not strict YAML: %s", doc.FrontmatterYAMLErr)
	}
	if doc.Type != "dag" {
		t.Fatalf("type = %q, want dag", doc.Type)
	}
	if len(doc.Tasks) != 1 || doc.Tasks[0].Name != "run" {
		t.Fatalf("tasks = %v, want exactly [run]", doc.Order)
	}
	return doc.Tasks[0]
}

func TestGenerateArgvRoundTripsThroughDagBody(t *testing.T) {
	cases := [][]string{
		{"sh", "-c", "for i in 1 2 3; do echo tick $i; sleep 5; done; exit 7"},
		{"echo", "two  spaces", "tab\there", "", " lead", "trail "},
		{"printf", `it's "quoted" \back\slash`, "$HOME", "`date`", "*.go", "~"},
		{"printf", "line1\nline2\n", "```", "\n```\n", "## run", "---"},
		{"cr", "a\r\nb", "\x01ctl", "unicodé ✓"},
	}
	for _, argv := range cases {
		in := testInput()
		in.Argv = argv
		m, err := Generate(in)
		if err != nil {
			t.Fatalf("Generate(%q): %v", argv, err)
		}
		task := parseManifest(t, m)
		if task.Lang != "sh" {
			t.Fatalf("body lang = %q, want sh", task.Lang)
		}
		if got := bodyArgv(t, task.Body); !reflect.DeepEqual(got, argv) {
			t.Fatalf("argv round trip:\n got %q\nwant %q\nbody %q", got, argv, task.Body)
		}
	}
}

func TestGeneratedManifestValidatesForPipeline(t *testing.T) {
	in := testInput()
	in.Match["outpost.dhnt.io/host"] = "worker-a"
	in.Run.Accelerator = Accelerator{Kind: "gpu", Family: "cuda", Count: 1, Memory: "16Gi"}
	in.Run.Credentials = []string{"vendor/subscription"}
	m, err := Generate(in)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	task := parseManifest(t, m)
	spec := dag.SpecFor(task)
	if err := spec.ValidateForPipeline(); err != nil {
		t.Fatalf("ValidateForPipeline: %v\n%s", err, m)
	}
	if spec.Venue != dag.VenueCluster || spec.Distribution != dag.DistributionSingle {
		t.Fatalf("venue/distribution = %q/%q", spec.Venue, spec.Distribution)
	}
	if spec.CPUPerTask != 2 || spec.MemPerTask != 4<<30 {
		t.Fatalf("cpu/mem = %d/%d", spec.CPUPerTask, spec.MemPerTask)
	}
	if spec.Accelerator.Kind != "gpu" || spec.Accelerator.Family != "cuda" ||
		spec.Accelerator.Count != 1 || spec.Accelerator.MemoryBytes != 16<<30 {
		t.Fatalf("accelerator = %+v", spec.Accelerator)
	}
	if spec.Retries != 0 || spec.Timeout != 30*time.Minute {
		t.Fatalf("retries/timeout = %d/%v", spec.Retries, spec.Timeout)
	}
	if spec.Match["kubernetes.io/os"] != "linux" || spec.Match["outpost.dhnt.io/host"] != "worker-a" {
		t.Fatalf("match = %v", spec.Match)
	}
	if !reflect.DeepEqual(task.Env, []string{"LANG=C.UTF-8", "TZ=UTC"}) {
		t.Fatalf("env = %q", task.Env)
	}
	if !reflect.DeepEqual(task.Secrets, []string{"vendor/subscription"}) {
		t.Fatalf("secrets = %q", task.Secrets)
	}
	if !reflect.DeepEqual(task.Artifacts, []string{"out/report.pdf"}) {
		t.Fatalf("artifacts = %q", task.Artifacts)
	}

	man, err := ParseManifest(m)
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if man.Meta.RunKind != RunOneShot || man.Meta.Workspace != in.WorkspaceID || man.Meta.RunID != in.RunID ||
		man.Meta.Command.Name != "render" || man.Meta.Command.Digest != testRecord ||
		man.Meta.Artifact["linux/arm64"] != testDigestARM64 || man.Meta.Cwd != "~/docs" ||
		man.Meta.Egress != EgressNone {
		t.Fatalf("metadata = %+v", man.Meta)
	}
}

func TestGenerateFailsClosed(t *testing.T) {
	cases := map[string]func(*GenerateInput){
		"empty argv":            func(in *GenerateInput) { in.Argv = nil },
		"unknown kind":          func(in *GenerateInput) { in.Run.Kind = "daemon" },
		"no command digest":     func(in *GenerateInput) { in.Command.Digest = "" },
		"bad artifact digest":   func(in *GenerateInput) { in.Command.Artifacts["linux/amd64"] = "md5:abc" },
		"missing pinned arch":   func(in *GenerateInput) { in.Match["kubernetes.io/arch"] = "riscv64" },
		"missing pinned os":     func(in *GenerateInput) { in.Match["kubernetes.io/os"] = "windows" },
		"no run id":             func(in *GenerateInput) { in.RunID = "" },
		"unsafe workspace":      func(in *GenerateInput) { in.WorkspaceID = "../etc" },
		"unknown venue":         func(in *GenerateInput) { in.Venue = "moon" },
		"no venue":              func(in *GenerateInput) { in.Venue = "" },
		"unknown distribution":  func(in *GenerateInput) { in.Run.Distribution = "scatter" },
		"unknown egress":        func(in *GenerateInput) { in.Run.Egress.Mode = "everywhere" },
		"bad memory":            func(in *GenerateInput) { in.Run.Resources.Memory = "lots" },
		"negative cpu":          func(in *GenerateInput) { in.Run.Resources.CPU = -1 },
		"env value with space":  func(in *GenerateInput) { in.Env["PS1"] = "$ " },
		"env value with ${":     func(in *GenerateInput) { in.Env["X"] = "${HOME}" },
		"bad env key":           func(in *GenerateInput) { in.Env["1X"] = "y" },
		"cwd newline":           func(in *GenerateInput) { in.Cwd = "a\n---" },
		"ports on one-shot":     func(in *GenerateInput) { in.Run.Ports = []Port{{Name: "http", Port: 8080}} },
		"reach match key":       func(in *GenerateInput) { in.Match["host"] = "worker-a" },
		"credential with space": func(in *GenerateInput) { in.Run.Credentials = []string{"a b"} },
		"accelerator no kind":   func(in *GenerateInput) { in.Run.Accelerator = Accelerator{Family: "cuda", Count: 1} },
		"nul in argv":           func(in *GenerateInput) { in.Argv = []string{"echo", "a\x00b"} },
		"image newline":         func(in *GenerateInput) { in.Command.Image = "img:1\n---" },
		"release newline":       func(in *GenerateInput) { in.Command.Release = "v1\nx" },
	}
	for name, mutate := range cases {
		in := testInput()
		mutate(&in)
		if m, err := Generate(in); err == nil {
			t.Errorf("%s: Generate succeeded, want error:\n%s", name, m)
		} else if !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: error %v is not ErrInvalidInput", name, err)
		}
	}
}

func TestGenerateDefaults(t *testing.T) {
	in := testInput()
	in.Run = RunSpec{}
	in.Match = nil
	m, err := Generate(in)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	man, err := ParseManifest(m)
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if man.Meta.RunKind != RunOneShot || man.Meta.Egress != EgressNone || man.Task.Distribution != dag.DistributionSingle {
		t.Fatalf("defaults = kind %q egress %q distribution %q", man.Meta.RunKind, man.Meta.Egress, man.Task.Distribution)
	}
	if !strings.Contains(string(m), "\nRetries: 0\n") {
		t.Fatalf("manifest lacks Retries: 0:\n%s", m)
	}
}

// A shipped command's release and executor image are recorded in the
// manifest; a registered command's description is unchanged.
func TestGenerateRecordsShippedReleaseAndImage(t *testing.T) {
	in := testInput()
	in.Command = CommandRef{Name: "bashy", Digest: testRecord, Release: "v1.2.3 656d72b8", Image: "localhost/bashy-scratch:656d72b8"}
	in.Match = nil
	m, err := Generate(in)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if _, err := ParseManifest(m); err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	want := "bashy run manifest for bashy (shipped: bashy release v1.2.3 656d72b8, image localhost/bashy-scratch:656d72b8) (generated; do not edit)"
	if !strings.Contains(string(m), want) {
		t.Errorf("manifest lacks %q:\n%s", want, m)
	}
	reg, err := Generate(testInput())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(reg), "description: bashy run manifest for render (generated; do not edit)\n") {
		t.Errorf("registered description changed:\n%s", reg)
	}
}
