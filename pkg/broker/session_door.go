package broker

// The session door: CLIs whose subscription login lives in the user's login
// session (the macOS keychain: claude, muse, agy) cannot run under a boot
// service, which has no access to that session. The system door stays the one
// endpoint and keeps serving everything else in-process (local models, API
// models, file-login CLIs); it forwards only those bindings to a door the user
// started in the login session (`bashy llm up --session`). Found Sprint 738.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/broker/door"
	"github.com/qiangli/yoke/pkg/fleet"
)

// SessionDoorEnv marks a door started by `llm up --session`: it serves the
// session-bound CLIs itself, never forwards, and never starts a local engine.
const SessionDoorEnv = "BASHY_LLM_SESSION_DOOR"

// BackendSession names the forwarded route in records and headers.
const BackendSession = "cligw-session"

// SessionDoor decides which tools need the user's login session and where the
// session door answers. Options.Session nil disables forwarding.
type SessionDoor interface {
	NeedsSession(tool string) bool
	Upstream(ctx context.Context) (baseURL, key string, ok bool)
}

// LocalSessionDoor reads the fleet's tool definitions and the session door's
// own state under <bashy home>/session.
type LocalSessionDoor struct {
	tool func(name string) (fleet.Tool, bool) // nil = fleet.New().Tool
}

func (s *LocalSessionDoor) NeedsSession(name string) bool {
	lookup := s.tool
	if lookup == nil {
		lookup = fleet.New().Tool
	}
	t, ok := lookup(name)
	return ok && t.CLI.Launch.Auth.NeedsLoginSession(runtime.GOOS)
}

func (s *LocalSessionDoor) Upstream(ctx context.Context) (string, string, bool) {
	home, err := door.SessionHome()
	if err != nil {
		return "", "", false
	}
	tok, err := os.ReadFile(filepath.Join(home, "cligw", "token"))
	if err != nil || strings.TrimSpace(string(tok)) == "" {
		return "", "", false
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", door.SessionPort())
	cctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, base+"/health", nil)
	if err != nil {
		return "", "", false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", false
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusUnauthorized {
		return "", "", false
	}
	return base + "/v1", strings.TrimSpace(string(tok)), true
}

// sessionUpstream resolves model to its agent; found is true when that agent's
// tool needs the login session. err means it does but no session door answers.
func (b *Broker) sessionUpstream(ctx context.Context, model string) (APIUpstream, bool, error) {
	info, err := b.opts.CLI.ResolveAgent(ctx, model, "")
	if err != nil || !b.opts.Session.NeedsSession(info.Tool) {
		return APIUpstream{}, false, nil // the CLI path routes or reports it
	}
	base, key, ok := b.opts.Session.Upstream(ctx)
	if !ok {
		return APIUpstream{}, true, fmt.Errorf("%s runs %s, whose login lives in your login session, and no session door answers: start it in your login session with `bashy llm up --session`", info.Name, info.Tool)
	}
	return APIUpstream{BaseURL: base, ModelID: info.Name, Key: key}, true, nil
}
