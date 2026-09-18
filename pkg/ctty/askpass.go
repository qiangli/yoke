package ctty

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// SSHAskpassEnv is the long-established name for this exact facility. Honouring
// it means an operator who already configured a pinentry for ssh/gpg gets one for
// bashy with no further setup.
const SSHAskpassEnv = "SSH_ASKPASS"

// helperKind selects the result contract, because every askpass program reports
// "the user cancelled" differently and getting that wrong is precisely how an
// empty secret gets returned as a success.
type helperKind int

const (
	kindSSHAskpass helperKind = iota // argv[1]=prompt, value on stdout, exit 0
	kindOsascript                    // our AppleScript, prints OK:<v> | CANCEL | TIMEOUT
	kindZenity                       // --entry --hide-text; exit 1 cancel, 5 timeout
	kindKDialog                      // --password; exit 1 cancel
	kindPinentry                     // Assuan protocol on stdin/stdout
	kindPowerShell                   // our WinForms script, prints OK:<v> | CANCEL
)

type helper struct {
	path string
	kind helperKind
}

// findHelper resolves the askpass program to use, or nil.
//
// An explicitly configured helper wins and is NOT subjected to the attendedness
// test below: naming a helper is the operator asserting that it reaches them, and
// second-guessing an explicit configuration is how tools become unpredictable.
// Autodetected helpers get no such assertion, so they must pass guiAttended.
func findHelper() *helper {
	for _, env := range []string{AskpassEnv, SSHAskpassEnv} {
		if p := strings.TrimSpace(os.Getenv(env)); p != "" {
			if abs, err := exec.LookPath(p); err == nil {
				return &helper{path: abs, kind: kindForPath(abs)}
			}
		}
	}
	if !guiAttended() {
		return nil
	}
	for _, c := range guiCandidates() {
		if abs, err := exec.LookPath(c.path); err == nil {
			c.path = abs
			return &c
		}
	}
	return nil
}

// kindForPath guesses the contract for an operator-named helper. A pinentry is
// recognised by name because its protocol is nothing like the ssh-askpass one;
// everything else is assumed to follow ssh-askpass, which is the convention the
// variable implies.
func kindForPath(p string) helperKind {
	base := strings.ToLower(p)
	switch {
	case strings.Contains(base, "pinentry"):
		return kindPinentry
	default:
		return kindSSHAskpass
	}
}

// GUIAvailable reports whether an ATTENDED GUI askpass channel exists here.
//
// "Attended" is the whole point — see the package doc. A GUI that exists on this
// machine but that the invoking human cannot see is worse than no GUI at all.
func GUIAvailable() bool { return findHelper() != nil }

// AskGUI puts the request to the human through a GUI helper.
//
// Errors are meaningfully distinct and callers depend on it: ErrNoGUI means the
// channel is unavailable (try the next rung); ErrDeclined and ErrTimeout mean the
// human WAS reached and the answer was no or nothing (do not try another rung —
// re-prompting someone who just cancelled is how a nuisance becomes a phish).
func AskGUI(req Request) ([]byte, error) {
	h := findHelper()
	if h == nil {
		return nil, ErrNoGUI
	}

	// The context is a backstop, not the user-facing timeout: helpers that support
	// a native timeout get it (so the human sees the dialog close itself), and the
	// grace period keeps us from killing a helper that is about to report cleanly.
	ctx, cancel := context.WithTimeout(context.Background(), req.timeout()+10*time.Second)
	defer cancel()

	switch h.kind {
	case kindOsascript:
		return runOsascript(ctx, h.path, req)
	case kindZenity:
		return runZenity(ctx, h.path, req)
	case kindKDialog:
		return runKDialog(ctx, h.path, req)
	case kindPinentry:
		return runPinentry(ctx, h.path, req)
	case kindPowerShell:
		return runPowerShell(ctx, h.path, req)
	default:
		return runSSHAskpass(ctx, h.path, req)
	}
}

// answered enforces the rule that a value only counts when it is non-empty.
//
// An affirmative button with an empty field is the human declining in a way that
// looks like success to a naive check. Returning it as a secret would store an
// empty credential and report success — the failure mode this package exists to
// prevent.
func answered(v []byte) ([]byte, error) {
	if len(v) == 0 {
		return nil, ErrDeclined
	}
	return v, nil
}

