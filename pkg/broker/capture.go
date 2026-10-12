package broker

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/execlog"
	"github.com/qiangli/yoke/pkg/redact"
)

const captureLimit = 4 << 20

type boundedBody struct {
	data      []byte
	truncated bool
}

func (b *boundedBody) Write(p []byte) (int, error) {
	n := len(p)
	room := captureLimit - len(b.data)
	if n > room {
		b.truncated = true
		p = p[:room]
	}
	b.data = append(b.data, p...)
	return n, nil
}

type teeReadCloser struct {
	io.ReadCloser
	dst io.Writer
}

func (t *teeReadCloser) Read(p []byte) (int, error) {
	n, e := t.ReadCloser.Read(p)
	if n > 0 {
		_, _ = t.dst.Write(p[:n])
	}
	return n, e
}

type captureUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}
type captureTurn struct {
	SchemaVersion string        `json:"schema_version"`
	At            time.Time     `json:"at"`
	Session       string        `json:"session"`
	Episode       string        `json:"episode"`
	Dialect       string        `json:"dialect"`
	Path          string        `json:"path"`
	Model         string        `json:"model"`
	Status        int           `json:"status"`
	Usage         *captureUsage `json:"usage,omitempty"`
	Routed        string        `json:"routed"`
	Request       string        `json:"request"`
	Response      string        `json:"response"`
	Truncated     bool          `json:"truncated"`
	CaptureLevel  string        `json:"capture_level"`
}

var captureMu sync.Mutex
var episodeCache sync.Map

func captureRoot() (string, error) {
	h := strings.TrimSpace(os.Getenv("BASHY_HOME"))
	if h == "" {
		var err error
		h, err = os.UserHomeDir()
		if err != nil {
			return "", err
		}
		h = filepath.Join(h, ".bashy")
	}
	return filepath.Join(h, "cligw", "capture"), nil
}
func captureSessionDir(session string) (string, error) {
	if session == "" {
		session = "_nosession"
	} else if err := ValidateSession(session); err != nil {
		return "", err
	}
	root, err := captureRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, session), nil
}
func writeCaptureSession(session, episode string) error {
	dir, err := captureSessionDir(session)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	b, err := json.Marshal(struct {
		Session string    `json:"session"`
		Episode string    `json:"episode"`
		At      time.Time `json:"at"`
	}{session, episode, time.Now().UTC()})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "session.json"), b, 0600)
}
func captureEpisode(session string) string {
	if session == "" {
		return ""
	}
	dir, err := captureSessionDir(session)
	if err != nil {
		return ""
	}
	if v, ok := episodeCache.Load(dir); ok {
		return v.(string)
	}
	b, err := os.ReadFile(filepath.Join(dir, "session.json"))
	if err != nil {
		return ""
	}
	var row struct {
		Episode string `json:"episode"`
	}
	if json.Unmarshal(b, &row) == nil {
		episodeCache.Store(dir, row.Episode)
	}
	return row.Episode
}
func captureDialect(path string) string {
	switch {
	case strings.HasPrefix(path, "/anthropic/"):
		return "anthropic"
	case strings.HasPrefix(path, "/api/"):
		return "ollama"
	case strings.HasSuffix(path, "/responses"):
		return "responses"
	default:
		return "openai"
	}
}
func captureLevel(body []byte) string {
	check := func(raw []byte) bool {
		var v map[string]any
		if json.Unmarshal(raw, &v) != nil {
			return false
		}
		if ids, ok := v["prompt_token_ids"].([]any); ok && len(ids) > 0 && v["logprobs"] != nil {
			return true
		}
		if ids, ok := v["token_ids"].([]any); ok && len(ids) > 0 && v["logprobs"] != nil {
			return true
		}
		choices, _ := v["choices"].([]any)
		for _, c := range choices {
			choice, _ := c.(map[string]any)
			lp, _ := choice["logprobs"].(map[string]any)
			content, _ := lp["content"].([]any)
			for _, item := range content {
				entry, _ := item.(map[string]any)
				_, hasID := entry["token_id"]
				if !hasID {
					_, hasID = entry["token_ids"]
				}
				_, hasLP := entry["logprob"]
				if hasID && hasLP {
					return true
				}
			}
		}
		return false
	}
	if check(body) {
		return "train"
	}
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(bytes.TrimPrefix(bytes.TrimSpace(line), []byte("data:")))
		if check(line) {
			return "train"
		}
	}
	return "eval"
}
func captureRedact(s string) string { return redact.FromHost().String(execlog.MaskSecrets(s)) }
func (b *Broker) appendCapture(ri *reqInfo, path string, req *boundedBody, cw *captureWriter) {
	dir, err := captureSessionDir(ri.session)
	if err != nil {
		return
	}
	var payload struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(req.data, &payload)
	model := ri.rec.Model
	if model == "" {
		model = payload.Model
	}
	usage := (*captureUsage)(nil)
	if ri.rec.PromptTok != 0 || ri.rec.OutputTok != 0 {
		usage = &captureUsage{ri.rec.PromptTok, ri.rec.OutputTok}
	}
	row := captureTurn{"bashy-capture-v1", ri.started.UTC(), ri.session, captureEpisode(ri.session), captureDialect(path), path, captureRedact(model), cw.status, usage, captureRedact(cw.Header().Get("X-Bashy-Routed")), captureRedact(string(req.data)), captureRedact(string(cw.full)), req.truncated || cw.truncated, captureLevel(cw.full)}
	data, err := json.Marshal(row)
	if err != nil {
		return
	}
	captureMu.Lock()
	defer captureMu.Unlock()
	if os.MkdirAll(dir, 0700) != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, "turns.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	_, _ = f.Write(append(data, '\n'))
	_ = f.Close()
}
func readCaptureTurns(session string) ([]captureTurn, error) {
	dir, err := captureSessionDir(session)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(dir, "turns.jsonl"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 4096), 32*captureLimit)
	var turns []captureTurn
	for s.Scan() {
		var t captureTurn
		if err = json.Unmarshal(s.Bytes(), &t); err != nil {
			return nil, fmt.Errorf("capture: invalid turn: %w", err)
		}
		turns = append(turns, t)
	}
	return turns, s.Err()
}
