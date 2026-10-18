package kb

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/qiangli/yoke/pkg/redact"
	"github.com/qiangli/yoke/pkg/secrets"
	"io"
	"os"
	"strings"
)

// Transcript is the turn transcript accepted by `kb note add --transcript`:
//
//	{"episode":"ID","turn":N,"messages":[{"role":"user|assistant|tool","content":".."}]}
//
// Distillation is deterministic and model-free: title = first user message
// (whitespace-collapsed, bounded), body = the turn's messages as "role: content"
// blocks, bounded.
type Transcript struct {
	Episode  string              `json:"episode"`
	Turn     int                 `json:"turn"`
	Messages []TranscriptMessage `json:"messages"`
}

// TranscriptMessage is one message of a Transcript.
type TranscriptMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

const (
	transcriptTitleCap = 80
	transcriptBodyCap  = 4000
)

func readTranscript(c io.Reader, path string) (*Transcript, error) {
	var b []byte
	var err error
	if path == "-" {
		b, err = io.ReadAll(c)
	} else {
		b, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, fmt.Errorf("kb: read transcript: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	var t Transcript
	if err := dec.Decode(&t); err != nil {
		return nil, fmt.Errorf("kb: invalid transcript JSON: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("kb: invalid transcript JSON: expected exactly one object")
	}
	if len(t.Messages) == 0 {
		return nil, fmt.Errorf("kb: invalid transcript: no messages")
	}
	return &t, nil
}

// Distill returns the deterministic (title, body) for the transcript.
func (t *Transcript) Distill() (title, body string) {
	pick := func(role string) string {
		for _, m := range t.Messages {
			if (role == "" || m.Role == role) && strings.TrimSpace(m.Content) != "" {
				return m.Content
			}
		}
		return ""
	}
	first := pick("user")
	if first == "" {
		first = pick("")
	}
	title = truncateRunes(maskDistilled(strings.Join(strings.Fields(first), " ")), transcriptTitleCap)

	var sb strings.Builder
	for _, m := range t.Messages {
		content := strings.TrimSpace(m.Content)
		if content == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(m.Role + ": " + content)
	}
	body = truncateRunes(maskDistilled(sb.String()), transcriptBodyCap)
	return title, body
}

// maskDistilled scrubs host identity, then masks credential shapes.
// pkg/execlog.MaskSecrets would cycle (execlog imports kb), so the shared
// pkg/secrets shape masker is used instead.
func maskDistilled(s string) string {
	s = redact.FromHost().String(s)
	r := secrets.NewRedactor()
	_ = r.SetShapeMode(secrets.ShapeMask)
	return string(r.Redact([]byte(s)))
}
