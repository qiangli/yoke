package feed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
)

const doctorSecretValue = "s3cr3t-NEVER-ECHO-9f2c"

// doctorFixture is a fully configured feed home plus probes that all
// succeed; each test breaks exactly what it is about.
type doctorFixture struct {
	dir   string
	env   Env
	token string
}

func newDoctorFixture(t *testing.T) *doctorFixture {
	t.Helper()
	f := &doctorFixture{dir: t.TempDir(), token: "paired"}
	f.env = Env{
		Dir:   func() string { return f.dir },
		Cloud: func() (string, string) { return "https://registry.invalid", f.token },
		RcloneRun: func(_ context.Context, args ...string) ([]byte, error) {
			if len(args) == 1 && args[0] == "listremotes" {
				return []byte("r2:\n"), nil
			}
			return nil, errors.New("unexpected rclone call")
		},
		doctor: doctorProbes{
			lookPath:    func(string) (string, error) { return "/fake/openclaw", nil },
			nodeVersion: func(context.Context) (string, error) { return "v24.16.0", nil },
			gatewayUp:   func(context.Context, string) error { return nil },
			gitResolve:  func(context.Context, string, string) error { return nil },
			lookupCommand: func(ref string) (fleet.Tool, fleet.ToolCommand, error) {
				tool, _, _ := strings.Cut(ref, ":")
				return fleet.Tool{Name: tool}, fleet.ToolCommand{Name: "send", Mode: "http",
					HTTP: &fleet.ToolCommandHTTPAction{Method: "POST", URL: "http://127.0.0.1:18789", TokenRef: "openclaw"}}, nil
			},
			vaultNames: func() map[string]struct{} { return map[string]struct{}{"OPENCLAW_TOKEN": {}} },
			lookupEnv:  func(string) (string, bool) { return "", false },
		},
	}
	if err := os.WriteFile(filepath.Join(f.dir, keyFileName), bytes.Repeat([]byte{7}, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SaveSinks(f.dir, []SinkConfig{
		{Name: "gh", Kind: "github", Target: "https://github.com/example/feed.git"},
		{Name: "bucket", Kind: "rclone", Target: "r2:feed"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := SaveChannels(f.dir, []Channel{{Name: "phone", Cmd: "openclaw:send", Args: "--channel signal"}}); err != nil {
		t.Fatal(err)
	}
	return f
}

func runDoctorCmd(t *testing.T, env Env, args ...string) (string, error) {
	t.Helper()
	cmd := NewCmdWith(env)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(append([]string{"doctor"}, args...))
	err := cmd.Execute()
	return out.String(), err
}

func statusOf(rep DoctorReport, area string) []string {
	var s []string
	for _, c := range rep.Checks {
		if c.Area == area {
			s = append(s, c.Status)
		}
	}
	return s
}

func dirSnapshot(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil {
			out = append(out, p+"|"+info.ModTime().String()+"|"+info.Mode().String())
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func TestDoctorAllConfigured(t *testing.T) {
	f := newDoctorFixture(t)
	before := dirSnapshot(t, f.dir)
	out, err := runDoctorCmd(t, f.env, "--json")
	if err != nil {
		t.Fatalf("doctor: %v\n%s", err, out)
	}
	var rep DoctorReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if rep.SchemaVersion != DoctorSchema || !rep.OK {
		t.Fatalf("schema=%q ok=%v\n%s", rep.SchemaVersion, rep.OK, out)
	}
	for _, area := range []string{"pairing", "signing-key", "sinks", "channels", "secrets"} {
		st := statusOf(rep, area)
		if len(st) == 0 {
			t.Errorf("area %s not reported", area)
		}
		for _, s := range st {
			if s != DoctorConfigured {
				t.Errorf("area %s: %s\n%s", area, s, out)
			}
		}
	}
	if got := len(statusOf(rep, "sinks")); got != 2 {
		t.Errorf("want one check per sink, got %d", got)
	}
	if after := dirSnapshot(t, f.dir); strings.Join(before, "\n") != strings.Join(after, "\n") {
		t.Errorf("doctor mutated the feed dir:\nbefore %v\nafter  %v", before, after)
	}
	// Idempotent: a second run reports the same thing.
	out2, err := runDoctorCmd(t, f.env, "--json")
	if err != nil || out2 != out {
		t.Errorf("second run differs (err=%v)", err)
	}
}

func TestDoctorAllMissing(t *testing.T) {
	dir := t.TempDir()
	env := Env{
		Dir:   func() string { return dir },
		Cloud: func() (string, string) { return "", "" },
		doctor: doctorProbes{
			vaultNames: func() map[string]struct{} { return nil },
			lookupEnv:  func(string) (string, bool) { return "", false },
		},
	}
	out, err := runDoctorCmd(t, env)
	if !errors.Is(err, errDoctorMissing) {
		t.Fatalf("want errDoctorMissing, got %v\n%s", err, out)
	}
	for _, want := range []string{
		"missing    pairing", "next: bashy login",
		"missing    signing-key", "next: bashy feed post FILE",
		"missing    sinks", "next: bashy feed sink add github",
		"missing    channels", "next: bashy feed channel add",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.HasPrefix(out, DoctorConfigured) || strings.Contains(out, "\n"+DoctorConfigured) {
		t.Errorf("nothing should be configured:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, keyFileName)); !os.IsNotExist(err) {
		t.Errorf("doctor must not create the signing key (stat err %v)", err)
	}
}

func TestDoctorSecretPresentNeverEchoed(t *testing.T) {
	f := newDoctorFixture(t)
	f.env.doctor.vaultNames = func() map[string]struct{} { return nil }
	f.env.doctor.lookupEnv = func(name string) (string, bool) {
		if name == "OPENCLAW_TOKEN" {
			return doctorSecretValue, true
		}
		return "", false
	}
	for _, args := range [][]string{nil, {"--json"}} {
		out, err := runDoctorCmd(t, f.env, args...)
		if err != nil {
			t.Fatalf("doctor %v: %v\n%s", args, err, out)
		}
		if strings.Contains(out, doctorSecretValue) {
			t.Fatalf("doctor %v echoed the secret value:\n%s", args, out)
		}
		if !strings.Contains(out, "OPENCLAW_TOKEN") {
			t.Errorf("doctor %v should name the bound secret:\n%s", args, out)
		}
	}
}

func TestDoctorSecretMissingNamesRefOnly(t *testing.T) {
	f := newDoctorFixture(t)
	f.env.doctor.vaultNames = func() map[string]struct{} { return nil }
	rep := RunDoctor(context.Background(), f.env)
	if rep.OK {
		t.Fatal("want not OK")
	}
	for _, c := range rep.Checks {
		if c.Area == "secrets" {
			if c.Status != DoctorMissing || c.Next != "bashy secret set OPENCLAW_API_KEY" {
				t.Errorf("secrets check = %+v", c)
			}
			return
		}
	}
	t.Fatal("no secrets check")
}

func TestDoctorOpenClaw(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*doctorProbes)
		wantNext string
	}{
		{"binary missing", func(p *doctorProbes) {
			p.lookPath = func(string) (string, error) { return "", errors.New("not found") }
		}, "npm install -g openclaw@latest"},
		{"node missing", func(p *doctorProbes) {
			p.nodeVersion = func(context.Context) (string, error) { return "", errors.New("not found") }
		}, "bashy mise use -g node@24"},
		{"node too old", func(p *doctorProbes) {
			p.nodeVersion = func(context.Context) (string, error) { return "v24.15.9", nil }
		}, "bashy mise use -g node@24"},
		{"gateway down", func(p *doctorProbes) {
			p.gatewayUp = func(context.Context, string) error { return errors.New("connection refused") }
		}, "openclaw gateway start"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDoctorFixture(t)
			tc.mutate(&f.env.doctor)
			rep := RunDoctor(context.Background(), f.env)
			if rep.OK {
				t.Fatal("want not OK")
			}
			for _, c := range rep.Checks {
				if c.Area != "channels" {
					continue
				}
				if c.Status != DoctorMissing || c.Next != tc.wantNext {
					t.Errorf("channel check = %+v, want next %q", c, tc.wantNext)
				}
				return
			}
			t.Fatal("no channel check")
		})
	}
}

func TestDoctorSinkFailures(t *testing.T) {
	f := newDoctorFixture(t)
	f.env.doctor.gitResolve = func(context.Context, string, string) error { return errors.New("not found") }
	f.env.RcloneRun = func(context.Context, ...string) ([]byte, error) { return []byte("other:\n"), nil }
	rep := RunDoctor(context.Background(), f.env)
	got := map[string]string{}
	for _, c := range rep.Checks {
		if c.Area == "sinks" {
			if c.Status != DoctorMissing {
				t.Errorf("sink %s: %s", c.Name, c.Status)
			}
			got[c.Name] = c.Next
		}
	}
	if got["gh"] != "git ls-remote https://github.com/example/feed.git" || got["bucket"] != "bashy rclone config" {
		t.Errorf("sink nexts = %v", got)
	}
}

func TestNodeAtLeast(t *testing.T) {
	for v, want := range map[string]bool{
		"v24.16.0": true, "v24.16.1": true, "v25.0.0": true, "v24.17.0-rc.1": true,
		"v24.15.9": false, "v22.23.2": false, "": false, "garbage": false,
	} {
		if got := nodeAtLeast(v, openclawMinNode); got != want {
			t.Errorf("nodeAtLeast(%q) = %v, want %v", v, got, want)
		}
	}
}
