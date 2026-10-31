package enroll_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/provider"
	"github.com/qiangli/yoke/pkg/provider/enroll"
)

const masterScript = "#!/bin/bash\nset -e\necho enroll sat-1 single-use-token\n__BASHY_MASTER_SCRIPT__\nprintf 'tail without newline'"

func TestGPUWorkerUserDataHonoursModelDir(t *testing.T) {
	ud, err := enroll.GPUWorkerUserData([]byte(masterScript), "/var/lib/ollama")
	if err != nil {
		t.Fatal(err)
	}
	s := string(ud)
	for _, want := range []string{
		"model_dir='/var/lib/ollama'\n",
		"mkdir -p \"$model_dir\"\n",
		"DefaultEnvironment=OLLAMA_MODELS=%s",
		"systemctl set-environment OLLAMA_MODELS=\"$model_dir\"",
		"export OLLAMA_MODELS=\"$model_dir\"\n",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("user data lacks %q:\n%s", want, s)
		}
	}
	if !strings.HasPrefix(s, "#!/bin/sh\n") {
		t.Fatal("prologue is not a POSIX sh script")
	}
	if got := enroll.MasterScript(ud); string(got) != masterScript {
		t.Fatalf("master script not byte-identical: %q", got)
	}
	if got := enroll.MasterScript([]byte(masterScript)); string(got) != masterScript {
		t.Fatal("MasterScript changed data without a prologue")
	}
}

// The prologue's own extraction (sed over "$0") yields the master's script
// byte-identical, including a marker-looking line inside it.
func TestGPUWorkerPrologueExtractsMasterScriptByteIdentical(t *testing.T) {
	if _, err := exec.LookPath("sed"); err != nil {
		t.Skip("no sed")
	}
	ud, err := enroll.GPUWorkerUserData([]byte(masterScript), "/var/lib/ollama")
	if err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(t.TempDir(), "part-001")
	if err := os.WriteFile(f, ud, 0o700); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("sed", "1,/^__BASHY_MASTER_SCRIPT__$/d", f).Output()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.TrimSuffix(out, []byte("\n")), []byte(masterScript)) {
		t.Fatalf("extracted %q", out)
	}
}

func TestGPUWorkerUserDataRejectsUnsafeModelDir(t *testing.T) {
	for _, dir := range []string{"", "var/lib/ollama", "/", "/var/lib/../ollama", "/var/lib/ollama/", "/var/lib/o'llama", "/var/lib/$(id)"} {
		if _, err := enroll.GPUWorkerUserData([]byte(masterScript), dir); enroll.KindOf(err) != enroll.KindEnrollFailed {
			t.Errorf("model_dir %q: err = %v", dir, err)
		}
	}
	if _, err := enroll.GPUWorkerUserData(nil, "/var/lib/ollama"); enroll.KindOf(err) != enroll.KindBundleIncomplete {
		t.Fatalf("empty master script: %v", err)
	}
}

func bundleMaster(t *testing.T, hits *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*hits++
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"satellite_id": "sat-1", "enrollment_token": "tok", "master_public_key": "ed25519:k",
			"master_url": "https://master.example", "artifact_url": "https://master.example/a",
			"user_data": masterScript,
		})
	}))
}

// The executable path for a GPU worker creates the host with user data that
// honours the plan's model_dir, carrying the master's script unchanged.
func TestProvisionGPUWorkerUserDataCarriesModelDir(t *testing.T) {
	hits := 0
	master := bundleMaster(t, &hits)
	defer master.Close()
	fixture := &countingAdapter{manifest: fixtureManifest()}
	d := openDispatcher(t, fixture, "fixture-credential")
	_, err := enroll.ProvisionExecutable(context.Background(), enroll.Client{BaseURL: master.URL, Session: "s"}, d,
		enroll.ProvisionRequest{
			SatelliteID: "sat-1", Endpoint: "sat-1.example.com",
			Spec:           provider.Spec{Name: "sat", Region: "r1", Size: "s-1", Image: "img"},
			IdempotencyKey: "gpu-1", ModelDir: "/var/lib/ollama",
		})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(fixture.userData), "export OLLAMA_MODELS=\"$model_dir\"") ||
		string(enroll.MasterScript(fixture.userData)) != masterScript {
		t.Fatalf("created with user data %q", fixture.userData)
	}

	// A bad model_dir is refused before the master or the provider is asked.
	hits = 0
	fixture2 := &countingAdapter{manifest: fixtureManifest()}
	d2 := openDispatcher(t, fixture2, "fixture-credential")
	_, err = enroll.ProvisionExecutable(context.Background(), enroll.Client{BaseURL: master.URL, Session: "s"}, d2,
		enroll.ProvisionRequest{SatelliteID: "sat-1", Endpoint: "e", IdempotencyKey: "gpu-2", ModelDir: "relative",
			Spec: provider.Spec{Name: "sat", Region: "r1", Size: "s-1", Image: "img"}})
	if enroll.KindOf(err) != enroll.KindEnrollFailed || hits != 0 {
		t.Fatalf("err = %v, master hits = %d", err, hits)
	}
	if calls, _, _ := fixture2.counts(); calls != 0 {
		t.Fatalf("provider called %d times", calls)
	}
}
