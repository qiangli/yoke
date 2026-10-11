package toolcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/secrets"
)

var httpClient = func(timeout time.Duration) *http.Client { return &http.Client{Timeout: timeout} }

func runHTTP(ctx context.Context, tool fleet.Tool, cmd fleet.ToolCommand, args string, opts Options) (Result, error) {
	started := time.Now()
	res := resultFor(tool, cmd)
	res.Started = started
	finish := func(outcome string, err error) (Result, error) {
		res.Outcome, res.Duration = outcome, time.Since(started).Round(time.Millisecond).String()
		if err != nil {
			res.Error = err.Error()
		}
		return res, err
	}
	if opts.DryRun {
		return finish(OutcomeDryRun, nil)
	}
	h := cmd.HTTP
	token, err := httpToken(*h)
	if err != nil {
		return finish(OutcomeError, fmt.Errorf("toolcmd: %s:%s: %w", tool.Name, cmd.Name, err))
	}
	body, err := json.Marshal(expandHTTPBody(h.Body, args))
	if err != nil {
		return finish(OutcomeError, fmt.Errorf("toolcmd: %s:%s: encode JSON body: %w", tool.Name, cmd.Name, err))
	}
	target, err := httpTarget(*h)
	if err != nil {
		return finish(OutcomeError, fmt.Errorf("toolcmd: %s:%s: %w", tool.Name, cmd.Name, err))
	}
	runCtx, cancel := context.WithTimeout(ctx, CommandTimeout(cmd, opts))
	defer cancel()
	req, err := http.NewRequestWithContext(runCtx, strings.ToUpper(h.Method), target, bytes.NewReader(body))
	if err != nil {
		return finish(OutcomeError, fmt.Errorf("toolcmd: %s:%s: request: %w", tool.Name, cmd.Name, err))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := httpClient(CommandTimeout(cmd, opts)).Do(req)
	if err != nil {
		if runCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
			return finish(OutcomeTimeout, fmt.Errorf("toolcmd: %s:%s: timed out after %s", tool.Name, cmd.Name, CommandTimeout(cmd, opts)))
		}
		if ctx.Err() != nil {
			return finish(OutcomeCancelled, fmt.Errorf("toolcmd: %s:%s: %w", tool.Name, cmd.Name, ctx.Err()))
		}
		return finish(OutcomeError, fmt.Errorf("toolcmd: %s:%s: HTTP request failed", tool.Name, cmd.Name))
	}
	defer resp.Body.Close()
	res.HTTPStatus = resp.StatusCode
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if readErr != nil {
		return finish(OutcomeError, fmt.Errorf("toolcmd: %s:%s: read HTTP response: %w", tool.Name, cmd.Name, readErr))
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return finish(OutcomeError, fmt.Errorf("toolcmd: %s:%s: HTTP status %d", tool.Name, cmd.Name, resp.StatusCode))
	}
	res.Text = string(data)
	return finish(OutcomeSuccess, nil)
}

func httpTarget(h fleet.ToolCommandHTTPAction) (string, error) {
	u, err := url.Parse(h.URL)
	if err != nil {
		return "", err
	}
	if h.Path != "" {
		u.Path = strings.TrimRight(u.Path, "/") + h.Path
	}
	return u.String(), nil
}

func httpToken(h fleet.ToolCommandHTTPAction) (string, error) {
	if h.TokenFile != "" {
		b, err := os.ReadFile(h.TokenFile)
		if err != nil {
			return "", fmt.Errorf("read token file: %w", err)
		}
		if token := strings.TrimSpace(string(b)); token != "" {
			return token, nil
		}
		return "", fmt.Errorf("token file is empty")
	}
	kv, ok := secrets.ResolveAgentKey(os.Environ(), h.TokenRef)
	if !ok {
		return "", fmt.Errorf("token ref %q is unavailable", h.TokenRef)
	}
	_, token, _ := strings.Cut(kv, "=")
	if strings.TrimSpace(token) == "" {
		return "", fmt.Errorf("token ref %q is empty", h.TokenRef)
	}
	return token, nil
}

func expandHTTPBody(body map[string]any, args string) map[string]any {
	if body == nil {
		return nil
	}
	return expandHTTPValue(body, args).(map[string]any)
}

func expandHTTPValue(v any, args string) any {
	switch x := v.(type) {
	case string:
		return strings.ReplaceAll(x, "{args}", args)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, v := range x {
			out[k] = expandHTTPValue(v, args)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = expandHTTPValue(v, args)
		}
		return out
	default:
		return v
	}
}
