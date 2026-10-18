package feed

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestParseQuiet(t *testing.T) {
	s, e, err := parseQuiet("22:00-07:30")
	if err != nil || s != 22*60 || e != 7*60+30 {
		t.Fatalf("parseQuiet = %d,%d,%v", s, e, err)
	}
	s, e, err = parseQuiet("off")
	if err != nil || s != e || s == 0 {
		t.Fatalf("off must be equal nonzero bounds, got %d,%d,%v", s, e, err)
	}
	for _, bad := range []string{"", "22", "22:00", "25:00-07:00", "22:00-07:60", "22:0-07:00", "08:00-08:00", "a-b"} {
		if _, _, err := parseQuiet(bad); err == nil {
			t.Errorf("parseQuiet(%q) accepted", bad)
		}
	}
}

func TestChannelPrefs(t *testing.T) {
	p, err := Channel{Name: "x", Cmd: "t:c"}.Prefs()
	if err != nil {
		t.Fatal(err)
	}
	if d := DefaultDeliveryPrefs(); p.Interval != d.Interval || p.MaxItems != d.MaxItems || p.QuietStart != d.QuietStart || p.QuietEnd != d.QuietEnd {
		t.Fatalf("unset channel must take the defaults, got %+v", p)
	}
	p, err = Channel{Name: "x", Cmd: "t:c", Interval: "90m", MaxItems: 3, Quiet: "23:00-06:00", Immediate: []string{"a"}, Off: true}.Prefs()
	if err != nil || p.Interval.Minutes() != 90 || p.MaxItems != 3 || p.QuietStart != 23*60 || p.QuietEnd != 6*60 || len(p.Immediate) != 1 || !p.Off {
		t.Fatalf("prefs = %+v, %v", p, err)
	}
	for _, bad := range []Channel{
		{Name: "x", Cmd: "t:c", Interval: "soon"},
		{Name: "x", Cmd: "t:c", Interval: "-1h"},
		{Name: "x", Cmd: "t:c", MaxItems: -1},
		{Name: "x", Cmd: "t:c", Quiet: "never"},
	} {
		if _, err := bad.Prefs(); err == nil {
			t.Errorf("Prefs accepted %+v", bad)
		}
	}
}

func TestChannelStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if cs, err := LoadChannels(dir); err != nil || cs != nil {
		t.Fatalf("missing file = %v, %v", cs, err)
	}
	in := []Channel{{Name: "phone", Cmd: "openclaw:send", Args: "--to me", Interval: "2h", Immediate: []string{"alice", "bob"}}}
	if err := SaveChannels(dir, in); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(channelsPath(dir))
		if err != nil {
			t.Fatal(err)
		}
		if m := st.Mode().Perm(); m != 0o600 {
			t.Fatalf("channels.json mode %v, want 0600", m)
		}
	}
	out, err := LoadChannels(dir)
	if err != nil || len(out) != 1 || out[0].Name != "phone" || len(out[0].Immediate) != 2 || out[0].Args != "--to me" {
		t.Fatalf("round trip = %+v, %v", out, err)
	}
	if err := SaveChannels(dir, []Channel{{Name: "a", Cmd: "t:c"}, {Name: "a", Cmd: "t:c"}}); err == nil {
		t.Fatal("duplicate names accepted")
	}
	if err := SaveChannels(dir, []Channel{{Name: "a/b", Cmd: "t:c"}}); err == nil {
		t.Fatal("path separator in name accepted")
	}
	if err := SaveChannels(dir, []Channel{{Name: "a", Cmd: "nocolon"}}); err == nil {
		t.Fatal("cmd without TOOL:NAME accepted")
	}
}

func TestChannelAddListRm(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake tool is a shell script")
	}
	te := newTestEnv(t, nil)
	fx := newDeliverFixture(t, te)

	te.mustRun("channel", "add", "phone", "--cmd", "fakeclaw:send", "--args", "--to me",
		"--interval", "2h", "--max", "5", "--quiet-hours", "off", "--immediate", "alice", "--immediate", "bob")
	out := te.mustRun("channel", "list")
	for _, want := range []string{"phone", "fakeclaw:send", "interval=2h0m0s", "max=5", "quiet=off", "immediate=alice,bob", "args=--to me"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q:\n%s", want, out)
		}
	}
	if _, _, err := te.run("channel", "add", "phone", "--cmd", "fakeclaw:send"); err == nil {
		t.Error("duplicate channel accepted")
	}
	if _, _, err := te.run("channel", "add", "x", "--cmd", "fakeclaw:nosuch"); err == nil {
		t.Error("unregistered command accepted")
	}
	if _, _, err := te.run("channel", "add", "x", "--cmd", "nosuchtool:send"); err == nil {
		t.Error("unregistered tool accepted")
	}
	if _, _, err := te.run("channel", "add", "x", "--cmd", "fakeclaw:send", "--quiet-hours", "late"); err == nil {
		t.Error("bad quiet window accepted")
	}
	if _, _, err := te.run("channel", "add", "x"); err == nil {
		t.Error("add without --cmd accepted")
	}

	fx.writeState(t, "phone")
	te.mustRun("channel", "rm", "phone")
	if out := te.mustRun("channel", "list"); strings.TrimSpace(out) != "" {
		t.Errorf("list after rm = %q", out)
	}
	if st, _ := loadChannelState(te.env.dir()); len(st) != 0 {
		t.Errorf("rm left delivery state behind: %v", st)
	}
	if _, _, err := te.run("channel", "rm", "phone"); err == nil {
		t.Error("removing a missing channel succeeded")
	}
}
