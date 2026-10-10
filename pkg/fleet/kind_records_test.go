package fleet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/assetring"
)

func TestCommandAppPersonHostDeclareRecordSpecs(t *testing.T) {
	for _, kind := range []string{KindCommand, KindApp, KindPerson, KindHost} {
		spec, ok := kindByName(kind)
		if !ok || spec.Record == nil {
			t.Fatalf("%s: no record spec on the kind table", kind)
		}
		for _, verb := range []string{"show", "add", "set"} {
			if runKindVerb(kind, nil, verb) == nil {
				t.Errorf("%s: no %s verb", kind, verb)
			}
		}
	}
}

func sharedEntry(t *testing.T, dir, file, body string) Option {
	t.Helper()
	shared := t.TempDir()
	if err := os.WriteFile(filepath.Join(shared, file), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return WithSource(dir, assetring.FileDir(shared, assetring.RingShared, ext))
}

func TestSetOnASharedEntryWritesASparseOverlayForEveryKind(t *testing.T) {
	cases := []struct {
		kind, dir, file, body, entry string
		verb                         []string
		want                         string
	}{
		{KindCommand, dirCommands, "org.yaml", "name: org\nkind: command\nexec: [/bin/true]\n", "org", []string{"set", "org", "--hidden"}, "hidden: true"},
		{KindApp, dirApps, "wiki.yaml", "name: wiki\nkind: app\nport: 9000\n", "wiki", []string{"set", "wiki", "--label", "Wiki"}, "label: Wiki"},
		{KindHost, dirHosts, "box.yaml", "name: box\naddress: box.test\n", "box", []string{"set", "box", "--ssh-user", "me"}, "ssh_user: me"},
		{KindPerson, dirPeople, "pat.yaml", "handle: pat\ndisplay: Pat\n", "pat", []string{"set", "pat", "--email", "pat@example.test"}, "email: pat@example.test"},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			isolatedFleetRoot(t)
			root := t.TempDir()
			opts := []Option{WithRoot(root), sharedEntry(t, tc.dir, tc.file, tc.body)}
			out, err := runKind(t, tc.kind, opts, tc.verb...)
			if err != nil {
				t.Fatalf("%v: %v\n%s", tc.verb, err, out)
			}
			if !strings.Contains(out, "note: overlaid "+tc.entry+" from the shared ring in the local store") {
				t.Errorf("no overlay note:\n%s", out)
			}
			got, err := os.ReadFile(filepath.Join(root, tc.dir, tc.file))
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"overlay: true", tc.want} {
				if !strings.Contains(string(got), want) {
					t.Errorf("local overlay lacks %q:\n%s", want, got)
				}
			}
			if strings.Contains(string(got), "kind: ") && tc.kind != KindApp && tc.kind != KindCommand {
				t.Errorf("overlay is not sparse:\n%s", got)
			}
		})
	}
}

func runKind(t *testing.T, kind string, opts []Option, args ...string) (string, error) {
	t.Helper()
	switch kind {
	case KindCommand:
		return runCmd(t, NewCommandsCmd(opts...), args...)
	case KindApp:
		return runCmd(t, appRoot(opts), args...)
	case KindHost:
		return runCmd(t, NewHostsCmd(opts...), args...)
	}
	return runCmd(t, genericRoot(kind, opts), args...)
}

// genericRoot mounts only the generic verbs, as a kind whose CLI root lives
// outside this package (person) does.
func genericRoot(kind string, opts []Option) *cobra.Command {
	root := &cobra.Command{Use: kind, SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(newShow(kind, opts), newAdd(kind, opts), newSet(kind, opts))
	return root
}

func runKindVerb(kind string, opts []Option, verb string) *cobra.Command {
	for _, c := range genericRoot(kind, opts).Commands() {
		if c.Name() == verb {
			return c
		}
	}
	return nil
}

func TestCommandSetRevalidatesAnOverlaidEntryAgainstReservedNames(t *testing.T) {
	isolatedFleetRoot(t)
	root := t.TempDir()
	opts := []Option{WithRoot(root), WithReservedNames(reservedFixture),
		sharedEntry(t, dirCommands, "cat.yaml", "name: cat\nkind: command\nexec: [/bin/true]\n")}
	out, err := runCmd(t, NewCommandsCmd(opts...), "set", "cat", "--hidden")
	if err == nil || !strings.Contains(err.Error(), "never a command bashy ships") {
		t.Fatalf("set on a shadowed entry must still be refused, got %v\n%s", err, out)
	}
	if _, statErr := os.Stat(filepath.Join(root, dirCommands, "cat.yaml")); statErr == nil {
		t.Error("a refused set must write nothing")
	}
}

func TestAppSetRevalidatesAnOverlaidEntry(t *testing.T) {
	isolatedFleetRoot(t)
	root := t.TempDir()
	opts := []Option{WithRoot(root), sharedEntry(t, dirApps, "wiki.yaml", "name: wiki\nkind: app\nport: 9000\n")}
	if _, err := runCmd(t, appRoot(opts), "set", "wiki", "--port", "70000"); err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("an out-of-range port must be refused on an overlay, got %v", err)
	}
}