// osascriptSrc asks AppleScript for a dialog.
//
// The text is passed as `argv`, never interpolated into the source: the prompt is
// agent-supplied, and a prompt containing a quote or a backslash would otherwise
// let the requester write AppleScript that runs as the operator. `on run argv` is
// the parameterised-query equivalent for osascript.
//
// The three returns are the positive-evidence contract. `giving up after` makes
// the dialog self-dismiss, and its result carries `gave up` — which is the field
// that must be checked, because the process still exits 0 when nobody answers.
const osascriptSrc = `on run argv
	set theText to item 1 of argv
	set theTitle to item 2 of argv
	set isHidden to (item 3 of argv is "1")
	set tmo to (item 4 of argv) as integer
	try
		if isHidden then
			set r to display dialog theText with title theTitle default answer "" with hidden answer buttons {"Cancel", "OK"} default button "OK" with icon caution giving up after tmo
		else
			set r to display dialog theText with title theTitle default answer "" buttons {"Cancel", "OK"} default button "OK" with icon caution giving up after tmo
		end if
	on error number -128
		return "CANCEL"
	end try
	if gave up of r then return "TIMEOUT"
	if button returned of r is not "OK" then return "CANCEL"
	return "OK:" & (text returned of r)
end run`

func runOsascript(ctx context.Context, path string, req Request) ([]byte, error) {
	hidden := "0"
	if req.Hidden {
		hidden = "1"
	}
	secs := strconv.Itoa(int(req.timeout().Seconds()))
	cmd := exec.CommandContext(ctx, path, "-e", osascriptSrc,
		req.text(), req.title(), hidden, secs)
	out, err := cmd.Output()
	if err != nil {
		return nil, guiFailure("osascript", err)
	}
	return parseTaggedResult("osascript", string(out))
}

// parseTaggedResult reads the OK:/CANCEL/TIMEOUT contract that our own osascript
// and PowerShell front-ends emit.
//
// This is the positive-evidence rule made concrete, and it is why the scripts
// print a tag at all instead of just the value. The exit status cannot carry this
// information: `osascript ... giving up after N` exits 0 whether the human typed a
// password or walked away, so a helper that trusts the exit code turns "nobody was
// there" into a successful empty answer. The tag makes the three outcomes
// distinguishable, and an unrecognised line is an ERROR rather than a fourth,
// silently-empty outcome.
func parseTaggedResult(who, raw string) ([]byte, error) {
	line := strings.TrimRight(raw, "\r\n")
	switch {
	case line == "CANCEL":
		return nil, ErrDeclined
	case line == "TIMEOUT":
		return nil, ErrTimeout
	case strings.HasPrefix(line, "OK:"):
		return answered([]byte(strings.TrimPrefix(line, "OK:")))
	default:
		return nil, fmt.Errorf("ctty: %s returned an unrecognised result %q", who, line)
	}
}

func runZenity(ctx context.Context, path string, req Request) ([]byte, error) {
	args := []string{
		"--entry",
		"--title=" + req.title(),
		"--text=" + req.text(),
		"--timeout=" + strconv.Itoa(int(req.timeout().Seconds())),
	}
	if req.Hidden {
		args = append(args, "--hide-text")
	}
	out, err := exec.CommandContext(ctx, path, args...).Output()
	if err != nil {
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			switch ee.ExitCode() {
			case 1: // the user pressed Cancel or closed the window
				return nil, ErrDeclined
			case 5: // --timeout elapsed
				return nil, ErrTimeout
			}
		}
		return nil, guiFailure("zenity", err)
	}
	return answered(trimAnswer(out))
}

func runKDialog(ctx context.Context, path string, req Request) ([]byte, error) {
	verb := "--inputbox"
	if req.Hidden {
		verb = "--password"
	}
	out, err := exec.CommandContext(ctx, path, verb, req.text(), "--title", req.title()).Output()
	if err != nil {
		if ee, ok := errors.AsType[*exec.ExitError](err); ok && ee.ExitCode() == 1 {
			return nil, ErrDeclined
		}
		return nil, guiFailure("kdialog", err)
	}
	return answered(trimAnswer(out))
}

// runSSHAskpass drives the conventional helper: prompt as argv[1], value on
// stdout, non-zero exit means the user declined.
func runSSHAskpass(ctx context.Context, path string, req Request) ([]byte, error) {
	out, err := exec.CommandContext(ctx, path, req.text()).Output()
	if err != nil {
		if _, ok := errors.AsType[*exec.ExitError](err); ok {
			return nil, ErrDeclined
		}
		return nil, guiFailure("askpass helper", err)
	}
	return answered(trimAnswer(out))
}

