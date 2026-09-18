package dag

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// The shipped `bashy dag` mounts AddCapacityCommands on the same root this
// package tests bare. With a subcommand present and no explicit Args, cobra
// rejects every positional target as an unknown subcommand — a regression the
// bare-root tests cannot see. Every case here runs with `capacity` mounted.
func mountedDagCmd(t *testing.T, path string, args ...string) (*bytes.Buffer, *bytes.Buffer, error) {
	t.Helper()
	cmd := NewDagCmd()
	AddCapacityCommands(cmd, CapacityServices{})
	out, errOut := new(bytes.Buffer), new(bytes.Buffer)
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	cmd.SetArgs(append([]string{"--file", path}, args...))
	return out, errOut, cmd.Execute()
}

func TestTargetRunsWhenSubcommandsAreMounted(t *testing.T) {
	path := writeDAG(t, "## Tasks\n\n### hello\nSay hi.\n"+block("bash", "echo hi-from-target"))

	out, errOut, err := mountedDagCmd(t, path, "hello")
	if err != nil {
		t.Fatalf("target with capacity mounted: %v (stderr=%s)", err, errOut.String())
	}
	if !strings.Contains(out.String()+errOut.String(), "hi-from-target") {
		t.Fatalf("target body did not run:\nstdout=%s\nstderr=%s", out.String(), errOut.String())
	}
}

func TestDryRunPlanPrintsWhenSubcommandsAreMounted(t *testing.T) {
	path := writeDAG(t, "## Tasks\n\n### hello\nSay hi.\n"+block("bash", "echo hi"))

	out, errOut, err := mountedDagCmd(t, path, "-n", "hello")
	if err != nil {
		t.Fatalf("dry run with capacity mounted: %v (stderr=%s)", err, errOut.String())
	}
	if !strings.Contains(out.String(), "hello") {
		t.Fatalf("plan did not name the target:\n%s", out.String())
	}
}

func TestUnknownTargetIsADagErrorNotACobraLookup(t *testing.T) {
	path := writeDAG(t, "## Tasks\n\n### hello\nSay hi.\n"+block("bash", "echo hi"))

	_, errOut, err := mountedDagCmd(t, path, "nosuch")
	if err == nil {
		t.Fatal("unknown target must fail")
	}
	var dagErr *Error
	if !errors.As(err, &dagErr) {
		t.Fatalf("unknown target must surface as *dag.Error, got %T: %v", err, err)
	}
	if strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("cobra's subcommand lookup leaked into a target error: %v", err)
	}
	if errOut.Len() == 0 {
		t.Fatal("a failing target must explain itself on stderr")
	}
}

func TestCapacitySubcommandStillMounts(t *testing.T) {
	cmd := NewDagCmd()
	AddCapacityCommands(cmd, CapacityServices{})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"capacity", "plan"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "request") {
		t.Fatalf("capacity plan must still resolve as the subcommand and demand --request, got %v", err)
	}
}
