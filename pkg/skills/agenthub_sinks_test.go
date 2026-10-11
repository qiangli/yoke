package skills

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

const hubSentinel = "SENTINEL-SECRET-BODY-7f3a91"

var hubFence = regexp.MustCompile("(?s)~~~bash\n(.*?)\n~~~")

// loadAgentHubFunctions returns the fenced Bash functions of skills/agent-hub.md.
func loadAgentHubFunctions(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "skills", "agent-hub.md"))
	if err != nil {
		t.Fatal(err)
	}
	var fns []string
	for _, m := range hubFence.FindAllSubmatch(b, -1) {
		fns = append(fns, string(m[1]))
	}
	if len(fns) == 0 {
		t.Fatal("no fenced bash block in agent-hub.md")
	}
	return strings.Join(fns, "\n")
}

// runHubSinks runs every external sink path (desktop via osascript and via
// notify-send, ntfy, webhook) over one event line and returns everything the
// stubbed external commands received (argv, stdin).
func runHubSinks(t *testing.T, event string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("bash sink recipes")
	}
	for _, tool := range []string{"bash", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "external.log")
	stub := "#!/bin/sh\n{ echo \"== $(basename \"$0\")\"; for a in \"$@\"; do printf 'arg:%s\\n' \"$a\"; done; cat; } >> " + log + "\n"
	for _, name := range []string{"osascript", "notify-send", "curl"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(stub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "hub.sh"), []byte(loadAgentHubFunctions(t)), 0o644); err != nil {
		t.Fatal(err)
	}

	script := `. "$HUB"
printf '%s\n' "$EVENT" | agent_hub_dispatch desktop
printf '%s\n' "$EVENT" | agent_hub_dispatch ntfy
printf '%s\n' "$EVENT" | agent_hub_dispatch webhook
`
	// Second desktop pass with osascript hidden, so notify-send is exercised too.
	scriptNotifySend := `. "$HUB"
command() { if [ "$1" = -v ] && [ "$2" = osascript ]; then return 1; fi; builtin command "$@"; }
printf '%s\n' "$EVENT" | desktop
`
	env := append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HUB="+filepath.Join(dir, "hub.sh"),
		"EVENT="+event,
		"NTFY_URL=https://ntfy.invalid/t",
		"WEBHOOK_URL=https://hook.invalid/h",
	)
	for _, sc := range []string{script, scriptNotifySend} {
		cmd := exec.Command("bash", "-c", sc)
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("sink run failed: %v\n%s", err, out)
		}
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAgentHubSinksNeverSendEventBody(t *testing.T) {
	event := `{"type":"approval","target":"run-42","body":"` + hubSentinel + ` token=abc"}`
	got := runHubSinks(t, event)
	if strings.Contains(got, hubSentinel) {
		t.Fatalf("event body leaked to an external sink:\n%s", got)
	}
	for _, sink := range []string{"== osascript", "== notify-send", "== curl"} {
		if !strings.Contains(got, sink) {
			t.Fatalf("sink %q was not exercised:\n%s", sink, got)
		}
	}
	if !strings.Contains(got, "bashy [approval] run-42") {
		t.Fatalf("blank summary should fall back to the type/target title:\n%s", got)
	}
}

func TestAgentHubSinksIgnoreBodyAlongsideSummary(t *testing.T) {
	event := `{"type":"notice","target":"sprint-1","summary":"deploy blocked","body":"` + hubSentinel + `"}`
	got := runHubSinks(t, event)
	if strings.Contains(got, hubSentinel) {
		t.Fatalf("event body leaked to an external sink:\n%s", got)
	}
	// every sink carries the summary: osascript, notify-send, ntfy, webhook
	if n := strings.Count(got, "deploy blocked"); n < 4 {
		t.Fatalf("summary delivered %d times, want >= 4:\n%s", n, got)
	}
}

func TestAgentHubSinksRejectNonStringSubject(t *testing.T) {
	event := `{"type":"notice","summary":{"nested":"` + hubSentinel + `"}}`
	got := runHubSinks(t, event)
	if strings.Contains(got, hubSentinel) {
		t.Fatalf("non-string summary leaked to an external sink:\n%s", got)
	}
}
