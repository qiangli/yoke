package weave

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/qiangli/yoke/pkg/secrets"
)

// weaveResultPlainTailLines is the maximum number of lines preserved when extracting
// a final message from plain-text output.
const weaveResultPlainTailLines = 50

// weaveResultTracker collects output from an agent run and extracts the final
// message/report across Claude stream-json, Codex exec, and plain-text tools.
type weaveResultTracker struct {
	mu              sync.Mutex
	claudeResult    string
	claudeAssistant string
	codexMessage    string
	plainLines      []string
}

func newWeaveResultTracker() *weaveResultTracker {
	return &weaveResultTracker{}
}

// Observe feeds one line of worker stdout or PTY output.
func (t *weaveResultTracker) Observe(rawLine string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	// Strip ANSI escape codes and line terminators.
	clean := weaveANSIEscape.ReplaceAllString(rawLine, "")
	clean = strings.TrimRight(clean, "\r\n")
	if idx := strings.LastIndexByte(clean, '\r'); idx >= 0 {
		clean = clean[idx+1:]
	}

	// Always track plain-text lines in a bounded buffer.
	t.plainLines = append(t.plainLines, clean)
	if len(t.plainLines) > 1000 {
		t.plainLines = t.plainLines[len(t.plainLines)-1000:]
	}

	// Inspect for JSON stream events.
	trimmed := strings.TrimSpace(clean)
	i := strings.IndexByte(trimmed, '{')
	if i < 0 {
		return
	}
	j := strings.LastIndexByte(trimmed, '}')
	if j <= i {
		return
	}
	jsonBytes := []byte(trimmed[i : j+1])

	var obj map[string]any
	if err := json.Unmarshal(jsonBytes, &obj); err != nil {
		return
	}

	typ, _ := obj["type"].(string)

	// Claude stream-json: final {"type":"result"} event carries the result field.
	if typ == "result" {
		if r, ok := obj["result"].(string); ok && strings.TrimSpace(r) != "" {
			t.claudeResult = r
		} else if rVal, ok := obj["result"]; ok && rVal != nil {
			if b, err := json.Marshal(rVal); err == nil {
				t.claudeResult = string(b)
			}
		}
		return
	}

	// Claude assistant message: track as fallback if no result event arrives.
	if typ == "assistant" {
		if text := extractClaudeAssistantText(obj); text != "" {
			t.claudeAssistant = text
		}
	}

	// Codex exec: item.completed or item.updated with item.type == agent_message.
	if msg := extractCodexAgentMessage(obj); msg != "" {
		t.codexMessage = msg
	}
}

// ObserveAll feeds a multi-line string of output.
func (t *weaveResultTracker) ObserveAll(output string) {
	if t == nil {
		return
	}
	for _, line := range strings.Split(output, "\n") {
		t.Observe(line)
	}
}

