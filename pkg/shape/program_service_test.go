package shape

import (
	"context"
	"errors"
	"github.com/qiangli/yoke/pkg/fleet"
	shapeverify "github.com/qiangli/yoke/pkg/shape/verify"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type programServices struct {
	units                          map[string]fleet.Service
	failHealth, failStop, failLive bool
	probes                         int
}

func (r *programServices) Load(_ context.Context, n string) (fleet.Service, error) {
	u, ok := r.units[n]
	if !ok {
		return u, os.ErrNotExist
	}
	return cloneService(u), nil
}
func (r *programServices) Save(_ context.Context, u fleet.Service) error {
	r.units[u.Name] = cloneService(u)
	return nil
}
func (r *programServices) Restart(context.Context, string) error { return nil }
func (r *programServices) Healthy(_ context.Context, u fleet.Service) error {
	r.probes++
	if r.failHealth || (r.failLive && u.Name == "disposable" && strings.HasSuffix(u.Settings["image"], strings.Repeat("b", 64))) {
		return errors.New("unhealthy")
	}
	return os.WriteFile(filepath.Join(u.Settings["data_dir"], "probe"), []byte("private write"), 0600)
}
func (r *programServices) Stop(context.Context, string) error {
	if r.failStop {
		return errors.New("stop failed")
	}
	return nil
}
func (r *programServices) Remove(_ context.Context, n string) error { delete(r.units, n); return nil }
func serviceFixture(t *testing.T) (Service, Caller, string, *programServices) {
	t.Helper()
	s, c, input := programFixture(t)
	s.App.Shaping.Kind = "service"
	s.App.Shaping.Service = "disposable"
	data := t.TempDir()
	os.WriteFile(filepath.Join(data, "original"), []byte("keep"), 0600)
	u := fleet.Service{Name: "disposable", Exec: []string{"podman", "run", "--rm", "--name", "disposable", "-p", "127.0.0.1:{port}:8080", "-v", "{settings.data_dir}:/data", "{settings.image}"}, Settings: map[string]string{"image": "sha256:" + strings.Repeat("a", 64), "data_dir": data}, Ports: []fleet.ServicePort{{Port: 18765}}, Health: fleet.ServiceHealth{TCP: true}, Enabled: true}
	r := &programServices{units: map[string]fleet.Service{u.Name: u}}
	s.Options.ServiceRuntime = r
	s.Options.ProgramRunner = shapeverify.ProgramRunnerFunc(func(_ context.Context, run shapeverify.ProgramRun) error {
		if run.Argv[0] == "builder" {
			return os.WriteFile(run.Argv[1], []byte("sha256:"+strings.Repeat("b", 64)), 0600)
		}
		return nil
	})
	return s, c, input, r
}
func TestProgramServiceHealthBeforePassAndDataIsolation(t *testing.T) {
	s, c, input, r := serviceFixture(t)
	ctx := context.Background()
	original := cloneService(r.units["disposable"])
	candidate, e := s.Import(ctx, c, Wish{Request: "change image"}, input)
	if e != nil {
		t.Fatal(e)
	}
	verified, e := s.Verify(ctx, c, candidate.ID)
	if e != nil {
		t.Fatal(e)
	}
	if !verified.Verdict.Pass || r.probes == 0 {
		t.Fatal(verified.Verdict)
	}
	if len(r.units) != 1 || r.units[original.Name].Settings["image"] != original.Settings["image"] {
		t.Fatal("verification touched original unit")
	}
	if _, e = os.Stat(filepath.Join(original.Settings["data_dir"], "probe")); !os.IsNotExist(e) {
		t.Fatal("probe wrote real data")
	}
	approved, e := s.Approve(ctx, c, candidate.ID)
	if e != nil {
		t.Fatal(e)
	}
	if approved.Status != Approved {
		t.Fatal(approved)
	}
	live := r.units[original.Name]
	if live.Settings["data_dir"] != original.Settings["data_dir"] || live.Settings["image"] == original.Settings["image"] {
		t.Fatal(live)
	}
	b, _ := os.ReadFile(filepath.Join(live.Settings["data_dir"], "original"))
	if string(b) != "keep" {
		t.Fatal("data lost")
	}
}
func TestProgramServiceFailedHealthCannotActivate(t *testing.T) {
	s, c, input, r := serviceFixture(t)
	r.failHealth = true
	candidate, e := s.Import(context.Background(), c, Wish{Request: "bad image"}, input)
	if e != nil {
		t.Fatal(e)
	}
	v, e := s.Verify(context.Background(), c, candidate.ID)
	if !errors.Is(e, ErrVerification) {
		t.Fatal(e)
	}
	if v.Verdict.Pass {
		t.Fatal("unhealthy image sealed")
	}
	if len(r.units) != 1 {
		t.Fatal("failed preview leaked")
	}
}
func TestProgramServiceRefusesUnsafeProfiles(t *testing.T) {
	for _, args := range [][]string{{"-eTOKEN=secret"}, {"-v/host:/data"}, {"-p", "0.0.0.0:8081:8081"}, {"--network=host"}, {"--secret=x"}} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			s, _, _, r := serviceFixture(t)
			u := r.units["disposable"]
			u.Exec = append(u.Exec[:len(u.Exec)-1], append(args, "{settings.image}")...)
			r.units[u.Name] = u
			if _, e := s.serviceUnit(context.Background()); e == nil {
				t.Fatal("unsafe unit accepted")
			}
		})
	}
}
func TestProgramServiceLiveHealthFailureRollsBackImage(t *testing.T) {
	s, c, input, r := serviceFixture(t)
	r.failLive = true
	ctx := context.Background()
	before := cloneService(r.units["disposable"])
	candidate, e := s.Import(ctx, c, Wish{Request: "runtime unhealthy"}, input)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Approve(ctx, c, candidate.ID); e == nil {
		t.Fatal("failed live health activated")
	}
	current := r.units[before.Name]
	if current.Settings["image"] != before.Settings["image"] || current.Settings["data_dir"] != before.Settings["data_dir"] {
		t.Fatal("rollback did not preserve old unit", current)
	}
	active, e := s.hostSource()
	if e != nil || active != candidate.BaseRev {
		t.Fatalf("source switched: %s %v", active, e)
	}
	if _, e = s.readTransition(); !os.IsNotExist(e) {
		t.Fatal("successful rollback journal retained", e)
	}
}
func TestProgramServiceCleanupFailureRetainsOwnedData(t *testing.T) {
	s, c, input, r := serviceFixture(t)
	r.failHealth = true
	r.failStop = true
	ctx := context.Background()
	candidate, e := s.Import(ctx, c, Wish{Request: "cleanup interruption"}, input)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Verify(ctx, c, candidate.ID); !errors.Is(e, ErrVerification) {
		t.Fatal(e)
	}
	if len(r.units) != 2 {
		t.Fatal("failed stop removed supervised unit")
	}
	for name, u := range r.units {
		if name != "disposable" {
			if _, e = os.Stat(u.Settings["data_dir"]); e != nil {
				t.Fatal("live preview data removed", e)
			}
		}
	}
	r.failHealth = false
	r.failStop = false
	verified, e := s.Verify(ctx, c, candidate.ID)
	if e != nil || !verified.Verdict.Pass || len(r.units) != 1 {
		t.Fatalf("retry failed %+v %v units=%d", verified.Verdict, e, len(r.units))
	}
}

