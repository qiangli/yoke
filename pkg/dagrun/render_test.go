package dagrun

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testImage = "registry.example/bashy/bashy:v0.0.0-test"

func linuxClass() ExecutorClass { return ExecutorClass{OS: "linux", Image: testImage} }

func mustGenerate(t *testing.T, in GenerateInput) []byte {
	t.Helper()
	m, err := Generate(in)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return m
}

// renderJSON is the golden form: encoding/json writes map keys sorted, so
// the bytes are a pure function of the objects.
func renderJSON(t *testing.T, objs []Object) []byte {
	t.Helper()
	b, err := json.MarshalIndent(objs, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return append(b, '\n')
}

func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	golden := filepath.Join("testdata", "render", name)
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
		t.Fatalf("render differs from %s:\n--- got\n%s\n--- want\n%s", golden, got, want)
	}
}

func renderCases() map[string]GenerateInput {
	oneShot := testInput()
	oneShot.Argv = []string{"sh", "-c", "for i in 1 2 3; do echo tick $i; sleep 5; done; exit 7"}

	session := testInput()
	session.Command = CommandRef{Name: "bashy", Digest: testRecord, Artifacts: map[string]string{"linux/amd64": testDigestAMD64}}
	session.Argv = []string{"bashy", "-i"}
	session.Cwd = "~/sub"
	session.Match = map[string]string{"kubernetes.io/os": "linux", "kubernetes.io/arch": "amd64", "outpost.dhnt.io/host": "worker-a"}
	session.Run = RunSpec{Kind: RunInteractive, Resources: Resources{CPU: 1, Memory: "512Mi"}, Egress: Egress{Mode: EgressInternet}}

	agent := testInput()
	agent.Command.Name = "agent+cli/v2"
	agent.Argv = []string{"agent", "--task", "say \"hi\""}
	agent.Cwd = ""
	agent.Env = nil
	agent.Match = nil
	agent.Run = RunSpec{
		Kind:        RunLongRunning,
		Accelerator: Accelerator{Kind: "gpu", Family: "cuda", Count: 2, Memory: "16Gi"},
		Timeout:     90 * time.Minute,
	}
	return map[string]GenerateInput{"one-shot": oneShot, "interactive": session, "long-running": agent}
}

func TestRenderLinuxGolden(t *testing.T) {
	for name, in := range renderCases() {
		t.Run(name, func(t *testing.T) {
			m := mustGenerate(t, in)
			objs, err := Render(m, linuxClass())
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			checkGolden(t, name+".json", renderJSON(t, objs))
		})
	}
}

func TestRenderIsPure(t *testing.T) {
	m := mustGenerate(t, renderCases()["one-shot"])
	a, err := Render(m, linuxClass())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		b, err := Render(m, linuxClass())
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(renderJSON(t, a), renderJSON(t, b)) {
			t.Fatal("Render is not deterministic")
		}
	}
}

func dig(t *testing.T, v any, path ...string) any {
	t.Helper()
	for _, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("at %q: %T is not an object", p, v)
		}
		v = m[p]
	}
	return v
}

func TestRenderOneShotIsJobWithoutRetry(t *testing.T) {
	in := renderCases()["one-shot"]
	m := mustGenerate(t, in)
	objs, err := Render(m, linuxClass())
	if err != nil {
		t.Fatal(err)
	}
	var job Object
	for _, o := range objs {
		if o["kind"] == "Job" {
			job = o
		}
	}
	if job == nil || job["apiVersion"] != "batch/v1" {
		t.Fatalf("no batch/v1 Job in %v", objs)
	}
	if dig(t, job, "spec", "backoffLimit") != int64(0) {
		t.Fatalf("backoffLimit = %v", dig(t, job, "spec", "backoffLimit"))
	}
	if dig(t, job, "spec", "template", "spec", "restartPolicy") != "Never" {
		t.Fatal("restartPolicy is not Never")
	}
	labels := dig(t, job, "metadata", "labels").(map[string]any)
	for k, v := range map[string]string{
		"app.kubernetes.io/managed-by": "bashy",
		"dhnt.io/workspace":            in.WorkspaceID,
		"dhnt.io/run":                  in.RunID,
		"dhnt.io/command":              "render",
	} {
		if labels[k] != v {
			t.Errorf("label %s = %v, want %s", k, labels[k], v)
		}
	}
	if got := dig(t, job, "metadata", "annotations").(map[string]any)["dhnt.io/manifest"]; got != string(m) {
		t.Fatalf("manifest annotation is not the manifest text")
	}
}

