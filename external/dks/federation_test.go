package dks

import (
	"bytes"
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/qiangli/yoke/external/kube"
	"github.com/qiangli/yoke/external/meshagent"
)

func TestIsFederation(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"federation", "status"}, true},
		{[]string{"federation"}, true},
		{[]string{"provision"}, false},
		{[]string{"status"}, false},
		{nil, false},
	} {
		if got := IsFederation(tc.args); got != tc.want {
			t.Errorf("IsFederation(%q) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

// runFederationCmd drives the cobra command with a stubbed outpost and
// returns what the stub saw, plus the command's own output streams.
func runFederationCmd(t *testing.T, args []string) (calls [][]string, stdout, stderr string, err error) {
	t.Helper()
	old := ExecOutpost
	ExecOutpost = func(_ context.Context, got ...string) error {
		calls = append(calls, got)
		return nil
	}
	t.Cleanup(func() { ExecOutpost = old })

	t.Setenv(kube.EnvProfile, "") // the command must select federation itself
	cmd := NewFederationCmd()
	var outBuf, errBuf bytes.Buffer
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(args)
	err = cmd.Execute()
	return calls, outBuf.String(), errBuf.String(), err
}

func TestFederationVerbsExecOutpost(t *testing.T) {
	for _, sub := range []string{"join", "leave", "status", "on", "off", "token"} {
		t.Run(sub, func(t *testing.T) {
			calls, _, _, err := runFederationCmd(t, []string{sub, "--extra", "arg"})
			if err != nil {
				t.Fatalf("federation %s: Execute() error = %v", sub, err)
			}
			want := []string{"cluster", "federation", sub, "--extra", "arg"}
			if len(calls) != 1 || !slices.Equal(calls[0], want) {
				t.Fatalf("outpost calls = %v, want one call %v", calls, want)
			}
		})
	}
}

func TestFederationSelectsFederationProfile(t *testing.T) {
	t.Setenv(kube.EnvProfile, "peer")
	old := ExecOutpost
	ExecOutpost = func(_ context.Context, _ ...string) error {
		if got := os.Getenv(kube.EnvProfile); got != kube.ProfileFederation {
			t.Errorf("outpost inherited %s=%q, want federation", kube.EnvProfile, got)
		}
		return nil
	}
	t.Cleanup(func() { ExecOutpost = old })
	cmd := NewFederationCmd()
	cmd.SetArgs([]string{"status"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := os.Getenv(kube.EnvProfile); got != "peer" {
		t.Errorf("%s leaked after federation status: got %q, want peer", kube.EnvProfile, got)
	}
}

func TestFederationKarmadactlSelectsAndRestoresProfile(t *testing.T) {
	t.Setenv(kube.EnvProfile, "cloudbox")
	old := execKarmadactl
	execKarmadactl = func(_ context.Context, args []string) error {
		if !slices.Equal(args, []string{"version"}) {
			t.Errorf("karmadactl args = %v, want [version]", args)
		}
		if got := os.Getenv(kube.EnvProfile); got != kube.ProfileFederation {
			t.Errorf("karmadactl inherited %s=%q, want federation", kube.EnvProfile, got)
		}
		return nil
	}
	t.Cleanup(func() { execKarmadactl = old })
	cmd := NewFederationCmd()
	cmd.SetArgs([]string{"karmadactl", "version"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := os.Getenv(kube.EnvProfile); got != "cloudbox" {
		t.Errorf("%s leaked after karmadactl: got %q, want cloudbox", kube.EnvProfile, got)
	}
}

func TestFederationUnknownSubcommand(t *testing.T) {
	_, _, stderr, err := runFederationCmd(t, []string{"bogus"})
	if err == nil {
		t.Fatal("Execute() = nil, want an error for an unknown subcommand")
	}
	if !errors.Is(err, errHandled) {
		t.Errorf("Execute() error = %v, want the handled sentinel", err)
	}
	for _, want := range []string{"join", "leave", "status", "karmadactl"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr %q does not name %q", stderr, want)
		}
	}
}

func TestFederationHelp(t *testing.T) {
	for _, args := range [][]string{{}, {"-h"}, {"--help"}, {"help"}} {
		calls, stdout, _, err := runFederationCmd(t, args)
		if err != nil {
			t.Fatalf("federation %v: Execute() error = %v", args, err)
		}
		if len(calls) != 0 {
			t.Errorf("federation %v exec'd outpost: %v", args, calls)
		}
		if !strings.Contains(stdout, "karmadactl") {
			t.Errorf("federation %v help does not mention karmadactl", args)
		}
	}
}

func TestFederationOutpostAbsent(t *testing.T) {
	old := ExecOutpost
	ExecOutpost = func(_ context.Context, _ ...string) error { return meshagent.ErrNotFound }
	t.Cleanup(func() { ExecOutpost = old })

	cmd := NewFederationCmd()
	var errBuf bytes.Buffer
	cmd.SetErr(&errBuf)
	cmd.SetArgs([]string{"status"})
	err := cmd.Execute()
	if !errors.Is(err, errHandled) {
		t.Fatalf("Execute() error = %v, want the handled sentinel", err)
	}
	if !strings.Contains(errBuf.String(), "OUTPOST_BIN") {
		t.Errorf("stderr %q does not point at $OUTPOST_BIN", errBuf.String())
	}
}

// TestFederationKarmadactlReachesRegistry pins the karmadactl branch routing
// without network: offline, Ensure must fail closed before any download, and
// the failure must name karmadactl (not some other tool).
func TestFederationKarmadactlReachesRegistry(t *testing.T) {
	t.Setenv("BASHY_BIN_CACHE", t.TempDir())
	t.Setenv("BASHY_OFFLINE", "1")
	cmd := NewFederationCmd()
	var errBuf bytes.Buffer
	cmd.SetErr(&errBuf)
	cmd.SetArgs([]string{"karmadactl", "version"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() = nil, want the offline fail-closed error")
	}
	if errors.Is(err, errHandled) {
		t.Fatalf("Execute() error = %v, want the real registry error, not the handled sentinel", err)
	}
	if !strings.Contains(err.Error(), "karmadactl") {
		t.Errorf("Execute() error = %q, want it to name karmadactl", err.Error())
	}
}