// Real fleet load/save/remove semantics with only process supervision stubbed.
// In particular, missing units retain BashyServiceRuntime's wrapped sentinel.
type catalogPreviewRuntime struct {
	BashyServiceRuntime
	started, stopped []string
}

func (r *catalogPreviewRuntime) Restart(_ context.Context, name string) error {
	r.started = append(r.started, name)
	return nil
}
func (r *catalogPreviewRuntime) Healthy(context.Context, fleet.Service) error { return nil }
func (r *catalogPreviewRuntime) Stop(_ context.Context, name string) error {
	r.stopped = append(r.stopped, name)
	return nil
}
func TestProgramServicePreviewUsesWrappedMissingRealCatalog(t *testing.T) {
	s, caller, input, originalRuntime := serviceFixture(t)
	catalog := fleet.New(fleet.WithRoot(t.TempDir()), fleet.WithoutCloudOverlay())
	runtime := &catalogPreviewRuntime{BashyServiceRuntime: BashyServiceRuntime{Catalog: catalog}}
	ctx := context.Background()
	_, missing := runtime.Load(ctx, "shape-preview-missing-native-catalog")
	if missing == nil || !errors.Is(missing, os.ErrNotExist) {
		t.Fatalf("real missing unit sentinel: %v", missing)
	}
	if os.IsNotExist(missing) {
		t.Fatal("fixture must exercise the real wrapped-not-exist case")
	}
	if e := runtime.Save(ctx, originalRuntime.units["disposable"]); e != nil {
		t.Fatal(e)
	}
	s.Options.ServiceRuntime = runtime
	candidate, e := s.Import(ctx, caller, Wish{Request: "actual-catalog preview"}, input)
	if e != nil {
		t.Fatal(e)
	}
	verified, e := s.Verify(ctx, caller, candidate.ID)
	if e != nil || verified.Verdict == nil || !verified.Verdict.Pass {
		t.Fatalf("wrapped missing unit refused before disposable health: %+v %v", verified.Verdict, e)
	}
	preview, e := s.PreviewProgram(ctx, caller, candidate.ID)
	if e != nil {
		t.Fatal(e)
	}
	unit, e := runtime.Load(ctx, preview.Unit)
	if e != nil || unit.Settings["data_dir"] != preview.DataDir {
		t.Fatalf("preview not stored in real catalog: %+v %v", unit, e)
	}
	if e = runtime.Remove(ctx, preview.Unit); e != nil {
		t.Fatal(e)
	}
	// Recovery from a disappeared preview re-enters both absence checks.
	preview, e = s.PreviewProgram(ctx, caller, candidate.ID)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Reject(ctx, caller, candidate.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = runtime.Load(ctx, preview.Unit); !errors.Is(e, os.ErrNotExist) {
		t.Fatalf("preview not removed: %v", e)
	}
	// Cleanup of an already missing, owned preview must remove only owned data.
	home, _ := s.home()
	id := "0123456789abcdef"
	data := filepath.Join(home, "shape-host", s.App.Name, "previews", id, "data")
	if e = os.MkdirAll(data, 0700); e != nil {
		t.Fatal(e)
	}
	if e = s.cleanupProgramPreview(ctx, Candidate{ID: id, ProgramPreview: &ProgramPreview{Unit: "shape-preview-" + s.App.Name + "-" + id, DataDir: data}}); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(data); !os.IsNotExist(e) {
		t.Fatal("missing preview cleanup left private data", e)
	}
	if _, e = runtime.Load(ctx, "disposable"); e != nil {
		t.Fatal("original unit removed", e)
	}
	if len(runtime.started) < 3 || len(runtime.stopped) < 2 {
		t.Fatalf("disposable lifecycle not exercised: %+v", runtime)
	}
}
