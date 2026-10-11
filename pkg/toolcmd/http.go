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
	h := cmd.HTTP
	// Binding is decided entirely before the token is read or a connection
	// opened: a missing, extra or out-of-range argument never reaches the wire.
	payload, err := httpPayload(*h, args, opts.Argv)
	if err != nil {
		return finish(OutcomeError, fmt.Errorf("toolcmd: %s:%s: %w", tool.Name, cmd.Name, err))
	}
	if opts.DryRun {
		return finish(OutcomeDryRun, nil)
	}
	token, err := httpToken(*h)
	if err != nil {
		return finish(OutcomeError, fmt.Errorf("toolcmd: %s:%s: %w", tool.Name, cmd.Name, err))
	}
	body, err := json.Marshal(payload)
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
	// A configured loopback endpoint must not be able to forward its bearer
	// token or command body to a different host through an HTTP redirect.
	client := *httpClient(CommandTimeout(cmd, opts))
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
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

// httpPayload builds the JSON body: the declared literal body with either
// {args} substituted (no http.args) or each bound argument set as its own
// typed value at its field (http.args).
func httpPayload(h fleet.ToolCommandHTTPAction, args string, argv []string) (map[string]any, error) {
	if len(h.Args) == 0 {
		return expandHTTPBody(h.Body, args), nil
	}
	if argv == nil {
		words, err := SplitArgs(args)
		if err != nil {
			return nil, err
		}
		argv = words
	}
	bound, err := BindHTTPArgs(h.Args, argv)
	if err != nil {
		return nil, err
	}
	body, _ := expandHTTPValue(h.Body, "").(map[string]any) // deep copy; validated bodies hold no {args}
	if body == nil {
		body = map[string]any{}
	}
	for _, a := range h.Args {
		v, ok := bound[a.Name]
		if !ok {
			continue
		}
		cur := body
		segs := strings.Split(a.BodyField(), ".")
		for _, seg := range segs[:len(segs)-1] {
			next, ok := cur[seg].(map[string]any)
			if !ok {
				next = map[string]any{}
				cur[seg] = next
			}
			cur = next
		}
		cur[segs[len(segs)-1]] = v
	}
	return body, nil
}

// BindHTTPArgs binds caller words to declared arguments and projects each to
// its JSON value. A word is --name VALUE, --name=VALUE, or positional; the
// positionals fill the arguments not bound by name, in declaration order. A
// bare "--" ends option parsing. Unknown names, repeats, surplus positionals,
// missing required arguments and out-of-range values are errors.
func BindHTTPArgs(decl []fleet.ToolCommandHTTPArg, argv []string) (map[string]any, error) {
	byName := make(map[string]fleet.ToolCommandHTTPArg, len(decl))
	for _, a := range decl {
		byName[a.Name] = a
	}
	raw := map[string]string{}
	var positional []string
	for i := 0; i < len(argv); i++ {
		w := argv[i]
		if w == "--" {
			positional = append(positional, argv[i+1:]...)
			break
		}
		if !strings.HasPrefix(w, "--") {
			positional = append(positional, w)
			continue
		}
		name, val, hasVal := strings.Cut(w[2:], "=")
		if _, ok := byName[name]; !ok {
			return nil, fmt.Errorf("unknown argument --%s", name)
		}
		if !hasVal {
			if i+1 >= len(argv) {
				return nil, fmt.Errorf("argument --%s needs a value", name)
			}
			i++
			val = argv[i]
		}
		if _, dup := raw[name]; dup {
			return nil, fmt.Errorf("argument %s given more than once", name)
		}
		raw[name] = val
	}
	for _, a := range decl {
		if len(positional) == 0 {
			break
		}
		if _, ok := raw[a.Name]; !ok {
			raw[a.Name], positional = positional[0], positional[1:]
		}
	}
	if len(positional) > 0 {
		return nil, fmt.Errorf("%d unexpected extra argument(s) starting at %q", len(positional), positional[0])
	}
	out := make(map[string]any, len(decl))
	for _, a := range decl {
		v, ok := raw[a.Name]
		if !ok {
			if a.Required {
				return nil, fmt.Errorf("missing required argument %s", a.Name)
			}
			if a.Default == "" {
				continue
			}
			v = a.Default
		}
		p, err := a.Project(v)
		if err != nil {
			return nil, fmt.Errorf("argument %s: %w", a.Name, err)
		}
		out[a.Name] = p
	}
	return out, nil
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
