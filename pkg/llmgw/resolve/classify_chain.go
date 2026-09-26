package resolve

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// The `model: "auto"` classifier chain.
//
// V1 ships two stages, not the full four the design describes:
//
//  1. HistoryNN — exact-prefix-hash lookup over the last N
//     successful resolutions. If a near-identical prompt was
//     routed to a model and succeeded, route this one the same
//     way. No embedding model required; the v1 similarity proxy
//     is "same first 256 chars → same resolution".
//  2. PromptHeuristic — keyword + length scan over the first
//     user message. Code fence ⇒ coding; image attached ⇒
//     vision; "summari[sz]e" ⇒ general L1-L2; long input ⇒
//     higher tier. Always returns at least (TierL2, [general]).
//
// An embedding-model stage (ONNX MiniLM) plus cascading-LLM stages
// land when that runtime infrastructure is scheduled. For now this
// chain handles the niche "I don't know what model to ask for" case
// end-to-end.

// AutoModelNames are the magic strings the resolver treats as the
// auto-classify trigger.
var AutoModelNames = map[string]struct{}{
	"auto":  {},
	"smart": {},
}

// IsAutoModelName reports whether the bare model string asks the
// resolver to classify from the prompt itself.
func IsAutoModelName(s string) bool {
	_, ok := AutoModelNames[strings.ToLower(strings.TrimSpace(s))]
	return ok
}

// PromptResolution is what the History-NN stage remembers about one
// past resolution, as far as the classifier chain is concerned.
type PromptResolution struct {
	Tier    int
	Domains []string
	Success bool
}

// PromptHistory is the History-NN stage's lookup seam. The recent
// (prompt-prefix-hash → resolution) buffer is owned by the scheduler,
// so the chain takes it as a dependency instead of reaching for a
// package global; a nil PromptHistory skips the stage.
type PromptHistory interface {
	Lookup(key string) (PromptResolution, bool)
}

// PromptHistoryFunc adapts a plain function to PromptHistory.
type PromptHistoryFunc func(key string) (PromptResolution, bool)

// Lookup implements PromptHistory.
func (f PromptHistoryFunc) Lookup(key string) (PromptResolution, bool) { return f(key) }

// ClassifyFromPrompt runs the chain over the JSON request body.
// Returns (tier, domains, stage) where stage is the name of the
// chain stage that produced the answer. Always returns at least
// (TierL2, [DomainGeneral]) — never "no opinion."
func ClassifyFromPrompt(body []byte, hist PromptHistory) (int, []string, string) {
	prompt := ExtractFirstUserPrompt(body)
	hasImage := hasImagePart(body)

	// Stage 1: HistoryNN. Skipped when prompt is too short to be
	// useful as a key, or when no history was supplied.
	if prompt != "" && hist != nil {
		key := PromptPrefixKey(prompt)
		if rec, ok := hist.Lookup(key); ok && rec.Success {
			return rec.Tier, rec.Domains, "history_nn"
		}
	}

	// Stage 2: PromptHeuristic.
	tier, doms := promptHeuristic(prompt, hasImage)
	return tier, doms, "heuristic"
}

// ExtractFirstUserPrompt pulls a best-effort text snippet from the
// JSON body. Returns "" when the body isn't a recognisable chat or
// embeddings shape.
func ExtractFirstUserPrompt(body []byte) string {
	var doc struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Input  json.RawMessage `json:"input"`
		Prompt string          `json:"prompt"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return ""
	}
	// /v1/chat/completions — first user message wins.
	for _, m := range doc.Messages {
		if strings.EqualFold(m.Role, "user") {
			return extractTextFromContent(m.Content)
		}
	}
	// /v1/embeddings — input is the text.
	if len(doc.Input) > 0 {
		var s string
		if err := json.Unmarshal(doc.Input, &s); err == nil {
			return s
		}
		var arr []string
		if err := json.Unmarshal(doc.Input, &arr); err == nil && len(arr) > 0 {
			return arr[0]
		}
	}
	// /v1/completions — prompt is a flat string.
	if doc.Prompt != "" {
		return doc.Prompt
	}
	return ""
}

// extractTextFromContent unwraps the various OpenAI content shapes —
// plain string, [{type:"text", text:"..."}, {type:"image_url", ...}],
// or anything else. Returns the concatenated text segments.
func extractTextFromContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var arr []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &arr); err == nil {
		var b strings.Builder
		for _, p := range arr {
			if p.Text != "" {
				b.WriteString(p.Text)
				b.WriteByte(' ')
			}
		}
		return strings.TrimSpace(b.String())
	}
	return ""
}

// hasImagePart reports whether any chat message carries an
// image_url part. Cheap regex-ish scan over the bytes — we don't
// need to fully parse the content shape to know an image is
// present, and false positives just bias toward vision routing
// which is harmless when the chosen model accepts text too.
func hasImagePart(body []byte) bool {
	return strings.Contains(string(body), `"image_url"`)
}

// promptHeuristic is the classifier-of-last-resort. Always returns
// something. Designed to be cheap and predictable so test
// assertions are stable.
func promptHeuristic(prompt string, hasImage bool) (int, []string) {
	p := strings.ToLower(prompt)
	tier := TierL2 // safe default

	doms := []string{}
	if hasImage {
		doms = append(doms, DomainVision)
	}
	if hasCodeMarkers(p) {
		doms = append(doms, DomainCoding)
	}
	if hasReasoningMarkers(p) {
		doms = append(doms, DomainReasoning)
	}
	if len(doms) == 0 {
		doms = []string{DomainGeneral}
	}

	// Long prompts → bigger model. Threshold tuned conservatively;
	// most agentic prompts under 1.5KB fit L2 fine.
	switch {
	case len(prompt) > 8000:
		tier = TierL3
	case len(prompt) > 32000:
		tier = TierL4
	}
	return tier, doms
}

func hasCodeMarkers(p string) bool {
	if strings.Contains(p, "```") {
		return true
	}
	for _, kw := range []string{
		"def ", "function ", "class ", "import ", "func ", "package ",
		"#include", "module.exports", "console.log", "print(",
		"refactor", "compile", "stack trace", "syntax error",
	} {
		if strings.Contains(p, kw) {
			return true
		}
	}
	return false
}

func hasReasoningMarkers(p string) bool {
	for _, kw := range []string{
		"step by step", "step-by-step", "let's think", "reasoning",
		"prove", "derive", "show that", "calculate exactly",
	} {
		if strings.Contains(p, kw) {
			return true
		}
	}
	return false
}

// PromptPrefixKey hashes the first 256 chars of the (normalised)
// prompt. Used by the History-NN buffer as the lookup key.
func PromptPrefixKey(prompt string) string {
	const sz = 256
	p := strings.ToLower(strings.Join(strings.Fields(prompt), " "))
	if len(p) > sz {
		p = p[:sz]
	}
	h := sha256.Sum256([]byte(p))
	return hex.EncodeToString(h[:8])
}
