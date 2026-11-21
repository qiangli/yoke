package webconsole

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const defaultExternalScreenshotTimeout = 15 * time.Second

type externalCheckpointConfig struct {
	App                string
	PagePath           string
	ExpectedGreeting   string
	ExpectedRevision   string
	ArtifactDir        string
	Cookie             string
	ScreenshotTimeout  time.Duration
	ScreenshotRequired bool
}

type screenshotObservation struct {
	File   string `json:"file"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

type externalCheckpointObservation struct {
	Greeting         string                           `json:"greeting"`
	ExpectedGreeting string                           `json:"expected_greeting"`
	ActiveUI         string                           `json:"active_ui"`
	ExpectedRevision string                           `json:"expected_revision"`
	Screenshots      map[string]screenshotObservation `json:"screenshots,omitempty"`
	Errors           []string                         `json:"browser_errors"`
	Failure          string                           `json:"failure,omitempty"`
	Passed           bool                             `json:"passed"`
}

type externalCheckpointDriver interface {
	CheckGreeting(ctx context.Context) (string, error)
	CheckActiveUI(ctx context.Context) (string, error)
	CaptureScreenshot(ctx context.Context) ([]byte, error)
}

func parseExternalScreenshotConfig(timeoutRaw, requiredRaw string) (time.Duration, bool, error) {
	timeout := defaultExternalScreenshotTimeout
	if s := strings.TrimSpace(timeoutRaw); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil || d <= 0 {
			return 0, false, fmt.Errorf("invalid screenshot timeout %q", timeoutRaw)
		}
		timeout = d
	}
	required := true
	switch strings.ToLower(strings.TrimSpace(requiredRaw)) {
	case "", "1", "true", "yes", "on":
		required = true
	case "0", "false", "no", "off":
		required = false
	default:
		return 0, false, fmt.Errorf("invalid screenshot required setting %q", requiredRaw)
	}
	return timeout, required, nil
}

func redactCheckpointSecret(msg, secret string) string {
	if secret == "" {
		return msg
	}
	return strings.ReplaceAll(msg, secret, "[session redacted]")
}

// runExternalShapeCheckpoint executes DOM/revision checkpoints before bounded
// screenshot captures. A stalled or failing screenshot never prevents the
// active-revision DOM check from running; required capture failures still fail
// the checkpoint visibly while optional captures record their error in
// checkpoint.json without failing DOM-verified checkpoints.
func runExternalShapeCheckpoint(
	ctx context.Context,
	cfg externalCheckpointConfig,
	drv externalCheckpointDriver,
	browserErrors func() []string,
) (externalCheckpointObservation, []string) {
	if cfg.ScreenshotTimeout <= 0 {
		cfg.ScreenshotTimeout = defaultExternalScreenshotTimeout
	}
	obs := externalCheckpointObservation{
		ExpectedGreeting: cfg.ExpectedGreeting,
		ExpectedRevision: cfg.ExpectedRevision,
		Screenshots:      make(map[string]screenshotObservation, 2),
	}
	var failures []string
	recordFailure := func(stage string, err error) {
		msg := redactCheckpointSecret(stage+": "+err.Error(), cfg.Cookie)
		if obs.Failure == "" {
			obs.Failure = msg
		} else {
			obs.Failure += "; " + msg
		}
		failures = append(failures, msg)
	}
	capture := func(key, filename string) {
		shotCtx, cancel := context.WithTimeout(ctx, cfg.ScreenshotTimeout)
		defer cancel()
		data, err := drv.CaptureScreenshot(shotCtx)
		if err == nil {
			err = os.WriteFile(filepath.Join(cfg.ArtifactDir, filename), data, 0600)
		}
		if err == nil {
			obs.Screenshots[key] = screenshotObservation{File: filename, Status: "ok"}
			return
		}
		status := "failed"
		if errors.Is(err, context.DeadlineExceeded) {
			status = "timeout"
		}
		redacted := redactCheckpointSecret(err.Error(), cfg.Cookie)
		obs.Screenshots[key] = screenshotObservation{File: filename, Status: status, Error: redacted}
		if cfg.ScreenshotRequired {
			recordFailure("screenshot "+filename+" "+status, err)
		}
	}

	greeting, err := drv.CheckGreeting(ctx)
	obs.Greeting = greeting
	if err != nil {
		recordFailure("external greeting checkpoint failed", err)
		if browserErrors != nil {
			obs.Errors = browserErrors()
		}
		obs.Passed = false
		return obs, failures
	}
	if obs.Greeting != cfg.ExpectedGreeting {
		failures = append(failures, "rendered greeting differs from expected")
		if obs.Failure == "" {
			obs.Failure = "rendered greeting differs from expected"
		}
	}
	capture("page", "page.png")

	activeUI, err := drv.CheckActiveUI(ctx)
	obs.ActiveUI = activeUI
	if err != nil {
		recordFailure("external active-version UI checkpoint failed", err)
		if browserErrors != nil {
			obs.Errors = browserErrors()
		}
		obs.Passed = false
		return obs, failures
	}
	capture("shape", "shape.png")

	if browserErrors != nil {
		obs.Errors = browserErrors()
	}
	if len(obs.Errors) != 0 {
		failures = append(failures, "browser reported errors; inspect private checkpoint observation")
		if obs.Failure == "" {
			obs.Failure = "browser reported errors; inspect private checkpoint observation"
		}
	}
	obs.Passed = len(failures) == 0
	return obs, failures
}
