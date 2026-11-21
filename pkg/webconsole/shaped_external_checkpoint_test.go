package webconsole

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeExternalCheckpointDriver struct {
	greeting      string
	greetingErr   error
	activeUI      string
	activeUIErr   error
	shots         []func(context.Context) ([]byte, error)
	shotCalls     int
	greetingCalls int
	activeCalls   int
}

func (f *fakeExternalCheckpointDriver) CheckGreeting(context.Context) (string, error) {
	f.greetingCalls++
	return f.greeting, f.greetingErr
}

func (f *fakeExternalCheckpointDriver) CheckActiveUI(context.Context) (string, error) {
	f.activeCalls++
	return f.activeUI, f.activeUIErr
}

func (f *fakeExternalCheckpointDriver) CaptureScreenshot(ctx context.Context) ([]byte, error) {
	idx := f.shotCalls
	f.shotCalls++
	if idx < len(f.shots) && f.shots[idx] != nil {
		return f.shots[idx](ctx)
	}
	return []byte("png-bytes"), nil
}

func TestExternalShapeCheckpoint_BoundedScreenshotKeepsDOMAndActiveUI(t *testing.T) {
	hang := func(ctx context.Context) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	okShot := func(context.Context) ([]byte, error) {
		return []byte("ok-png"), nil
	}

	t.Run("required screenshot timeout fails visibly but still runs active UI check", func(t *testing.T) {
		dir := t.TempDir()
		drv := &fakeExternalCheckpointDriver{
			greeting: "Hello, Shaping!",
			activeUI: "abcdef0123",
			shots:    []func(context.Context) ([]byte, error){hang, okShot},
		}
		parentCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		started := time.Now()
		obs, failures := runExternalShapeCheckpoint(parentCtx, externalCheckpointConfig{
			ExpectedGreeting:   "Hello, Shaping!",
			ExpectedRevision:   "abcdef0123456789abcdef0123456789abcdef01",
			ArtifactDir:        dir,
			Cookie:             "secret-cookie-123",
			ScreenshotTimeout:  30 * time.Millisecond,
			ScreenshotRequired: true,
		}, drv, nil)
		elapsed := time.Since(started)
		if elapsed >= 500*time.Millisecond {
			t.Fatalf("hung screenshot was not bounded separately: elapsed=%v", elapsed)
		}
		if drv.greetingCalls != 1 || drv.activeCalls != 1 {
			t.Fatalf("expected both DOM checkpoints to run; greeting=%d active=%d", drv.greetingCalls, drv.activeCalls)
		}
		if obs.Greeting != "Hello, Shaping!" || obs.ActiveUI != "abcdef0123" {
			t.Fatalf("DOM values lost: %+v", obs)
		}
		if obs.Passed || len(failures) == 0 || !strings.Contains(obs.Failure, "page.png") {
			t.Fatalf("expected visible failure for required screenshot timeout: obs=%+v failures=%v", obs, failures)
		}
		if obs.Screenshots["page"].Status != "timeout" || obs.Screenshots["shape"].Status != "ok" {
			t.Fatalf("unexpected screenshot statuses: %+v", obs.Screenshots)
		}
		if _, err := os.Stat(filepath.Join(dir, "shape.png")); err != nil {
			t.Fatalf("expected shape.png written: %v", err)
		}
	})

	t.Run("optional screenshot timeout still records error visibly while DOM pass succeeds", func(t *testing.T) {
		dir := t.TempDir()
		drv := &fakeExternalCheckpointDriver{
			greeting: "Hello, Shaping!",
			activeUI: "abcdef0123",
			shots:    []func(context.Context) ([]byte, error){hang, hang},
		}
		parentCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		obs, failures := runExternalShapeCheckpoint(parentCtx, externalCheckpointConfig{
			ExpectedGreeting:   "Hello, Shaping!",
			ExpectedRevision:   "abcdef0123456789abcdef0123456789abcdef01",
			ArtifactDir:        dir,
			Cookie:             "secret-cookie-123",
			ScreenshotTimeout:  25 * time.Millisecond,
			ScreenshotRequired: false,
		}, drv, nil)
		if !obs.Passed || len(failures) != 0 {
			t.Fatalf("expected pass when screenshots optional and DOM/UI ok: obs=%+v failures=%v", obs, failures)
		}
		if obs.Screenshots["page"].Status != "timeout" || obs.Screenshots["page"].Error == "" ||
			obs.Screenshots["shape"].Status != "timeout" || obs.Screenshots["shape"].Error == "" {
			t.Fatalf("expected both screenshot errors visibly recorded: %+v", obs.Screenshots)
		}
	})

	t.Run("greeting mismatch or active UI error still fails even when screenshots optional", func(t *testing.T) {
		dir := t.TempDir()
		drv := &fakeExternalCheckpointDriver{
			greeting:    "Hello, Shaping!",
			activeUIErr: errors.New("poll failed with cookie=secret-cookie-123"),
		}
		obs, failures := runExternalShapeCheckpoint(context.Background(), externalCheckpointConfig{
			ExpectedGreeting:   "Hello, Shaping!",
			ExpectedRevision:   "abcdef0123456789abcdef0123456789abcdef01",
			ArtifactDir:        dir,
			Cookie:             "secret-cookie-123",
			ScreenshotTimeout:  25 * time.Millisecond,
			ScreenshotRequired: false,
		}, drv, nil)
		if obs.Passed || len(failures) == 0 {
			t.Fatalf("expected failure when active UI fails: obs=%+v", obs)
		}
		if strings.Contains(obs.Failure, "secret-cookie-123") || !strings.Contains(obs.Failure, "[session redacted]") {
			t.Fatalf("cookie not redacted in failure: %q", obs.Failure)
		}
	})
}

func TestParseExternalScreenshotConfig(t *testing.T) {
	d, req, err := parseExternalScreenshotConfig("", "")
	if err != nil || d != defaultExternalScreenshotTimeout || !req {
		t.Fatalf("defaults = (%v, %v, %v)", d, req, err)
	}
	d, req, err = parseExternalScreenshotConfig("5s", "0")
	if err != nil || d != 5*time.Second || req {
		t.Fatalf("override = (%v, %v, %v)", d, req, err)
	}
	if _, _, err := parseExternalScreenshotConfig("0s", ""); err == nil {
		t.Fatal("expected error for non-positive duration")
	}
	if _, _, err := parseExternalScreenshotConfig("", "maybe"); err == nil {
		t.Fatal("expected error for invalid required flag")
	}
}
