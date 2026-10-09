package fleet

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// Every registry noun follows one name rule: a name that already belongs to
// another entry of the kind is refused unless --force. Person used to
// overwrite silently; command and app refused but pointed at a --force flag
// they did not have.
func TestCreatePersonRefusesExistingNameUnlessForced(t *testing.T) {
	cat := New(WithRoot(t.TempDir()))
	if err := cat.CreatePerson(Person{Handle: "alice", Display: "Alice", Aliases: []string{"al"}}, false); err != nil {
		t.Fatal(err)
	}
	if err := cat.CreatePerson(Person{Handle: "alice", Display: "Impostor"}, false); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("duplicate handle: err = %v", err)
	}
	if err := cat.CreatePerson(Person{Handle: "bob", Aliases: []string{"al"}}, false); err == nil || !strings.Contains(err.Error(), "already belongs") {
		t.Fatalf("alias collision: err = %v", err)
	}
	if p, _ := cat.Person("alice"); p.Display != "Alice" {
		t.Fatalf("refused create still changed the record: %+v", p)
	}
	if err := cat.CreatePerson(Person{Handle: "alice", Display: "Alice B"}, true); err != nil {
		t.Fatalf("--force: %v", err)
	}
	if p, _ := cat.Person("alice"); p.Display != "Alice B" {
		t.Fatalf("--force did not replace: %+v", p)
	}
}

func TestPersonIsASchemaNoun(t *testing.T) {
	var paths []string
	for _, f := range schemaFields(KindPerson) {
		paths = append(paths, f.Path)
	}
	joined := strings.Join(paths, " ")
	for _, want := range []string{"handle", "email", "os_users"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("person schema lacks %q: %v", want, paths)
		}
	}
}

func TestCommandAndAppAddHonourForce(t *testing.T) {
	opts := []Option{WithRoot(t.TempDir())}
	if _, err := runCmd(t, NewCommandsCmd(opts...), "add", "one", "--set", "exec.0=external-program"); err != nil {
		t.Fatal(err)
	}
	add := []string{"add", "two", "--set", "exec.0=external-program", "--set", "aliases.0=one"}
	if _, err := runCmd(t, NewCommandsCmd(opts...), add...); err == nil {
		t.Fatal("a name owned by another command was taken without --force")
	}
	if out, err := runCmd(t, NewCommandsCmd(opts...), append(add, "--force")...); err != nil {
		t.Fatalf("--force refused: %v\n%s", err, out)
	}
	out, err := runCmd(t, NewCommandsCmd(opts...), "show", "two", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var rec map[string]any
	if json.Unmarshal([]byte(out), &rec) != nil || rec["name"] != "two" {
		t.Fatalf("show two = %s", out)
	}
	for _, c := range []*cobra.Command{newCommandsAdd(opts), newCommandsSet(opts), newAppsAdd(opts), newAppsSet(opts)} {
		if c.Flags().Lookup("force") == nil {
			t.Errorf("%q has no --force", c.Use)
		}
	}
}
