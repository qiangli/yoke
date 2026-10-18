package adapt

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
)

func curated(t *testing.T) []fleet.Adapter {
	t.Helper()
	t.Setenv("BASHY_ADAPTERS_DIR", t.TempDir())
	t.Setenv("BASHY_ADAPTERS_PATH", "")
	rows, errs := fleet.New(fleet.WithoutCloudOverlay()).Adapters()
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	return rows
}
func TestCuratedMappings(t *testing.T) {
	rows := curated(t)
	cases := []struct {
		argv   []string
		option string
		want   []string
		refuse bool
	}{
		{[]string{"git", "status", "a b"}, "null", []string{"git", "status", "a b", "--porcelain=v1", "-z"}, false},
		{[]string{"git", "clean", "-fd"}, "dry-run", []string{"git", "clean", "-fd", "--dry-run"}, false},
		{[]string{"docker", "ps", "--all"}, "json", []string{"docker", "ps", "--all", "--format=json"}, false},
		{[]string{"kubectl", "get", "pods"}, "json", []string{"kubectl", "get", "pods", "--output=json"}, false},
		{[]string{"kubectl", "apply", "-f", "x"}, "dry-run", []string{"kubectl", "apply", "-f", "x", "--dry-run=server"}, false},
		{[]string{"rsync", "src/", "dst/"}, "dry-run", []string{"rsync", "src/", "dst/", "--dry-run"}, false},
		{[]string{"apt", "install", "jq"}, "dry-run", []string{"apt", "install", "jq", "--simulate"}, false},
		{[]string{"tar", "-xf", "x"}, "dry-run", nil, true},
		{[]string{"unzip", "x"}, "json", nil, true},
		{[]string{"make", "target"}, "dry-run", nil, true},
		{[]string{"rsync", "src/", "dst/"}, "null", nil, true},
		{[]string{"apt", "install", "jq"}, "no-input", nil, true},
		{[]string{"git", "clean", "--", "file"}, "dry-run", nil, true},
		{[]string{"git", "-C", "repo", "clean"}, "dry-run", nil, true},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.argv, " ")+tc.option, func(t *testing.T) {
			p, e := Build(rows, tc.argv, Request{Options: map[string]string{tc.option: "true"}})
			if tc.refuse {
				if e == nil {
					t.Fatal("must refuse")
				}
				return
			}
			if e != nil || !reflect.DeepEqual(p.Argv, tc.want) {
				t.Fatalf("%v %v", p.Argv, e)
			}
		})
	}
	for _, name := range []string{"tar", "unzip"} {
		p, e := Build(rows, []string{name, "archive with spaces"}, Request{Listing: true})
		if e != nil || p.Argv[len(p.Argv)-1] != "archive with spaces" {
			t.Fatalf("listing: %v %v", p, e)
		}
	}
}

