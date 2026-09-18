package tzcmd

import (
	"encoding/json"
	"fmt"
	"time"
	_ "time/tzdata"

	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/timezones"
)

var cmd = &tool.Tool{
	Name:     "tz",
	Synopsis: "Inspect and convert IANA timezones.",
	Usage:    "tz [--json] list [substr]\n   or: tz [--json] now ZONE\n   or: tz [--json] convert TIME FROM_ZONE TO_ZONE\n   or: tz [--json] info ZONE",
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
	case "list":
		if len(operands) > 2 {
			return tool.UsageError(rc, cmd, "extra operand %q", operands[2])
		}
		substr := ""
		if len(operands) == 2 {
			substr = operands[1]
		}
		zones := timezones.Filter(substr)
		if *jsonOut {
			return writeJSON(rc, map[string]any{"ok": true, "command": "list", "filter": substr, "zones": zones})
		}
		for _, z := range zones {
			fmt.Fprintln(rc.Out, z)
		}
		return 0
	case "now":
		if len(operands) != 2 {
			return tool.UsageError(rc, cmd, "usage: tz now ZONE")
		}
		loc, err := time.LoadLocation(operands[1])
		if err != nil {
			fmt.Fprintf(rc.Err, "tz: %s: %v\n", operands[1], err)
			return 1
		}
		t := time.Now().In(loc)
		if *jsonOut {
			name, off := t.Zone()
			return writeJSON(rc, map[string]any{"ok": true, "command": "now", "zone": operands[1], "time": t.Format(time.RFC3339), "abbrev": name, "offset_seconds": off})
		}
		fmt.Fprintln(rc.Out, t.Format(time.RFC3339))
		return 0
	case "convert":
		if len(operands) != 4 {
			return tool.UsageError(rc, cmd, "usage: tz convert TIME FROM_ZONE TO_ZONE")
		}
		from, err := time.LoadLocation(operands[2])
		if err != nil {
			fmt.Fprintf(rc.Err, "tz: %s: %v\n", operands[2], err)
			return 1
		}
		to, err := time.LoadLocation(operands[3])
		if err != nil {
			fmt.Fprintf(rc.Err, "tz: %s: %v\n", operands[3], err)
			return 1
		}
		t, err := parseWallTime(operands[1], from)
		if err != nil {
			return tool.UsageError(rc, cmd, "invalid time %q", operands[1])
		}
		out := t.In(to)
		if *jsonOut {
			return writeJSON(rc, map[string]any{"ok": true, "command": "convert", "input": operands[1], "from_zone": operands[2], "to_zone": operands[3], "time": out.Format(time.RFC3339)})
		}
		fmt.Fprintln(rc.Out, out.Format(time.RFC3339))
		return 0
	case "info":
		if len(operands) != 2 {
			return tool.UsageError(rc, cmd, "usage: tz info ZONE")
		}
		loc, err := time.LoadLocation(operands[1])
		if err != nil {
			fmt.Fprintf(rc.Err, "tz: %s: %v\n", operands[1], err)
			return 1
		}
		now := time.Now().In(loc)
		abbr, off := now.Zone()
		dst := now.IsDST()
		if *jsonOut {
			return writeJSON(rc, map[string]any{"ok": true, "command": "info", "zone": operands[1], "abbrev": abbr, "offset_seconds": off, "dst": dst})
		}
		fmt.Fprintf(rc.Out, "%s offset=%+03d:%02d abbrev=%s dst=%t\n", operands[1], off/3600, abs(off%3600)/60, abbr, dst)
		return 0
	default:
		return tool.UsageError(rc, cmd, "unknown subcommand %q", operands[0])
	}
}

func parseWallTime(s string, loc *time.Location) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.ParseInLocation("2006-01-02 15:04", s, loc)
}

func writeJSON(rc *tool.RunContext, v any) int {
	if err := json.NewEncoder(rc.Out).Encode(v); err != nil {
		fmt.Fprintf(rc.Err, "tz: %v\n", err)
		return 1
	}
	return 0
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
