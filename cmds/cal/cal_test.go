package calcmd

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/coreutils/tool"
)

func runTool(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errb bytes.Buffer
	rc := &tool.RunContext{
		Ctx:   context.Background(),
		Dir:   t.TempDir(),
		Stdio: tool.Stdio{In: strings.NewReader(""), Out: &out, Err: &errb},
	}
	code = cmd.Run(rc, args)
	return out.String(), errb.String(), code
}

func TestRenderMonthSundayFirst(t *testing.T) {
	now := time.Date(2024, time.March, 14, 12, 0, 0, 0, time.Local)
	got, err := render(now, []string{"2", "2024"}, false, false, false, false)
	want := "" +
		"   February 2024    \n" +
		"Su Mo Tu We Th Fr Sa\n" +
		"             1  2  3\n" +
		" 4  5  6  7  8  9 10\n" +
		"11 12 13 14 15 16 17\n" +
		"18 19 20 21 22 23 24\n" +
		"25 26 27 28 29      \n" +
		"\n"
	if err != nil || got != want {
		t.Fatalf("render = %q, %v; want %q, nil", got, err, want)
	}
}

func TestRenderMonthMondayFirstAndHighlight(t *testing.T) {
	now := time.Date(2024, time.January, 1, 12, 0, 0, 0, time.Local)
	got, err := render(now, []string{"1", "2024"}, false, false, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "Mo Tu We Th Fr Sa Su") {
		t.Fatalf("missing Monday-first header in %q", got)
	}
	if !strings.Contains(got, "\x1b[7m 1\x1b[0m") {
		t.Fatalf("missing highlighted day in %q", got)
	}
}

func TestRenderYearAndThreeMonths(t *testing.T) {
	now := time.Date(2024, time.March, 14, 12, 0, 0, 0, time.Local)
	year, err := render(now, []string{"2024"}, false, false, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.SplitN(year, "\n", 2)[0], "2024") ||
		!strings.Contains(year, "January 2024") ||
		!strings.Contains(year, "February 2024") ||
		!strings.Contains(year, "March 2024") {
		t.Fatalf("unexpected year layout:\n%s", year)
	}

	three, err := render(now, nil, true, false, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(three, "February 2024") ||
		!strings.Contains(three, "March 2024") ||
		!strings.Contains(three, "April 2024") {
		t.Fatalf("unexpected -3 layout:\n%s", three)
	}
}

func TestCalRunAndErrors(t *testing.T) {
	out, errb, code := runTool(t, "2", "2024")
	if code != 0 || errb != "" || !strings.Contains(out, "February 2024") {
		t.Fatalf("cal = (%q, %q, %d), want February output", out, errb, code)
	}
	_, errb, code = runTool(t, "13", "2024")
	if code != 2 || !strings.Contains(errb, "invalid month") {
		t.Fatalf("bad month = (%q, %d), want usage error", errb, code)
	}
	_, errb, code = runTool(t, "-m", "-s")
	if code != 2 || !strings.Contains(errb, "mutually exclusive") {
		t.Fatalf("-m -s = (%q, %d), want usage error", errb, code)
	}
}

func TestNcalRegistered(t *testing.T) {
	if tool.Lookup("ncal") == nil {
		t.Fatal("ncal is not registered")
	}
}

func TestNcalAliasExecutes(t *testing.T) {
	var out, errb bytes.Buffer
	rc := &tool.RunContext{
		Ctx: context.Background(), Dir: t.TempDir(),
		Stdio: tool.Stdio{In: strings.NewReader(""), Out: &out, Err: &errb},
	}
	code := ncalCmd.Run(rc, []string{"2", "2024"})
	if code != 0 || errb.String() != "" || !strings.Contains(out.String(), "February 2024") {
		t.Fatalf("ncal: code=%d stdout=%q stderr=%q", code, out.String(), errb.String())
	}
}
