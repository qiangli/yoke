package fleet

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/spf13/cobra"
)

type guardCall struct{ kind, name string }

func stubGuard(calls *[]guardCall, err error) {
	SetMutationGuard(func(_ context.Context, kind, name string) error {
		*calls = append(*calls, guardCall{kind, name})
		return err
	})
}

func runVerb(t *testing.T, cmd *cobra.Command, args ...string) error {
	t.Helper()
	cmd.SetArgs(args)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	return cmd.Execute()
}

func withoutEditor(t *testing.T) {
	t.Helper()
	for _, key := range []string{"VISUAL", "EDITOR"} {
		key := key
		if v, ok := os.LookupEnv(key); ok {
			t.Cleanup(func() { os.Setenv(key, v) })
		} else {
			t.Cleanup(func() { os.Unsetenv(key) })
		}
		os.Unsetenv(key)
	}
}

func TestWriteGuardOnlyExistingEntries(t *testing.T) {
	root := t.TempDir()
	opts := []Option{WithRoot(root)}
	var calls []guardCall
	stubGuard(&calls, nil)
	t.Cleanup(func() { SetMutationGuard(nil) })

	const name = "guard-m1"
	if err := runVerb(t, newAdd(KindModel, opts), name, "--provider", "p", "--kind", "api"); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 0 {
		t.Fatalf("minting a new entry guarded: %+v", calls)
	}
	if err := runVerb(t, newAdd(KindModel, opts), name, "--provider", "p", "--kind", "api"); err != nil {
		t.Fatal(err)
	}
	if err := runVerb(t, newSet(KindModel, opts), name, "--set", "display=x"); err != nil {
		t.Fatal(err)
	}
	withoutEditor(t)
	if err := runVerb(t, newEdit(KindModel, opts, (*Catalog).MaterializeModel), name); err == nil {
		t.Fatal("edit without $EDITOR should fail after materializing")
	}
	if err := runVerb(t, newRm(KindModel, opts, (*Catalog).RemoveModel), "missing"); err == nil {
		t.Fatal("rm of a missing entry should fail")
	}
	// add-over, set, edit hit the guard with the canonical kind and name;
	// rm of a missing name never reaches it.
	want := []guardCall{{KindModel, name}, {KindModel, name}, {KindModel, name}}
	if len(calls) != len(want) {
		t.Fatalf("calls = %+v, want %+v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("calls = %+v, want %+v", calls, want)
		}
	}
}

func TestWriteGuardRefusalUnchanged(t *testing.T) {
	root := t.TempDir()
	opts := []Option{WithRoot(root)}
	sentinel := errors.New("held by someone else")
	var calls []guardCall
	stubGuard(&calls, sentinel)
	t.Cleanup(func() { SetMutationGuard(nil) })

	const name = "guard-m2"
	if err := runVerb(t, newAdd(KindModel, opts), name, "--provider", "p", "--kind", "api"); err != nil {
		t.Fatal(err)
	}
	if err := runVerb(t, newSet(KindModel, opts), name, "--set", "display=x"); err != sentinel {
		t.Fatalf("set returned %v, want the guard refusal unchanged", err)
	}
	if err := runVerb(t, newRm(KindModel, opts, (*Catalog).RemoveModel), name); err != sentinel {
		t.Fatalf("rm returned %v, want the guard refusal unchanged", err)
	}
}
