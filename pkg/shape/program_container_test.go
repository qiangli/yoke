package shape

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
)

func TestContainerExitHelper(t *testing.T) {
	if os.Getenv("SHAPE_CONTAINER_EXIT_ONE") == "yes" {
		os.Exit(1)
	}
}
func containerMissingError(t *testing.T) error {
	t.Helper()
	c := exec.Command(os.Args[0], "-test.run=^TestContainerExitHelper$")
	c.Env = append(os.Environ(), "SHAPE_CONTAINER_EXIT_ONE=yes")
	return c.Run()
}

func TestProgramServiceRuntimeCompletedStopOwnership(t *testing.T) {
	for _, mode := range []string{"owned", "missing", "image", "mount", "destination", "extra-mount", "name", "id", "exists-error", "inspect-error", "remove-error", "status-error"} {
		t.Run(mode, func(t *testing.T) {
			_, _, _, fixture := serviceFixture(t)
			u := fixture.units["disposable"]
			cat := fleet.New(fleet.WithRoot(t.TempDir()), fleet.WithoutCloudOverlay())
			if e := cat.SaveService(u); e != nil {
				t.Fatal(e)
			}
			id := strings.Repeat("c", 64)
			doc := map[string]any{"Id": id, "Name": u.Name, "Image": strings.TrimPrefix(u.Settings["image"], "sha256:"), "Mounts": []map[string]string{{"Type": "bind", "Source": u.Settings["data_dir"], "Destination": "/data"}}}
			switch mode {
			case "image":
				doc["Image"] = strings.Repeat("b", 64)
			case "mount":
				doc["Mounts"].([]map[string]string)[0]["Source"] = t.TempDir()
			case "destination":
				doc["Mounts"].([]map[string]string)[0]["Destination"] = "/other"
			case "extra-mount":
				doc["Mounts"] = append(doc["Mounts"].([]map[string]string), map[string]string{"Type": "bind", "Source": "/other", "Destination": "/extra"})
			case "name":
				doc["Name"] = "foreign"
			case "id":
				doc["Id"] = "bad-id"
			}
			var calls []string
			polls := 0
			r := BashyServiceRuntime{Bashy: "controller", Catalog: cat, run: func(_ context.Context, exe string, args ...string) ([]byte, error) {
				calls = append(calls, exe+" "+strings.Join(args, " "))
				if exe == "controller" {
					if args[1] == "status" {
						polls++
						if mode == "status-error" {
							return nil, errors.New("status failed")
						}
						state := "inactive"
						if polls == 1 {
							state = "stopping"
						}
						return []byte(`{"units":[{"unit":"disposable","state":"` + state + `"}]}`), nil
					}
					return nil, nil
				}
				if polls < 2 {
					t.Fatal("container touched before normal stop completed")
				}
				switch args[1] {
				case "exists":
					if mode == "missing" {
						return nil, containerMissingError(t)
					}
					if mode == "exists-error" {
						return nil, errors.New("engine failed")
					}
				case "inspect":
					if mode == "inspect-error" {
						return nil, errors.New("inspect failed")
					}
					return json.Marshal([]any{doc})
				case "rm":
					if !reflect.DeepEqual(args, []string{"container", "rm", "--force", id}) {
						t.Fatalf("unsafe removal %q", args)
					}
					if mode == "remove-error" {
						return nil, errors.New("remove failed")
					}
				}
				return nil, nil
			}}
			e := r.Stop(context.Background(), u.Name)
			good := mode == "owned" || mode == "missing"
			if (e == nil) != good {
				t.Fatalf("mode %s: %v; %q", mode, e, calls)
			}
			if !good && mode != "remove-error" {
				for _, call := range calls {
					if strings.Contains(call, " rm ") {
						t.Fatal("removed unproven container")
					}
				}
			}
			if _, e := r.Load(context.Background(), u.Name); e != nil {
				t.Fatal("failed cleanup removed declaration", e)
			}
			if _, e := os.Stat(filepath.Join(u.Settings["data_dir"], "original")); e != nil {
				t.Fatal("cleanup removed data", e)
			}
		})
	}
}

func TestProgramServiceContainerBackendIdentity(t *testing.T) {
	_, _, _, fixture := serviceFixture(t)
	for _, backend := range [][]string{{"/trusted/podman", "run"}, {"/trusted/bashy", "oci", "run"}, {"/trusted/bashy.real", "podman", "run"}} {
		u := fixture.units["disposable"]
		u.Exec = append(append([]string(nil), backend...), u.Exec[2:]...)
		prefix, name, dest, e := containerIdentity(u)
		if e != nil || !reflect.DeepEqual(prefix, backend[:len(backend)-1]) || name != u.Name || dest != "/data" {
			t.Fatalf("backend changed: %q %q %s %v", backend, prefix, dest, e)
		}
	}
	u := fixture.units["disposable"]
	u.Exec[4] = "foreign"
	if _, _, _, e := containerIdentity(u); e == nil {
		t.Fatal("foreign container name accepted")
	}
}

type orderedContainerRuntime struct {
	*programServices
	calls []string
}

func (r *orderedContainerRuntime) Stop(ctx context.Context, name string) error {
	r.calls = append(r.calls, "stop:"+r.units[name].Settings["image"])
	return r.programServices.Stop(ctx, name)
}
func (r *orderedContainerRuntime) Save(ctx context.Context, u fleet.Service) error {
	r.calls = append(r.calls, "save:"+u.Settings["image"])
	return r.programServices.Save(ctx, u)
}
func (r *orderedContainerRuntime) Restart(_ context.Context, name string) error {
	r.calls = append(r.calls, "restart:"+r.units[name].Settings["image"])
	return nil
}
func TestProgramServiceTransitionStopsActualImageBeforeChangingDeclaration(t *testing.T) {
	s, _, _, units := serviceFixture(t)
	r := &orderedContainerRuntime{programServices: units}
	s.Options.ServiceRuntime = r
	previous := cloneService(units.units["disposable"])
	target := cloneService(previous)
	target.Settings["image"] = "sha256:" + strings.Repeat("b", 64)
	transition := programTransition{PreviousUnit: &previous, TargetUnit: &target}
	if e := s.applyProgramTarget(context.Background(), transition); e != nil {
		t.Fatal(e)
	}
	want := []string{"stop:" + previous.Settings["image"], "save:" + target.Settings["image"], "restart:" + target.Settings["image"]}
	if !reflect.DeepEqual(r.calls, want) {
		t.Fatalf("activation order %q", r.calls)
	}
	r.calls = nil
	if e := s.rollbackProgram(context.Background(), transition); e != nil {
		t.Fatal(e)
	}
	want = []string{"stop:" + target.Settings["image"], "save:" + previous.Settings["image"], "restart:" + previous.Settings["image"]}
	if !reflect.DeepEqual(r.calls, want) {
		t.Fatalf("rollback order %q", r.calls)
	}
	r.calls = nil
	r.failStop = true
	if e := s.applyProgramTarget(context.Background(), transition); e == nil {
		t.Fatal("incomplete stop accepted")
	}
	if len(r.calls) != 1 || r.units[previous.Name].Settings["image"] != previous.Settings["image"] {
		t.Fatal("declaration changed after failed stop", r.calls)
	}
}
