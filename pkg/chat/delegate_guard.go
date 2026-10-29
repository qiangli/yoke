package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// DelegateError is a machine-readable failure of a bounded delegation.
type DelegateError struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

func (e *DelegateError) Error() string           { return e.Message }
func delegateFailure(kind, message string) error { return &DelegateError{kind, message} }

type delegateKey struct{}
type delegateGuard struct {
	allow       string
	genie       bool
	idle        time.Duration
	capRequired bool
	identity    string
}

func delegation(ctx context.Context) *delegateGuard {
	g, _ := ctx.Value(delegateKey{}).(*delegateGuard)
	return g
}

func (g *delegateGuard) check(l Launch) error {
	model := l.ModelName
	if model == "" {
		model = l.Model
	}
	target := l.ToolName + ":" + model
	if g.genie {
		g.identity = "genie-delegate:" + l.ToolName + "-" + model
	}
	allowed := strings.TrimSpace(g.allow) == "" && !g.genie
	for _, entry := range strings.Split(g.allow, ",") {
		if strings.TrimSpace(entry) == target {
			allowed = true
		}
	}
	if !allowed {
		return delegateFailure("denied", "delegate: target "+target+" is not allowed")
	}
	// CLI tool loops are opaque. An output-text estimate or a final usage event
	// cannot enforce a hard ceiling on intermediate calls. Refuse BEFORE launch.
	if g.capRequired {
		return delegateFailure("budget", "delegate: required dollar/token cap cannot be enforced by this vendor adapter")
	}
	if len(l.PreserveEnv) != 0 || len(l.CredentialEnvAliases) != 0 {
		return delegateFailure("denied", "delegate: metered credential bindings are not permitted")
	}
	return nil
}

// Defense in depth AFTER launch environment overlays: no recipe may restore a
// metered credential or select a cloud billing backend. Vendor login state is
// queried through its CLI, never by opening a credential file in this process.
func scrubDelegateEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		k := strings.ToUpper(key)
		if strings.HasSuffix(k, "_API_KEY") || strings.HasSuffix(k, "_AUTH_TOKEN") ||
			strings.HasPrefix(k, "AWS_") || strings.HasPrefix(k, "AZURE_") ||
			strings.HasPrefix(k, "GOOGLE_") || strings.HasPrefix(k, "CLOUD_ML_") ||
			strings.HasPrefix(k, "CLAUDE_CODE_USE_") || k == "ANTHROPIC_BASE_URL" ||
			k == "OPENAI_BASE_URL" || k == "OPENAI_API_BASE" || k == "OPENAI_ORG_ID" {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// Auth checks use the same binary and scrubbed environment as the task. Their
// output is deliberately not returned: status errors can contain account data.
func delegateAuth(ctx context.Context, agent, cwd string, l Launch) error {
	var args []string
	switch l.ToolName {
	case "claude":
		args = []string{"auth", "status", "--json"}
	case "codex":
		args = []string{"login", "status"}
	default:
		return delegateFailure("auth", "delegate: no subscription login check for "+l.ToolName)
	}
	args = append(delegateSubscriptionArgs(l.ToolName), args...)
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := agentCommand(probeCtx, agent, args, cwd)
	setProcessGroup(cmd)
	cmd.Cancel = nil
	var out boundedAuthOutput
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return delegateFailure("auth", "delegate: cannot check "+l.ToolName+" login; log in with the vendor CLI")
	}
	err := waitForProcessTree(probeCtx, cmd, func() error { return killProcessTree(cmd) })
	if ctx.Err() != nil {
		return ctx.Err()
	}
	authenticated := false
	if err == nil {
		switch l.ToolName {
		case "claude":
			var status struct {
				LoggedIn         bool   `json:"loggedIn"`
				AuthMethod       string `json:"authMethod"`
				SubscriptionType string `json:"subscriptionType"`
			}
			if json.Unmarshal(out.data, &status) == nil {
				authenticated = status.LoggedIn && status.AuthMethod == "oauth_token" && status.SubscriptionType != ""
			}
		case "codex":
			authenticated = strings.TrimSpace(string(out.data)) == "Logged in using ChatGPT"
		}
	}
	if !authenticated {
		return delegateFailure("auth", "delegate: "+l.ToolName+" is not logged in with a subscription; log in with the vendor CLI (no metered fallback)")
	}
	return nil
}

// os/exec serializes writes when stdout and stderr share this writer.
type boundedAuthOutput struct{ data []byte }

func (b *boundedAuthOutput) Write(p []byte) (int, error) {
	n := len(p)
	if remaining := 16384 - len(b.data); remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		b.data = append(b.data, p...)
	}
	return n, nil
}

type activityWriter struct {
	dst  io.Writer
	last *atomic.Int64
}

func (w activityWriter) Write(p []byte) (int, error) {
	if len(p) > 0 {
		w.last.Store(time.Now().UnixNano())
	}
	return w.dst.Write(p)
}

// Match agentpty.IdleTimeout: either output stream counts as activity; input
// does not. Cancellation uses the existing runner's process-tree teardown.
func watchDelegateIdle(ctx context.Context, cmdOut, cmdErr io.Writer, idle time.Duration, started <-chan struct{}) (context.Context, io.Writer, io.Writer, func()) {
	ctx, cancel := context.WithCancelCause(ctx)
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	stop := make(chan struct{})
	interval := idle / 4
	if interval <= 0 {
		interval = time.Nanosecond
	}
	go func() {
		select {
		case <-started:
			last.Store(time.Now().UnixNano())
		case <-stop:
			return
		case <-ctx.Done():
			return
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if time.Since(time.Unix(0, last.Load())) >= idle {
					cancel(delegateFailure("idle", fmt.Sprintf("delegate: no output for %s", idle)))
					return
				}
			}
		}
	}()
	return ctx, activityWriter{cmdOut, &last}, activityWriter{cmdErr, &last}, func() { close(stop); cancel(nil) }
}

func finishDelegate(res Result, err error, g *delegateGuard) (delegateResult, error) {
	if err == nil && res.ExitCode != 0 {
		err = fmt.Errorf("delegate: vendor exited %d", res.ExitCode)
	}
	result := delegateResult{Result: res, Receipt: os.Getenv("BASHY_ATTEST_ID"), Identity: g.identity, Exit: res.ExitCode}
	if err != nil {
		var failure *DelegateError
		if !errors.As(err, &failure) {
			kind := "vendor"
			if errors.Is(err, context.DeadlineExceeded) {
				kind = "timeout"
			}
			failure = &DelegateError{kind, err.Error()}
		}
		result.Error = failure
		if result.Exit == 0 {
			result.Exit = 1
		}
		result.ExitCode = result.Exit
	}
	return result, err
}

type delegateResult struct {
	Result
	Receipt  string         `json:"receipt"`
	Identity string         `json:"identity"`
	Exit     int            `json:"exit"`
	Error    *DelegateError `json:"error,omitempty"`
}

// Pin the login lane for the task as well as the probe. Scrubbing the process
// environment alone is insufficient when a vendor loads a provider or key
// helper from its settings. These are documented vendor CLI/config switches.
func delegateSubscriptionArgs(tool string) []string {
	switch tool {
	case "claude":
		return []string{"--setting-sources", "", "--settings", `{"forceLoginMethod":"claudeai","apiKeyHelper":""}`}
	case "codex":
		return []string{"-c", `forced_login_method="chatgpt"`, "-c", `model_provider="openai"`}
	}
	return nil
}

func delegateBudgetFailure(err error) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return err
	}
	return delegateFailure("budget", err.Error())
}