func TestRenderCommandLabelIsLabelSafe(t *testing.T) {
	m := mustGenerate(t, renderCases()["long-running"])
	objs, err := Render(m, linuxClass())
	if err != nil {
		t.Fatal(err)
	}
	got := dig(t, objs[0], "metadata", "labels").(map[string]any)["dhnt.io/command"].(string)
	if !labelValueRE.MatchString(got) {
		t.Fatalf("command label %q is not label-safe", got)
	}
}

func TestRenderUnsupportedExecutors(t *testing.T) {
	m := mustGenerate(t, renderCases()["one-shot"])
	for _, c := range []ExecutorClass{
		{OS: "darwin", Image: testImage},
		{OS: "windows", Image: testImage},
		{OS: "plan9", Image: testImage},
	} {
		if _, err := Render(m, c); !errors.Is(err, ErrUnsupportedExecutor) {
			t.Errorf("Render(%s) err = %v, want ErrUnsupportedExecutor", c.OS, err)
		}
	}
}

func TestRenderFailsClosed(t *testing.T) {
	unsupported := map[string]func(*GenerateInput){
		"service":            func(in *GenerateInput) { in.Run.Kind = RunService },
		"egress plane":       func(in *GenerateInput) { in.Run.Egress.Mode = EgressPlane },
		"native venue":       func(in *GenerateInput) { in.Venue = "native" },
		"native backend":     func(in *GenerateInput) { in.Match["outpost.dhnt.io/backend"] = "vk-native" },
		"non-cuda gpu":       func(in *GenerateInput) { in.Run.Accelerator = Accelerator{Kind: "gpu", Family: "metal", Count: 1} },
		"darwin-only pinned": func(in *GenerateInput) { in.Match["kubernetes.io/os"] = "darwin" },
	}
	for name, mutate := range unsupported {
		in := renderCases()["one-shot"]
		mutate(&in)
		m := mustGenerate(t, in)
		if _, err := Render(m, linuxClass()); !errors.Is(err, ErrUnsupportedExecutor) {
			t.Errorf("%s: err = %v, want ErrUnsupportedExecutor", name, err)
		}
	}

	invalidCases := map[string]func(*GenerateInput){
		"userland venue": func(in *GenerateInput) { in.Venue = "userland" },
		"no linux artifact": func(in *GenerateInput) {
			in.Match = nil
			in.Command.Artifacts = map[string]string{"darwin/arm64": testDigestMac}
		},
		"bare match key":      func(in *GenerateInput) { in.Match["libc"] = "gnu" },
		"relative cwd":        func(in *GenerateInput) { in.Cwd = "docs" },
		"cwd escapes to root": func(in *GenerateInput) { in.Cwd = "~/../../etc" },
	}
	for name, mutate := range invalidCases {
		in := renderCases()["one-shot"]
		mutate(&in)
		m := mustGenerate(t, in)
		if _, err := Render(m, linuxClass()); !errors.Is(err, ErrInvalidManifest) {
			t.Errorf("%s: err = %v, want ErrInvalidManifest", name, err)
		}
	}

	m := mustGenerate(t, renderCases()["one-shot"])
	if _, err := Render(m, ExecutorClass{OS: "linux"}); err == nil {
		t.Error("Render with no image succeeded")
	}
	if _, err := Render([]byte("## run\n```sh\necho hi\n```\n"), linuxClass()); !errors.Is(err, ErrInvalidManifest) {
		t.Errorf("Render of a non-manifest err = %v, want ErrInvalidManifest", err)
	}
}