func TestPersonAddRefusesAnExistingHandleUnlessForced(t *testing.T) {
	isolatedFleetRoot(t)
	opts := []Option{WithRoot(t.TempDir())}
	if _, err := runCmd(t, genericRoot(KindPerson, opts), "add", "alice", "--display", "Alice", "--alias", "al"); err != nil {
		t.Fatal(err)
	}
	if _, err := runCmd(t, genericRoot(KindPerson, opts), "add", "alice", "--display", "Impostor"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("duplicate handle: %v", err)
	}
	if _, err := runCmd(t, genericRoot(KindPerson, opts), "add", "alice", "--display", "Alice B", "--force"); err != nil {
		t.Errorf("--force: %v", err)
	}
	if p, _ := New(opts...).Person("alice"); p.Display != "Alice B" {
		t.Errorf("forced replace = %+v", p)
	}
}

func TestPersonSetMergesOSUsersAndTakesAliasFlags(t *testing.T) {
	isolatedFleetRoot(t)
	opts := []Option{WithRoot(t.TempDir())}
	if _, err := runCmd(t, genericRoot(KindPerson, opts), "add", "alice", "--os-user", "a=x", "--alias", "al"); err != nil {
		t.Fatal(err)
	}
	if _, err := runCmd(t, genericRoot(KindPerson, opts), "set", "alice", "--os-user", "b=y", "--add-alias", "ally", "--rm-alias", "al"); err != nil {
		t.Fatal(err)
	}
	p, _ := New(opts...).Person("alice")
	if p.OSUsers["a"] != "x" || p.OSUsers["b"] != "y" || strings.Join(p.Aliases, ",") != "ally" {
		t.Errorf("person = %+v", p)
	}
	if _, err := runCmd(t, genericRoot(KindPerson, opts), "add", "bob", "--os-user", "nope"); err == nil || !strings.Contains(err.Error(), "host=account") {
		t.Errorf("bad --os-user: %v", err)
	}
	out, err := runCmd(t, genericRoot(KindPerson, opts), "show", "alice", "--field", "default_os_user")
	if err != nil {
		t.Fatalf("show --field: %v\n%s", err, out)
	}
}

func TestHostsCmdIsTheGenericVerbSet(t *testing.T) {
	isolatedFleetRoot(t)
	opts := []Option{WithRoot(t.TempDir())}
	run := func(args ...string) string {
		t.Helper()
		out, err := runCmd(t, NewHostsCmd(opts...), args...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		return out
	}
	if out := run("add", "lab", "--address", "lab.example.test", "--ssh-user", "me", "--ssh-port", "2222", "--alias", "labbox"); strings.TrimSpace(out) != "lab" {
		t.Errorf("add echo = %q", out)
	}
	run("set", "lab", "--notes", "rack 4", "--add-alias", "bench")
	h, ok := New(opts...).Host("bench")
	if !ok || h.SSHPort != 2222 || h.Notes != "rack 4" || h.Address != "lab.example.test" || h.SSHUser != "me" {
		t.Fatalf("host = %+v %v", h, ok)
	}
	if out := run("show", "lab", "--field", "ssh_port"); strings.TrimSpace(out) != "2222" {
		t.Errorf("show --field = %q", out)
	}
	list := run("list")
	if !strings.Contains(list, "lab") || !strings.Contains(list, "lab.example.test") || !strings.Contains(run("list", "--json"), `"ssh_port": 2222`) {
		t.Errorf("list = %q", list)
	}
	if !strings.Contains(run("schema", "--json"), "ssh_user") {
		t.Error("schema lacks ssh_user")
	}
	if _, err := runCmd(t, NewHostsCmd(opts...), "add", "other", "--address", "x", "--alias", "lab"); err == nil || !strings.Contains(err.Error(), "already belongs") {
		t.Errorf("alias collision: %v", err)
	}
	run("retire", "lab", "--reason", "decommissioned")
	if !strings.Contains(run("list", "--retired"), "lab") {
		t.Error("retired host missing from --retired")
	}
	run("unretire", "lab")
	if out := run("rm", "lab"); !strings.Contains(out, "removed") {
		t.Errorf("rm = %q", out)
	}
	if _, ok := New(opts...).Host("lab"); ok {
		t.Error("host survived rm")
	}
}