// runPinentry speaks Assuan.
//
// pinentry is the most trustworthy helper available when it is present — it is
// what gpg uses, it knows how to grab the keyboard, and on some desktops it is
// the only thing that can prompt above a full-screen window. The protocol is
// line-oriented: commands in, status lines out, the value arriving on a `D` line.
func runPinentry(ctx context.Context, path string, req Request) ([]byte, error) {
	cmd := exec.CommandContext(ctx, path)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, guiFailure("pinentry", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, guiFailure("pinentry", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, guiFailure("pinentry", err)
	}
	defer func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	}()

	br := bufio.NewReader(stdout)
	if _, err := br.ReadString('\n'); err != nil { // the greeting
		return nil, guiFailure("pinentry", err)
	}
	for _, line := range []string{
		"SETTITLE " + assuanEscape(req.title()),
		"SETDESC " + assuanEscape(req.text()),
		"SETPROMPT " + assuanEscape(shortPrompt(req.Prompt)),
		"SETTIMEOUT " + strconv.Itoa(int(req.timeout().Seconds())),
		"GETPIN",
	} {
		if _, err := fmt.Fprintf(stdin, "%s\n", line); err != nil {
			return nil, guiFailure("pinentry", err)
		}
		// Every command but GETPIN answers with a single OK/ERR; GETPIN answers
		// with an optional D line then OK. Reading the response for each keeps the
		// stream in sync — skipping them makes the D line arrive out of order.
		if strings.HasPrefix(line, "GETPIN") {
			break
		}
		if _, err := br.ReadString('\n'); err != nil {
			return nil, guiFailure("pinentry", err)
		}
	}

	return parsePinentryGetpin(br)
}

// parsePinentryGetpin reads the response to GETPIN.
//
// Assuan answers with an optional `D <value>` data line followed by `OK`, or with
// `ERR <code> <text>`. The load-bearing case is the middle one: an `OK` with NO
// preceding `D` line is pinentry saying the user submitted an empty entry. That
// must be a decline, not a successful empty secret — the same rule as the tagged
// helpers, arriving in a different shape.
func parsePinentryGetpin(br *bufio.Reader) ([]byte, error) {
	var value []byte
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, guiFailure("pinentry", err)
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(line, "D "):
			value = []byte(assuanUnescape(strings.TrimPrefix(line, "D ")))
		case line == "OK" || strings.HasPrefix(line, "OK "):
			return answered(value)
		case strings.HasPrefix(line, "ERR "):
			// pinentry reports a user cancel as an ERR, so this is the normal
			// "no thanks" path and not a malfunction.
			return nil, ErrDeclined
		}
	}
}

// assuanEscape percent-encodes the characters Assuan reserves. Without this a
// newline in the frame would terminate the command line early and the rest of the
// frame would be interpreted as protocol.
func assuanEscape(s string) string {
	r := strings.NewReplacer("%", "%25", "\n", "%0A", "\r", "%0D")
	return r.Replace(s)
}

func assuanUnescape(s string) string {
	r := strings.NewReplacer("%0A", "\n", "%0a", "\n", "%0D", "\r", "%0d", "\r", "%25", "%")
	return r.Replace(s)
}

// shortPrompt keeps the pinentry input label to one short line; the full frame is
// already in SETDESC.
func shortPrompt(s string) string {
	s = strings.TrimSpace(strings.SplitN(s, "\n", 2)[0])
	if s == "" {
		return "Value:"
	}
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}

// powerShellSrc shows a WinForms prompt. Like the AppleScript, the caller-supplied
// text arrives as an argument rather than being interpolated into the source.
const powerShellSrc = `
param([string]$Text, [string]$Title, [string]$Hidden)
Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing
$f = New-Object System.Windows.Forms.Form
$f.Text = $Title; $f.Width = 560; $f.Height = 260; $f.TopMost = $true
$f.StartPosition = 'CenterScreen'
$l = New-Object System.Windows.Forms.Label
$l.Text = $Text; $l.SetBounds(12,12,520,140)
$t = New-Object System.Windows.Forms.TextBox
$t.SetBounds(12,160,520,24)
if ($Hidden -eq '1') { $t.UseSystemPasswordChar = $true }
$ok = New-Object System.Windows.Forms.Button
$ok.Text = 'OK'; $ok.SetBounds(360,192,80,26); $ok.DialogResult = 'OK'
$cx = New-Object System.Windows.Forms.Button
$cx.Text = 'Cancel'; $cx.SetBounds(452,192,80,26); $cx.DialogResult = 'Cancel'
$f.Controls.AddRange(@($l,$t,$ok,$cx))
$f.AcceptButton = $ok; $f.CancelButton = $cx
$r = $f.ShowDialog()
if ($r -ne [System.Windows.Forms.DialogResult]::OK) { Write-Output 'CANCEL'; exit 0 }
Write-Output ('OK:' + $t.Text)
`

func runPowerShell(ctx context.Context, path string, req Request) ([]byte, error) {
	hidden := "0"
	if req.Hidden {
		hidden = "1"
	}
	cmd := exec.CommandContext(ctx, path,
		"-NoProfile", "-NonInteractive", "-STA", "-Command", powerShellSrc,
		"-Text", req.text(), "-Title", req.title(), "-Hidden", hidden)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, guiFailure("powershell", err)
	}
	return parseTaggedResult("powershell", out.String())
}

// guiFailure maps a helper malfunction to ErrNoGUI so the caller falls through to
// the next channel. A broken helper must never look like a declined prompt, and it
// must never look like an answer.
func guiFailure(what string, err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrTimeout
	}
	return fmt.Errorf("%w (%s: %v)", ErrNoGUI, what, err)
}