// TestRenderLinuxContainerNeverOnNativeBackend: the native-process backend
// ignores the image, so a Linux container workload must require a node
// whose backend is not vk-native (unlabelled kubelets stay eligible) — on
// every workload kind, alongside the os selector and the host pin.
func TestRenderLinuxContainerNeverOnNativeBackend(t *testing.T) {
	for name, kind := range map[string]string{"one-shot": "Job", "interactive": "Pod", "long-running": "Pod"} {
		t.Run(name, func(t *testing.T) {
			in := renderCases()[name]
			objs, err := Render(mustGenerate(t, in), linuxClass())
			if err != nil {
				t.Fatal(err)
			}
			w := objs[0]
			if w["kind"] != kind {
				t.Fatalf("kind = %v, want %s", w["kind"], kind)
			}
			spec := dig(t, w, "spec").(map[string]any)
			if kind == "Job" {
				spec = dig(t, spec, "template", "spec").(map[string]any)
			}
			if sel := spec["nodeSelector"].(map[string]any); sel["kubernetes.io/os"] != "linux" {
				t.Fatalf("nodeSelector lost os=linux: %v", sel)
			} else if host := in.Match["outpost.dhnt.io/host"]; host != "" && sel["outpost.dhnt.io/host"] != host {
				t.Fatalf("nodeSelector lost the host pin: %v", sel)
			}
			terms, _ := dig(t, spec, "affinity", "nodeAffinity",
				"requiredDuringSchedulingIgnoredDuringExecution", "nodeSelectorTerms").([]any)
			if len(terms) == 0 {
				t.Fatal("no required node affinity")
			}
			for i, term := range terms {
				found := false
				for _, e := range dig(t, term, "matchExpressions").([]any) {
					e := e.(map[string]any)
					if e["key"] == "outpost.dhnt.io/backend" && e["operator"] == "NotIn" {
						vals := e["values"].([]any)
						found = len(vals) == 1 && vals[0] == "vk-native"
					}
				}
				if !found {
					t.Errorf("term %d lacks backend NotIn [vk-native]: %v", i, term)
				}
			}
		})
	}
}

func TestRenderCredentialSecretKeyRef(t *testing.T) {
	in := renderCases()["one-shot"]
	in.Run.Credentials = []string{"anthropic-subscription=CLAUDE_CODE_OAUTH_TOKEN", "anthropic"}
	m := mustGenerate(t, in)
	objs, err := Render(m, linuxClass())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(objs) == 0 {
		t.Fatal("Render returned no objects")
	}
	spec := dig(t, objs[0], "spec", "template", "spec").(map[string]any)
	containers := spec["containers"].([]any)
	container := containers[0].(map[string]any)
	envList := container["env"].([]any)

	wantSecretName := "bashy-cred-" + strings.ToLower(in.RunID)
	checked := make(map[string]bool)
	for _, e := range envList {
		entry := e.(map[string]any)
		name, _ := entry["name"].(string)
		if name == "CLAUDE_CODE_OAUTH_TOKEN" || name == "ANTHROPIC_API_KEY" {
			vf, ok := entry["valueFrom"].(map[string]any)
			if !ok {
				t.Fatalf("%s missing valueFrom: %v", name, entry)
			}
			skr, ok := vf["secretKeyRef"].(map[string]any)
			if !ok {
				t.Fatalf("%s missing secretKeyRef: %v", name, vf)
			}
			if skr["name"] != wantSecretName {
				t.Errorf("%s secretKeyRef name = %v, want %s", name, skr["name"], wantSecretName)
			}
			if skr["key"] != name {
				t.Errorf("%s secretKeyRef key = %v, want %s", name, skr["key"], name)
			}
			checked[name] = true
		}
	}
	if !checked["CLAUDE_CODE_OAUTH_TOKEN"] || !checked["ANTHROPIC_API_KEY"] {
		t.Errorf("expected both credentials in container env, got %v", checked)
	}
}

// A LimitRange default limit below the request makes every Pod invalid, so
// a cpu/memory request always carries an equal limit.
func TestRenderSetsLimitsEqualToRequests(t *testing.T) {
	in := renderCases()["interactive"]
	objs, err := Render(mustGenerate(t, in), linuxClass())
	if err != nil {
		t.Fatal(err)
	}
	var res map[string]any
	for _, o := range objs {
		if o["kind"] == "Pod" {
			c := dig(t, o, "spec", "containers").([]any)[0]
			res = dig(t, c, "resources").(map[string]any)
		}
	}
	if res == nil {
		t.Fatalf("no Pod resources in %v", objs)
	}
	requests, _ := res["requests"].(map[string]any)
	limits, _ := res["limits"].(map[string]any)
	for _, k := range []string{"cpu", "memory"} {
		if requests[k] == nil || limits[k] != requests[k] {
			t.Errorf("%s: request %v, limit %v; want an equal limit", k, requests[k], limits[k])
		}
	}
}
