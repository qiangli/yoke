// Package calcmd implements cal(1): print a calendar for a month or year.
package calcmd

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/qiangli/coreutils/tool"
)

var cmd = &tool.Tool{
	Name:     "cal",
	Synopsis: "Display a calendar.",
	Usage:    "cal [OPTION]... [[MONTH] YEAR]",
}

var ncalCmd = &tool.Tool{
	Name:     "ncal",
	Synopsis: "Display a calendar.",
	Usage:    "ncal [OPTION]... [[MONTH] YEAR]",
}

func init() {
	cmd.Run = run
	ncalCmd.Run = run
	tool.Register(cmd)
	tool.Register(ncalCmd)
}

func run(rc *tool.RunContext, args []string) int {
	fs := tool.NewFlags(cmd.Name)
	three := fs.BoolP("three", "3", false, "display previous, current and next month")
	yearFlag := fs.BoolP("year", "y", false, "display the current year")
	monday := fs.BoolP("monday", "m", false, "display Monday as the first day of the week")
	sunday := fs.BoolP("sunday", "s", false, "display Sunday as the first day of the week")
	noHighlight := fs.BoolP("no-highlight", "h", false, "suppress highlighting of today")
	operands, code := tool.Parse(rc, cmd, fs, args)
	if code >= 0 {
		return code
	}
	if *monday && *sunday && fs.Changed("monday") && fs.Changed("sunday") {
		return tool.UsageError(rc, cmd, "-m and -s are mutually exclusive")
	}

	now := time.Now()
	firstMonday := *monday && !*sunday
	highlight := !*noHighlight && stdoutIsTTY(rc.Out)
	text, err := render(now, operands, *three, *yearFlag, firstMonday, highlight)
	if err != nil {
		return tool.UsageError(rc, cmd, "%s", err)
	}
	fmt.Fprint(rc.Out, text)
	return 0
}

func render(now time.Time, operands []string, three, yearFlag, mondayFirst, highlight bool) (string, error) {
	year, month := now.Year(), now.Month()
	wholeYear := yearFlag

	switch len(operands) {
	case 0:
	case 1:
		y, err := parseYear(operands[0])
		if err != nil {
			return "", err
		}
		year, wholeYear = y, true
	case 2:
		m, err := parseMonth(operands[0])
		if err != nil {
			return "", err
		}
		y, err := parseYear(operands[1])
		if err != nil {
			return "", err
		}
		month, year = time.Month(m), y
	default:
		return "", fmt.Errorf("extra operand %q", operands[2])
	}

	switch {
	case three:
		return renderThree(year, month, mondayFirst, highlight, now), nil
	case wholeYear:
		return renderYear(year, mondayFirst, highlight, now), nil
	default:
		return strings.Join(monthLines(year, month, mondayFirst, highlight, now), "\n") + "\n", nil
	}
}

func parseMonth(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 12 {
		return 0, fmt.Errorf("invalid month %q", s)
	}
	return n, nil
}

func parseYear(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 9999 {
		return 0, fmt.Errorf("invalid year %q", s)
	}
	return n, nil
}

func renderThree(year int, month time.Month, mondayFirst, highlight bool, today time.Time) string {
	mid := time.Date(year, month, 1, 0, 0, 0, 0, time.Local)
	months := []time.Time{mid.AddDate(0, -1, 0), mid, mid.AddDate(0, 1, 0)}
	return joinMonthBlocks([][]string{
		monthLines(months[0].Year(), months[0].Month(), mondayFirst, highlight, today),
		monthLines(months[1].Year(), months[1].Month(), mondayFirst, highlight, today),
		monthLines(months[2].Year(), months[2].Month(), mondayFirst, highlight, today),
	})
}

func renderYear(year int, mondayFirst, highlight bool, today time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n", center(strconv.Itoa(year), 64))
	for row := 0; row < 4; row++ {
		var blocks [][]string
		for col := 0; col < 3; col++ {
			month := time.Month(row*3 + col + 1)
			blocks = append(blocks, monthLines(year, month, mondayFirst, highlight, today))
		}
		b.WriteString(joinMonthBlocks(blocks))
		if row != 3 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func joinMonthBlocks(blocks [][]string) string {
	var b strings.Builder
	maxLines := 0
	for _, block := range blocks {
		if len(block) > maxLines {
			maxLines = len(block)
		}
	}
	for i := 0; i < maxLines; i++ {
		for j, block := range blocks {
			line := ""
			if i < len(block) {
				line = block[i]
			}
			if j > 0 {
				b.WriteString("  ")
			}
			b.WriteString(padRight(line, 20))
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func monthLines(year int, month time.Month, mondayFirst, highlight bool, today time.Time) []string {
	lines := []string{
		center(time.Date(year, month, 1, 0, 0, 0, 0, time.Local).Format("January 2006"), 20),
		weekHeader(mondayFirst),
	}
	week := make([]string, 7)
	first := time.Date(year, month, 1, 0, 0, 0, 0, time.Local)
	offset := weekdayIndex(first.Weekday(), mondayFirst)
	for i := 0; i < offset; i++ {
		week[i] = "  "
	}
	for day := 1; day <= daysInMonth(year, month); day++ {
		d := time.Date(year, month, day, 0, 0, 0, 0, time.Local)
		idx := weekdayIndex(d.Weekday(), mondayFirst)
		cell := fmt.Sprintf("%2d", day)
		if highlight && sameDate(d, today) {
			cell = "\x1b[7m" + cell + "\x1b[0m"
		}
		week[idx] = cell
		if idx == 6 {
			lines = append(lines, strings.Join(week, " "))
			week = make([]string, 7)
		}
	}
	if hasDay(week) {
		for i := range week {
			if week[i] == "" {
				week[i] = "  "
			}
		}
		lines = append(lines, strings.Join(week, " "))
	}
	for len(lines) < 8 {
		lines = append(lines, "")
	}
	return lines
}

func weekHeader(mondayFirst bool) string {
	if mondayFirst {
		return "Mo Tu We Th Fr Sa Su"
	}
	return "Su Mo Tu We Th Fr Sa"
}

func weekdayIndex(w time.Weekday, mondayFirst bool) int {
	if !mondayFirst {
		return int(w)
	}
	if w == time.Sunday {
		return 6
	}
	return int(w) - 1
}

func daysInMonth(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.Local).Day()
}

func center(s string, width int) string {
	if len(s) >= width {
		return s
	}
	left := (width - len(s)) / 2
	return strings.Repeat(" ", left) + s + strings.Repeat(" ", width-len(s)-left)
}

func padRight(s string, width int) string {
	if len(s) >= width {
		return s
	}
	return s + strings.Repeat(" ", width-len(s))
}

func hasDay(week []string) bool {
	for _, cell := range week {
		if strings.TrimSpace(cell) != "" {
			return true
		}
	}
	return false
}

func sameDate(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

func stdoutIsTTY(w any) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}
