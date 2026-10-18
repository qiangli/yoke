package lenscmd

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/lens/stream"
	"github.com/qiangli/yoke/pkg/ref"
)

// SinceMode classifies the syntax and semantics of a --since parameter.
type SinceMode string

const (
	SinceModeStart    SinceMode = "start"
	SinceModeCursor   SinceMode = "cursor"
	SinceModeSeq      SinceMode = "seq"
	SinceModeTime     SinceMode = "time"
	SinceModeDuration SinceMode = "duration"
)

// ParsedSince represents a parsed and validated --since specification.
type ParsedSince struct {
	Mode     SinceMode
	Token    string
	Seq      int64
	Time     time.Time
	Duration time.Duration
	Cursor   *stream.Cursor
}

// ParseSinceCursor parses and validates a --since argument for an agent-world locator.
// It supports:
//   - "start": start from beginning of stream (Token: "")
//   - "@file": read change-stream cursor from file
//   - JSON string: parse change-stream cursor from inline JSON
//   - Sequence number: "42", "#42", "seq:42" (Token: "42")
//   - Duration: "10m", "1h", "30s", "500ms" (Token: RFC3339 timestamp of cutoff)
//   - Absolute time: "2026-10-18T17:42:52Z" (RFC3339 or RFC3339Nano)
//
// For cursor envelopes, it strictly checks that the cursor is bound to loc.Scheme,
// the full target string, and the query, preventing interchanging provider tokens.
func ParseSinceCursor(since string, loc ref.Locator, query string, now time.Time, readFile func(string) ([]byte, error)) (*ParsedSince, error) {
	s := strings.TrimSpace(since)
	if s == "" {
		return nil, fmt.Errorf("empty --since specification")
	}

	restart := fmt.Errorf("restart with --since=start")

	if s == "start" {
		return &ParsedSince{
			Mode:  SinceModeStart,
			Token: "",
		}, nil
	}

	if strings.HasPrefix(s, "@") || strings.HasPrefix(s, "{") {
		data := []byte(s)
		if strings.HasPrefix(s, "@") {
			path := strings.TrimSpace(s[1:])
			if path == "" {
				return nil, fmt.Errorf("empty cursor file path")
			}
			if readFile == nil {
				return nil, fmt.Errorf("cannot read cursor file: no reader provided")
			}
			var err error
			data, err = readFile(path)
			if err != nil {
				return nil, err
			}
		}
		c, err := stream.ParseCursor(data)
		if err != nil {
			return nil, err
		}
		target := loc.String()
		if err := c.Check(loc.Scheme, target, query); err != nil {
			return nil, fmt.Errorf("%w; %v", err, restart)
		}
		if c.Kind != stream.KindChangeStream {
			return nil, fmt.Errorf("requires a change-stream cursor")
		}
		if c.Expiry != "" {
			expiry, err := time.Parse(time.RFC3339, c.Expiry)
			if err != nil {
				return nil, fmt.Errorf("invalid cursor expiry %q: %w", c.Expiry, err)
			}
			if !now.Before(expiry) {
				return nil, fmt.Errorf("cursor expired; %v", restart)
			}
		}
		return &ParsedSince{
			Mode:   SinceModeCursor,
			Token:  c.Token,
			Cursor: &c,
		}, nil
	}

	// Sequence syntax: seq:123, #123, or pure unsigned decimal 123
	if strings.HasPrefix(s, "seq:") || strings.HasPrefix(s, "#") || isDecimalOnly(s) {
		seqStr := s
		if strings.HasPrefix(s, "seq:") {
			seqStr = strings.TrimPrefix(s, "seq:")
		} else if strings.HasPrefix(s, "#") {
			seqStr = strings.TrimPrefix(s, "#")
		}
		if seqStr == "" || !isDecimalOnly(seqStr) {
			return nil, fmt.Errorf("invalid sequence in --since %q: must be unsigned decimal", since)
		}
		seq, err := strconv.ParseInt(seqStr, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid sequence in --since %q: %w", since, err)
		}
		if seq <= 0 {
			return nil, fmt.Errorf("invalid sequence in --since %q: must be positive", since)
		}
		return &ParsedSince{
			Mode:  SinceModeSeq,
			Seq:   seq,
			Token: strconv.FormatInt(seq, 10),
		}, nil
	}

	// Try duration (e.g. 10m, 1h, 30s, 500ms).
	if d, err := time.ParseDuration(s); err == nil {
		if d <= 0 {
			return nil, fmt.Errorf("invalid duration in --since %q: must be positive", since)
		}
		cutoff := now.Add(-d)
		return &ParsedSince{
			Mode:     SinceModeDuration,
			Duration: d,
			Time:     cutoff,
			Token:    cutoff.UTC().Format(time.RFC3339Nano),
		}, nil
	}

	// Try RFC3339 / RFC3339Nano absolute timestamp
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return &ParsedSince{
				Mode:  SinceModeTime,
				Time:  t,
				Token: t.UTC().Format(time.RFC3339Nano),
			}, nil
		}
	}

	// Fallback attempt: if it looks like JSON but wasn't caught
	if strings.HasPrefix(s, "{") || strings.HasSuffix(s, "}") {
		var js map[string]any
		if err := json.Unmarshal([]byte(s), &js); err != nil {
			return nil, fmt.Errorf("invalid cursor JSON in --since: %w", err)
		}
	}

	return nil, fmt.Errorf("invalid --since specification %q: expected start, @file, cursor, sequence (e.g. 42, #42, seq:42), duration (e.g. 10m, 1h), or RFC3339 time", since)
}

func isDecimalOnly(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
