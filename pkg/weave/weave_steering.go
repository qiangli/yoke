package weave

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/agentpty"
	"github.com/qiangli/yoke/pkg/bus"
)

// The presence card carrying the socket must name the same immutable instance
// as mailbox delivery. Keep the model binding separate from this identity.
func weaveSteeringPrincipal(it *weaveItem) string {
	if it.Instance != "" {
		return "instance:" + it.Instance
	}
	if it.LaunchSpec != nil {
		return it.LaunchSpec.Agent
	}
	return ""
}

// Replay directed input without consuming mailbox cursors: constructing argv
// is not proof that a child started or read it. Comments and mail remain durable
// if launch fails, or if the worker itself read its inbox before being resumed.
func weaveResumePrompt(it *weaveItem) (string, error) {
	body := it.Body
	if strings.TrimSpace(body) == "" {
		body = it.Title
	}
	var thread strings.Builder
	for _, c := range it.Comments {
		if c.Kind == "system" || strings.TrimSpace(c.Body) == "" {
			continue
		}
		fmt.Fprintf(&thread, "\n[%s] %s (%s): %s\n", c.At.UTC().Format(time.RFC3339), c.Author, c.Kind, c.Body)
	}
	readers := []string{}
	if it.Instance != "" {
		readers = append(readers, it.Instance)
	}
	if it.LaunchSpec != nil && it.LaunchSpec.Agent != "" {
		readers = append(readers, it.LaunchSpec.Agent)
	}
	if len(readers) > 0 {
		posts, err := bus.Posts()
		if err != nil {
			return "", fmt.Errorf("read resume steering: %w", err)
		}
		for _, p := range posts {
			at, err := time.Parse(time.RFC3339Nano, p.At)
			if err != nil || at.Before(it.Created.Truncate(time.Second)) {
				continue
			}
			for _, reader := range readers {
				if p.Directed(reader) {
					fmt.Fprintf(&thread, "\n[mb:%d] %s: %s\n", p.Seq, p.From, p.Body)
					break
				}
			}
		}
	}
	if thread.Len() > 0 {
		body += "\n\n---\nWEAVE STEERING AND REVIEW\nIncorporate the following run input before resubmitting. Resolve any conflicting instructions explicitly.\n" + thread.String()
	}
	return body, nil
}

// Codex exec is a one-shot launch: a PTY does not turn it into the
// interactive Codex session. Keep this check on the actual launch recipe;
// a tool having a separate steer_exec does not make its exec steerable.
// Other tools (including streaming-input launches) retain their transport.
func weaveCodexExec(spec *weaveLaunchSpec) bool {
	if spec == nil {
		return false
	}
	name := strings.TrimSuffix(filepath.Base(spec.Tool), ".exe")
	if name != "codex" && spec.Agent != "" {
		cat := fleetCatalog()
		if a, ok := cat.Agent(spec.Agent); ok {
			name = a.Tool
		} else {
			name, _, _ = strings.Cut(spec.Agent, ":")
		}
	}
	if name != "codex" {
		return false
	}
	for i := 0; i < len(spec.Argv); i++ {
		arg := spec.Argv[i]
		switch arg {
		case "--":
			return false // subsequent text is a prompt, not a subcommand
		case "-c", "--config", "-m", "--model", "-p", "--profile", "-C", "--cd", "-s", "--sandbox", "-a", "--ask-for-approval", "--add-dir", "--enable", "--disable", "--local-provider":
			i++
		default:
			if !strings.HasPrefix(arg, "-") {
				return arg == "exec"
			}
		}
	}
	return false
}

// A socket write is transport delivery, not proof of model incorporation.
// Always persist the comment before attempting this best-effort live push.
func weavePushComment(it *weaveItem, body string) bool {
	if weaveCodexExec(it.LaunchSpec) {
		return false
	}
	if it.State != "working" || it.WrapperPid == 0 || !pidAlive(it.WrapperPid) || it.CtlSock == "" {
		return false
	}
	return weaveWriteControlFrame(it.CtlSock, agentpty.TextFrame(body)) == nil
}
