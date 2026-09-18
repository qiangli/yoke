package durationcmd

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/qiangli/coreutils/tool"
)

var cmd = &tool.Tool{
	Name:     "duration",
	Synopsis: "Convert and compare durations for agent workflows.",
	Usage:    "duration [--json] to-secs DUR\n   or: duration [--json] humanize SECONDS\n   or: duration [--json] since TIME\n   or: duration [--json] until TIME\n   or: duration [--json] between A B",
}

func init() { cmd.Run = run; tool.Register(cmd) }

func run(rc *tool.RunContext, args []string) int {
	fs := tool.NewFlags(cmd.Name)
	jsonOut := fs.Bool("json", false, "write a machine-readable JSON envelope")
	operands, code := tool.Parse(rc, cmd, fs, args)
	if code >= 0 {
		return code
	}
	if len(operands) == 0 {
		return tool.UsageError(rc, cmd, "missing subcommand")
	}
	switch operands[0] {
	case "to-secs":
		if len(operands) != 2 {
			return tool.UsageError(rc, cmd, "usage: duration to-secs DUR")
		}
		d, err := parseDurationOrSeconds(operands[1])
		if err != nil {
			return tool.UsageError(rc, cmd, "invalid duration %q", operands[1])
		}
		return emit(rc, *jsonOut, "to-secs", d)
	case "humanize":
		if len(operands) != 2 {
			return tool.UsageError(rc, cmd, "usage: duration humanize SECONDS")
		}
		d, err := parseDurationOrSeconds(operands[1])
		if err != nil {
			return tool.UsageError(rc, cmd, "invalid seconds %q", operands[1])
		}
		return emit(rc, *jsonOut, "humanize", d)
	case "since", "until":
		if len(operands) != 2 {
			return tool.UsageError(rc, cmd, "usage: duration %s TIME", operands[0])
		}
		t, err := parseTimeArg(operands[1])
		if err != nil {
			return tool.UsageError(rc, cmd, "invalid time %q", operands[1])
		}
		d := time.Since(t)
		if operands[0] == "until" {
			d = time.Until(t)
		}
		return emit(rc, *jsonOut, operands[0], d)
	case "between":
		if len(operands) != 3 {
			return tool.UsageError(rc, cmd, "usage: duration between A B")
		}
		a, err := parseTimeArg(operands[1])
		if err != nil {
			return tool.UsageError(rc, cmd, "invalid time %q", operands[1])
		}
		b, err := parseTimeArg(operands[2])
		if err != nil {
			return tool.UsageError(rc, cmd, "invalid time %q", operands[2])
		}
		return emit(rc, *jsonOut, "between", b.Sub(a))
	default:
		return tool.UsageError(rc, cmd, "unknown subcommand %q", operands[0])
	}
}

func emit(rc *tool.RunContext, jsonOut bool, op string, d time.Duration) int {
	secs := int64(d / time.Second)
	human := humanDuration(d)
	if jsonOut {
		if err := json.NewEncoder(rc.Out).Encode(map[string]any{"ok": true, "command": op, "seconds": secs, "duration": human}); err != nil {
			fmt.Fprintf(rc.Err, "duration: %v\n", err)
			return 1
		}
		return 0
	}
	if op == "to-secs" {
		fmt.Fprintln(rc.Out, secs)
	} else {
		fmt.Fprintln(rc.Out, human)
	}
	return 0
}

func parseDurationOrSeconds(s string) (time.Duration, error) {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.Duration(n) * time.Second, nil
	}
	return time.ParseDuration(s)
}

func parseTimeArg(s string) (time.Time, error) {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.Unix(n, 0), nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05", "2006-01-02 15:04"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid time")
}

func humanDuration(d time.Duration) string {
	sign := ""
	if d < 0 {
		sign = "-"
		d = -d
	}
	secs := int64(d / time.Second)
	h, rem := secs/3600, secs%3600
	m, s := rem/60, rem%60
	var b strings.Builder
	b.WriteString(sign)
	if h != 0 {
		fmt.Fprintf(&b, "%dh", h)
	}
	if m != 0 {
		fmt.Fprintf(&b, "%dm", m)
	}
	if s != 0 || h == 0 && m == 0 {
		fmt.Fprintf(&b, "%ds", s)
	}
	return b.String()
}
