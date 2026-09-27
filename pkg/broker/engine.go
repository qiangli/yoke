package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The engine is the local model server the device queue fronts: bashy's own
// Ollama, started by the broker on a private loopback port nobody else is
// told about. Clients only ever see the broker.

// Engine starts and stops the local model server.
type Engine interface {
	// Start brings the engine up and returns its base URL (http://host:port).
	Start(ctx context.Context) (string, error)
	Stop(ctx context.Context) error
}

// StaticEngine is an already-running engine at a fixed URL (tests, or an
// operator who runs the engine separately).
type StaticEngine string

func (e StaticEngine) Start(context.Context) (string, error) {
	return strings.TrimRight(string(e), "/"), nil
}
func (StaticEngine) Stop(context.Context) error { return nil }

// EngineModeEnv marks a child process as the raw engine: `bashy ollama serve`
// with it set runs Ollama itself instead of the broker.
const EngineModeEnv = "BASHY_OLLAMA_ENGINE"

// ExecEngine runs the engine as a child process: Argv (typically
// [bashy ollama serve]) with EngineModeEnv=1 and OLLAMA_HOST set to a free
// loopback port. NUM_PARALLEL=1 makes the engine itself exclusive, matching
// the device model; Env adds or overrides variables (e.g. the context length).
type ExecEngine struct {
	Argv []string
	Env  []string
	Log  io.Writer

	mu   sync.Mutex
	cmd  *exec.Cmd
	done chan struct{}
}

func (e *ExecEngine) Start(ctx context.Context) (string, error) {
	if len(e.Argv) == 0 {
		return "", errors.New("broker: engine command is empty")
	}
	port, err := freePort()
	if err != nil {
		return "", err
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	cmd := exec.Command(e.Argv[0], e.Argv[1:]...)
	env := append(os.Environ(),
		EngineModeEnv+"=1",
		"OLLAMA_HOST="+addr,
		"OLLAMA_NUM_PARALLEL=1",
	)
	cmd.Env = append(env, e.Env...)
	if e.Log != nil {
		cmd.Stdout, cmd.Stderr = e.Log, e.Log
	}
	cmd.SysProcAttr = engineSysProcAttr()
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("broker: start engine: %w", err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	e.mu.Lock()
	e.cmd, e.done = cmd, done
	e.mu.Unlock()
	base := "http://" + addr
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			return "", errors.New("broker: engine exited during start-up (see the engine log)")
		case <-ctx.Done():
			_ = e.Stop(context.Background())
			return "", ctx.Err()
		default:
		}
		if engineUp(base) {
			return base, nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	_ = e.Stop(context.Background())
	return "", errors.New("broker: engine did not answer within 90s")
}

func (e *ExecEngine) Stop(ctx context.Context) error {
	e.mu.Lock()
	cmd, exited := e.cmd, e.done
	e.cmd, e.done = nil, nil
	e.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
	case <-ctx.Done():
		_ = cmd.Process.Kill()
	}
	return nil
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("broker: pick an engine port: %w", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func engineUp(base string) bool {
	c := http.Client{Timeout: 500 * time.Millisecond}
	resp, err := c.Get(base + "/api/version")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// engineClient talks to the engine's native API for the broker's own needs
// (inventory, residency, derived models). It never serves client traffic.
type engineClient struct {
	base string
	http *http.Client
}

type engineModel struct {
	Name   string `json:"name"`
	Model  string `json:"model"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
}

func (c engineClient) tags(ctx context.Context) ([]engineModel, error) {
	var out struct {
		Models []engineModel `json:"models"`
	}
	if err := c.getJSON(ctx, "/api/tags", &out); err != nil {
		return nil, err
	}
	return out.Models, nil
}

func (c engineClient) ps(ctx context.Context) ([]engineModel, error) {
	var out struct {
		Models []engineModel `json:"models"`
	}
	if err := c.getJSON(ctx, "/api/ps", &out); err != nil {
		return nil, err
	}
	return out.Models, nil
}

// unload evicts a resident model (keep_alive 0), dropping its KV/prefix cache.
func (c engineClient) unload(ctx context.Context, model string) error {
	return c.postJSON(ctx, "/api/generate", map[string]any{"model": model, "keep_alive": 0}, nil)
}

// create makes a derived model that shares the base's weights and carries
// default parameters (e.g. num_ctx, which the OpenAI-compatible surface
// cannot pass per request).
func (c engineClient) create(ctx context.Context, name, from string, params map[string]any) error {
	return c.postJSON(ctx, "/api/create", map[string]any{
		"model": name, "from": from, "parameters": params, "stream": false,
	}, nil)
}

func (c engineClient) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	return c.do(req, out)
}

func (c engineClient) postJSON(ctx context.Context, path string, body, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, out)
}

func (c engineClient) do(req *http.Request, out any) error {
	hc := c.http
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Minute}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("engine %s: HTTP %d: %s", req.URL.Path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out != nil {
		// Streamed endpoints answer NDJSON; the last object is the result.
		lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
		return json.Unmarshal(lines[len(lines)-1], out)
	}
	return nil
}
