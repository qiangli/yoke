package sdlc

import (
	"context"
	"runtime"
	"testing"
)

func TestRunDeployTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh -c; deploy targets run on unix hosts")
	}
	ctx := context.Background()

	t.Run("skipped when no command", func(t *testing.T) {
		out, err := RunDeployTarget(ctx, ".", TargetConfig{Name: "qa"})
		if err != nil || out.Status != "skipped" {
			t.Fatalf("got status=%q err=%v", out.Status, err)
		}
	})

	t.Run("deployed on success", func(t *testing.T) {
		out, err := RunDeployTarget(ctx, ".", TargetConfig{
			Name: "qa", Environment: "qa", Command: "exit 0", Healthcheck: "exit 0",
		})
		if err != nil || out.Status != "deployed" {
			t.Fatalf("got status=%q err=%v", out.Status, err)
		}
	})

	t.Run("rolled-back when command fails", func(t *testing.T) {
		out, err := RunDeployTarget(ctx, ".", TargetConfig{
			Name: "prod", Command: "exit 3", Rollback: "exit 0",
		})
		if err == nil {
			t.Fatal("expected the command failure to surface")
		}
		if out.Status != "rolled-back" || !out.RolledBack {
			t.Fatalf("got status=%q rolledBack=%v", out.Status, out.RolledBack)
		}
	})

	t.Run("failed when healthcheck fails and no rollback", func(t *testing.T) {
		out, err := RunDeployTarget(ctx, ".", TargetConfig{
			Name: "prod", Command: "exit 0", Healthcheck: "exit 1",
		})
		if err == nil {
			t.Fatal("expected healthcheck failure to surface")
		}
		if out.Status != "failed" || out.RolledBack {
			t.Fatalf("got status=%q rolledBack=%v", out.Status, out.RolledBack)
		}
	})
}