// Result returns the extracted final message.
func (t *weaveResultTracker) Result() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.claudeResult != "" {
		return t.claudeResult
	}
	if t.codexMessage != "" {
		return t.codexMessage
	}
	if t.claudeAssistant != "" {
		return t.claudeAssistant
	}

	// Other tools: the last N lines of cleaned (ANSI-stripped) output.
	lines := t.plainLines
	if len(lines) > weaveResultPlainTailLines {
		lines = lines[len(lines)-weaveResultPlainTailLines:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

// extractClaudeAssistantText pulls text out of Claude's assistant message event.
func extractClaudeAssistantText(obj map[string]any) string {
	var texts []string
	if msg, ok := obj["message"].(map[string]any); ok {
		if content, ok := msg["content"].([]any); ok {
			for _, item := range content {
				if m, ok := item.(map[string]any); ok && m["type"] == "text" {
					if txt, ok := m["text"].(string); ok && strings.TrimSpace(txt) != "" {
						texts = append(texts, txt)
					}
				}
			}
		}
	}
	if content, ok := obj["content"].([]any); ok {
		for _, item := range content {
			if m, ok := item.(map[string]any); ok && m["type"] == "text" {
				if txt, ok := m["text"].(string); ok && strings.TrimSpace(txt) != "" {
					texts = append(texts, txt)
				}
			}
		}
	}
	return strings.Join(texts, "\n")
}

// extractCodexAgentMessage pulls text out of Codex's agent_message item event.
func extractCodexAgentMessage(obj map[string]any) string {
	if item, ok := obj["item"].(map[string]any); ok {
		if typ, _ := item["type"].(string); typ == "agent_message" {
			for _, key := range []string{"text", "content", "message"} {
				if s, ok := item[key].(string); ok && strings.TrimSpace(s) != "" {
					return s
				}
			}
		}
	}
	if typ, _ := obj["type"].(string); typ == "agent_message" {
		for _, key := range []string{"text", "content", "message"} {
			if s, ok := obj[key].(string); ok && strings.TrimSpace(s) != "" {
				return s
			}
		}
	}
	return ""
}

// weaveExtractResult extracts an agent's final report or message from recorded output.
func weaveExtractResult(output string) string {
	t := newWeaveResultTracker()
	t.ObserveAll(output)
	return t.Result()
}

// weaveResultLogPath returns the expected path to logs/issue-N.result.md next to the PTY log.
func weaveResultLogPath(dir string, issueID int64, logPath string) string {
	if logPath != "" && strings.HasSuffix(logPath, ".log") {
		return strings.TrimSuffix(logPath, ".log") + ".result.md"
	}
	return filepath.Join(dir, "logs", fmt.Sprintf("issue-%d.result.md", issueID))
}

// weaveWriteRunResultFile persists the run's final report to logs/issue-N.result.md.
func weaveWriteRunResultFile(dir string, issueID int64, logPath string, content string, secretsRendered bool, redactor *secrets.Redactor) (string, error) {
	if dir == "" {
		return "", errors.New("missing queue dir")
	}
	resultPath := weaveResultLogPath(dir, issueID, logPath)
	logsDir := filepath.Dir(resultPath)
	dirMode := os.FileMode(0o755)
	fileMode := os.FileMode(0o644)
	if secretsRendered {
		dirMode = 0o700
		fileMode = 0o600
	}
	if err := os.MkdirAll(logsDir, dirMode); err != nil {
		return "", fmt.Errorf("create result dir: %w", err)
	}
	if secretsRendered {
		_ = os.Chmod(logsDir, dirMode)
	}
	if redactor != nil && len(content) > 0 {
		content = string(redactor.Redact([]byte(content)))
	}
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	if err := os.WriteFile(resultPath, []byte(content), fileMode); err != nil {
		return "", fmt.Errorf("write result file: %w", err)
	}
	if secretsRendered && runtime.GOOS != "windows" {
		_ = os.Chmod(resultPath, fileMode)
	}
	return resultPath, nil
}

// weaveEnsureRunResultSaved checks if issue-N.result.md exists on disk and creates it
// from the run log if not already saved.
func weaveEnsureRunResultSaved(dir string, it *weaveItem, redactor *secrets.Redactor, secretsRendered bool) string {
	if it == nil || dir == "" {
		return ""
	}
	resultPath := weaveResultLogPath(dir, it.ID, it.LogPath)
	if _, err := os.Stat(resultPath); err == nil {
		return resultPath
	}
	logPath := it.LogPath
	if logPath == "" {
		conventional := filepath.Join(dir, "logs", fmt.Sprintf("issue-%d.log", it.ID))
		if _, err := os.Stat(conventional); err == nil {
			logPath = conventional
		}
	}
	if logPath == "" {
		return ""
	}
	b, err := os.ReadFile(logPath)
	if err != nil {
		return ""
	}
	res := weaveExtractResult(string(b))
	saved, _ := weaveWriteRunResultFile(dir, it.ID, logPath, res, secretsRendered, redactor)
	return saved
}