func TestHelperProcess(t *testing.T) {
	if os.Getenv("ADAPT_TEST_CHILD") != "1" {
		return
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	if len(args) > 0 && args[0] == "success" {
		fmt.Fprint(os.Stdout, "native output")
		os.Exit(0)
	}
	if len(args) > 0 && args[0] == "marker" {
		_ = os.WriteFile(args[1], []byte("executed"), 0600)
		os.Exit(0)
	}
	input, _ := io.ReadAll(os.Stdin)
	_ = json.NewEncoder(os.Stdout).Encode(args)
	fmt.Fprint(os.Stdout, string(input))
	fmt.Fprint(os.Stderr, "child stderr")
	os.Exit(37)
}
func helper(t *testing.T) ([]string, *fleet.Catalog) {
	t.Helper()
	t.Setenv("ADAPT_TEST_CHILD", "1")
	t.Setenv("BASHY_ADAPTERS_DIR", t.TempDir())
	t.Setenv("BASHY_ADAPTERS_PATH", "")
	argv := []string{os.Args[0], "-test.run=TestHelperProcess", "--"}
	cat := fleet.New(fleet.WithoutCloudOverlay())
	if e := cat.SaveAdapter(fleet.Adapter{Name: "fixture", Kind: "adapter", Match: argv, PreviewLevel: "unsupported"}); e != nil {
		t.Fatal(e)
	}
	return argv, cat
}
func TestCLITransparentExitArgvAndInput(t *testing.T) {
	argv, _ := helper(t)
	tail := []string{"a b", "", "$(touch nope)", "--dry_run", "--json=false"}
	argv = append(argv, tail...)
	var out, errOut bytes.Buffer
	cmd := NewCmd(nil, fleet.WithoutCloudOverlay())
	cmd.SetArgs(append([]string{"--"}, argv...))
	cmd.SetIn(strings.NewReader("payload"))
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	if code := ExitCode(cmd.Execute()); code != 37 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	expected, _ := json.Marshal(tail)
	if out.String() != string(expected)+"\npayload" || errOut.String() != "child stderr" {
		t.Fatalf("out=%q err=%q", out.String(), errOut.String())
	}
}
func TestCLIRefusalsAndExplainNeverExecute(t *testing.T) {
	argv, cat := helper(t)
	marker := filepath.Join(t.TempDir(), "ran")
	argv = append(argv, "marker", marker)
	for _, flags := range [][]string{{"--json"}, {"--dry_run"}, {"--since=secret"}, {"--unknown=secret"}, {"--explain"}, {"--fields=secret"}} {
		var out, errOut bytes.Buffer
		c := NewCmd(nil, fleet.WithoutCloudOverlay())
		c.SetArgs(append(append(flags, "--"), argv...))
		c.SetOut(&out)
		c.SetErr(&errOut)
		want := 125
		if flags[0] == "--explain" {
			want = 0
		}
		if code := ExitCode(c.Execute()); code != want {
			t.Fatalf("%v: %d %s", flags, code, errOut.String())
		}
		if _, e := os.Stat(marker); !os.IsNotExist(e) {
			t.Fatal("target executed")
		}
		if strings.Contains(out.String()+errOut.String(), "secret") || strings.Contains(out.String(), marker) {
			t.Fatal("diagnostic leaked argv")
		}
	}
	a, _ := cat.Adapter("fixture")
	a.JSON = []string{"lens: cmd:fixture"}
	if e := cat.SaveAdapter(a); e != nil {
		t.Fatal(e)
	}
	c := NewCmd(nil, fleet.WithoutCloudOverlay())
	c.SetArgs(append([]string{"--json", "--"}, argv...))
	c.SetOut(io.Discard)
	c.SetErr(io.Discard)
	if code := ExitCode(c.Execute()); code != 125 {
		t.Fatal(code)
	}
	if _, e := os.Stat(marker); !os.IsNotExist(e) {
		t.Fatal("lens refusal executed target")
	}
}
func TestNativePreferredAndLensPreflight(t *testing.T) {
	rows := curated(t)
	p, e := Build(rows, []string{"docker", "ps"}, Request{Options: map[string]string{"json": "true"}})
	if e != nil || p.Lens != "" {
		t.Fatal(p, e)
	}
	p = Plan{Argv: []string{"must-not-exist"}, Lens: "cmd:fixture"}
	called := false
	code := Run(context.Background(), p, nil, io.Discard, io.Discard, func(context.Context, string, io.Writer) (io.WriteCloser, error) {
		called = true
		return nil, fmt.Errorf("unavailable")
	})
	if !called || code != 125 {
		t.Fatal(called, code)
	}
}

func TestNativeGitDryRunAndNUL(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git unavailable")
	}
	rows := curated(t)
	dir := t.TempDir()
	init := exec.Command(git, "init", "-q", dir)
	if b, e := init.CombinedOutput(); e != nil {
		t.Fatalf("git init: %s %v", b, e)
	}
	if e := os.WriteFile(filepath.Join(dir, "space name"), []byte("keep"), 0600); e != nil {
		t.Fatal(e)
	}
	t.Chdir(dir)
	for _, tc := range []struct {
		args   []string
		option string
	}{{[]string{"git", "clean", "-fd"}, "dry-run"}, {[]string{"git", "status"}, "null"}} {
		p, e := Build(rows, tc.args, Request{Options: map[string]string{tc.option: "true"}})
		if e != nil {
			t.Fatal(e)
		}
		p.Argv[0] = git
		var out, errOut bytes.Buffer
		if code := Run(context.Background(), p, nil, &out, &errOut, nil); code != 0 {
			t.Fatalf("status %d: %s", code, errOut.String())
		}
		if tc.option == "null" && !bytes.Contains(out.Bytes(), []byte{0}) {
			t.Fatal("missing native NUL")
		}
		if _, e := os.Stat("space name"); e != nil {
			t.Fatal("dry-run removed file")
		}
	}
}
func TestNoInputKeepsPayloadAndEmptyMappings(t *testing.T) {
	argv, cat := helper(t)
	a, _ := cat.Adapter("fixture")
	a.NoInput = []string{}
	a.JSON = []string{}
	if e := cat.SaveAdapter(a); e != nil {
		t.Fatal(e)
	}
	var out bytes.Buffer
	c := NewCmd(nil, fleet.WithoutCloudOverlay())
	c.SetArgs(append([]string{"--no_input", "--json", "--"}, argv...))
	c.SetIn(strings.NewReader("explicit payload"))
	c.SetOut(&out)
	c.SetErr(io.Discard)
	if code := ExitCode(c.Execute()); code != 37 || !strings.Contains(out.String(), "explicit payload") {
		t.Fatalf("%d %q", code, out.String())
	}
}
func TestLongestPrefixAndAmbiguity(t *testing.T) {
	a := fleet.Adapter{Kind: "adapter", Name: "base", Match: []string{"tool"}, PreviewLevel: "unsupported"}
	b := a
	b.Name = "base2"
	c := a
	c.Name = "specific"
	c.Match = []string{"tool", "list"}
	p, e := Build([]fleet.Adapter{a, b, c}, []string{"tool", "list", ""}, Request{})
	if e != nil || p.Adapter.Name != "specific" {
		t.Fatal(p, e)
	}
	if _, e := Build([]fleet.Adapter{a, b}, []string{"tool"}, Request{}); e == nil {
		t.Fatal("ambiguous prefix accepted")
	}
}

type failingWriter struct{}

func (failingWriter) Write(p []byte) (int, error) { return 0, fmt.Errorf("output unavailable") }
func TestNativeStatusSurvivesForwardingFailure(t *testing.T) {
	argv, _ := helper(t)
	code := Run(context.Background(), Plan{Argv: append(argv, "success")}, nil, failingWriter{}, io.Discard, nil)
	if code != 0 {
		t.Fatalf("forwarding error replaced native success with %d", code)
	}
}
