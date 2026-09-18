package ntpcmd

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/coreutils/tool"
)

func runTool(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	rc := &tool.RunContext{Ctx: context.Background(), Dir: t.TempDir(), Stdio: tool.Stdio{Out: &out, Err: &errb, In: strings.NewReader("")}}
	code := ntpTool.Run(rc, args)
	return out.String(), errb.String(), code
}

func TestParseResponseOffsetMath(t *testing.T) {
	base := time.Unix(1700000000, 0).UTC()
	t1 := base
	t2 := base.Add(150 * time.Millisecond)
	t3 := base.Add(160 * time.Millisecond)
	t4 := base.Add(70 * time.Millisecond)
	var pkt [48]byte
	pkt[0] = 0x24 // server mode 4
	pkt[1] = 1
	putTimestamp(pkt[32:40], t2)
	putTimestamp(pkt[40:48], t3)
	got, err := parseResponse(pkt[:], t1, t4)
	if err != nil {
		t.Fatal(err)
	}
	if got.Offset < 120*time.Millisecond-time.Nanosecond || got.Offset > 120*time.Millisecond+time.Nanosecond {
		t.Fatalf("offset=%s want 120ms", got.Offset)
	}
	if got.RTT < 60*time.Millisecond-time.Nanosecond || got.RTT > 60*time.Millisecond+time.Nanosecond {
		t.Fatalf("rtt=%s want 60ms", got.RTT)
	}
}

func TestUsageValidation(t *testing.T) {
	_, errb, code := runTool(t, "--timeout", "nope")
	if code != 2 || !strings.Contains(errb, "invalid timeout") {
		t.Fatalf("code=%d err=%q", code, errb)
	}
}

func TestSNTPAliasValidatesUnderItsOwnName(t *testing.T) {
	var out, errb bytes.Buffer
	rc := &tool.RunContext{
		Ctx: context.Background(), Dir: t.TempDir(),
		Stdio: tool.Stdio{Out: &out, Err: &errb, In: strings.NewReader("")},
	}
	code := sntpTool.Run(rc, []string{"--timeout", "nope"})
	if code != 2 || !strings.HasPrefix(errb.String(), "sntp:") || strings.Contains(errb.String(), "Try 'ntp ") {
		t.Fatalf("sntp: code=%d stdout=%q stderr=%q", code, out.String(), errb.String())
	}
}

func TestLiveQueryOptional(t *testing.T) {
	t.Skip("live NTP requires network; packet parser is unit-tested")
}
