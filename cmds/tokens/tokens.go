// Package tokenscmd implements `tokens`: count the LLM tokens of files or
// standard input — a `wc` for tokens, so an agent can budget context before
// reading. By default it uses an exact BPE tokenizer (tiktoken-go/tokenizer,
// MIT, offline-embedded vocab) for OpenAI encodings; `--fast` switches to a
// dep-free, model-agnostic heuristic (≈ chars/4). Note exact counts are
// OpenAI-encoding-specific; other models' tokenizers differ, so treat any count
// as a close estimate. Pairs with `yc repomap --budget`.
package tokenscmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	tiktoken "github.com/tiktoken-go/tokenizer"

	"github.com/qiangli/coreutils/pkg/weavecli"
	"github.com/qiangli/coreutils/tool"
)

const tokensSchemaVersion = "bashy-tokens-v1"

var cmd = &tool.Tool{
	Name:     "tokens",
	Synopsis: "Count the LLM tokens of files or stdin (exact BPE; --fast for a heuristic).",
	Usage:    "tokens [--json] [--fast] [--encoding ENC] [FILE...]",
}

func init() { cmd.Run = run; tool.Register(cmd) }

type count struct {
	Name   string `json:"file"`
	Chars  int    `json:"chars"`
	Words  int    `json:"words"`
	Tokens int    `json:"tokens"`
}

// estimate is the --fast heuristic: max of the chars/4 and words*4/3 rules,
// which tracks real BPE counts better than either alone across prose and code.
func estimate(chars, words int) int {
	byChars := (chars + 3) / 4
	byWords := (words*4 + 2) / 3
	if byWords > byChars {
		return byWords
	}
	return byChars
}

func run(rc *tool.RunContext, args []string) int {
	fs := tool.NewFlags(cmd.Name)
	asJSON := fs.Bool("json", weavecli.IsAgent(), "emit a bashy-tokens-v1 envelope (default under $BASHY_AGENTIC)")
	fast := fs.Bool("fast", false, "use the dep-free heuristic (~chars/4) instead of exact BPE")
	enc := fs.String("encoding", "o200k_base", "tiktoken encoding for exact counts (o200k_base | cl100k_base)")
	operands, code := tool.Parse(rc, cmd, fs, args)
	if code >= 0 {
		return code
	}

	// Resolve the counter: exact BPE by default, heuristic on --fast or if the
	// encoding can't be loaded (fail soft to a count, loud on stderr).
	var codec tiktoken.Codec
	method := *enc
	if *fast {
		method = "heuristic"
	} else if c, err := tiktoken.Get(tiktoken.Encoding(*enc)); err != nil {
		fmt.Fprintf(rc.Err, "tokens: encoding %q unavailable (%v); using heuristic\n", *enc, err)
		method = "heuristic"
	} else {
		codec = c
	}
	countTok := func(s string, chars, words int) int {
		if codec != nil {
			if n, err := codec.Count(s); err == nil {
				return n
			}
		}
		return estimate(chars, words)
	}

	tally := func(name string, data []byte) count {
		s := string(data)
		chars, words := utf8.RuneCountInString(s), len(strings.Fields(s))
		return count{Name: name, Chars: chars, Words: words, Tokens: countTok(s, chars, words)}
	}

	var counts []count
	status := 0
	if len(operands) == 0 {
		data, _ := io.ReadAll(rc.In)
		counts = append(counts, tally("-", data))
	} else {
		for _, f := range operands {
			data, err := os.ReadFile(rc.Path(f))
			if err != nil {
				fmt.Fprintf(rc.Err, "tokens: %s: %v\n", f, err)
				status = 1
				continue
			}
			counts = append(counts, tally(f, data))
		}
	}

	total := count{Name: "total"}
	for _, c := range counts {
		total.Chars += c.Chars
		total.Words += c.Words
		total.Tokens += c.Tokens
	}

	if *asJSON {
		out := map[string]any{"schema_version": tokensSchemaVersion, "method": method, "files": counts}
		if len(counts) > 1 {
			out["total"] = total
		}
		b, _ := json.Marshal(out)
		fmt.Fprintln(rc.Out, string(b))
		return status
	}

	for _, c := range counts {
		fmt.Fprintf(rc.Out, "%8d  %s\n", c.Tokens, c.Name)
	}
	if len(counts) > 1 {
		fmt.Fprintf(rc.Out, "%8d  total\n", total.Tokens)
	}
	return status
}
