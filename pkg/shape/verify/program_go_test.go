package verify

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestProgramNativeGoLayouts(t *testing.T) {
	for _, argv := range [][]string{
		{`C:\Go\bin\go.exe`, "test", "-json", "./..."},
		{`C:\bin\bashy.exe`, "go", "test", "-json", "./..."},
		{"bash", `scripts\go-product.sh`, "test", "-json", "./..."},
		{"sh", "./scripts/go-product.sh", "test", "-json", "./..."},
	} {
		if _, native, err := nativeGoTest(argv); err != nil || !native {
			t.Fatalf("%q: native=%v err=%v", argv, native, err)
		}
	}
	for _, argv := range [][]string{
		{"go", "build"}, {"go", "test", "./..."},
		{"bashy.exe", "go", "test", "-json", "-exec=echo"},
		{"go", "test", "-json", "-overlay=x"},
		{"go", "test", "-json", "-list=Test"},
	} {
		if _, native, err := nativeGoTest(argv); !native || err == nil {
			t.Fatalf("accepted unsupported %q: native=%v err=%v", argv, native, err)
		}
	}
}

func TestProgramGoAccountingRequiresBaselineCompletion(t *testing.T) {
	expected := map[string]bool{"fixture\x00TestBaseline": true}
	for _, action := range []string{"", "skip", "fail"} {
		output := ""
		if action != "" {
			output = `{"Action":"` + action + `","Package":"fixture","Test":"TestBaseline"}`
		}
		if _, err := accountProgramGoTests(output, expected); err == nil {
			t.Fatalf("accepted %q", action)
		}
	}
	if _, err := accountProgramGoTests(`{"Action":"pass","Package":"fixture","Test":"TestBaseline"}`, expected); err != nil {
		t.Fatal(err)
	}
}

func TestProgramGoAccountingRejectsProductionInitExitZero(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"go.mod":          "module shape-accounting-fixture\n\ngo 1.20\n",
		"fixture.go":      "package fixture\n",
		"fixture_test.go": "package fixture\nimport \"testing\"\nfunc TestBaseline(t *testing.T) {}\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	run := func() string {
		t.Helper()
		cmd := exec.Command("go", "test", "-json", "-count=1", ".")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=")
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("go test: %v\n%s", err, output)
		}
		return string(output)
	}
	expected := map[string]bool{"shape-accounting-fixture\x00TestBaseline": true}
	if _, err := accountProgramGoTests(run(), expected); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	// Preserve the baseline test exactly; only production initialization changes.
	if err := os.WriteFile(filepath.Join(dir, "fixture.go"), []byte("package fixture\nimport \"os\"\nfunc init() { os.Exit(0) }\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := accountProgramGoTests(run(), expected); err == nil {
		t.Fatal("exit-zero process without baseline test completion was accepted")
	}
}
