// Package ask reaches the HUMAN OPERATOR for an ad-hoc value from inside an
// agent session.
//
// The problem it solves is narrow and concrete. When an agentic CLI needs a
// credential — a token, a password, a one-time code — there is no safe way to hand
// it over. Typing it into the chat input puts the plaintext into the conversation
// transcript, the model's context, and the provider's logs. The workaround
// everybody converges on is a side channel: write the value to /tmp/x and tell the
// agent to read it. That is world-writable, symlink-racy, unbounded in lifetime,
// and entirely manual.
//
// `bashy ask` is that side channel, done properly:
//
//   - the PROMPT reaches the human over a channel the harness does not own
//     (pkg/ctty: the controlling terminal, a GUI askpass, or an out-of-band
//     rendezvous);
//   - the VALUE lands in a mode-0600 file inside a private directory, and the
//     agent receives the PATH on stdout, never the value;
//   - the human sees WHO is asking and WHERE THE ANSWER GOES before they type.
//
// # Scope
//
// This is for AD-HOC values. Durable secrets belong in pkg/secrets, which is the
// cloudbox vault front door; this package deliberately does not store anything
// long-lived and is not a local vault. It is also purely local — no network, no
// pairing, no account — because reading a terminal cannot require a cloud.
//
// # The honest limit
//
// Once the agent reads the file, the value is in the agent's context. You cannot
// keep a secret from the party you are handing it to. What this package protects
// is everything around that: the chat input, the shell history, the process table,
// world-readable disk, and the transcript up until the moment the agent
// deliberately reads the file. That is a real improvement and it is not secrecy
// from the agent — see the design doc for the stronger `--exec` form.
package ask

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/ctty"
)

// options collects the request flags. No option here carries a value: there is no
// positional VALUE and no --shared-secret, because an argument is visible in the
// process table, in shell history, and in bashy's own audit log — which is the
// exact exposure this command exists to remove.
type options struct {
	prompt  string
	name    string
	secret  bool
	timeout time.Duration
	ttl     time.Duration
	channel string
	out     string
	stdout  bool
	json    bool
}

// NewAskCmd returns the `ask` command tree — the host-agnostic entry point a front
// end mounts (e.g. `bashy ask`), mirroring secrets.NewSecretsCmd.
func NewAskCmd() *cobra.Command { return newAskCmd() }

func newAskCmd() *cobra.Command {
	var o options

	cmd := &cobra.Command{
		Use:   "ask",
		Short: "ask the HUMAN operator for a value the agent must not see (never the model)",
		Long: `ask requests a value from the person at the keyboard, over a channel the
calling program does not control, and hands back a path instead of the value.

It exists because a command run by an agentic CLI does not own its stdin or
stdout — both are pipes owned by the harness. A prompt written there is seen by
nobody, and a value written there lands in the transcript, the model's context,
and the provider's logs.

  bashy ask --prompt "GitHub PAT" --name GH_PAT
  /home/you/.bashy/ask/a7f3.../value

The value is written mode 0600 in a private directory and expires. The agent
gets the path; the plaintext never crosses stdout unless you pass --stdout.

Channels are tried in order and the first that reaches a human wins:

  1. your controlling terminal, when this process has one;
  2. a GUI askpass (osascript / pinentry / zenity / kdialog / PowerShell),
     which works even when the harness has detached us from the terminal —
     but only when the session is one you can actually SEE, so a remote SSH
     session never pops a dialog on the far machine's screen;
  3. an out-of-band rendezvous: bashy prints a command, you run it in your own
     terminal and type the value there.

For durable secrets use 'bashy secret', which is the managed vault. This
command is for one-off values and stores nothing long-lived.`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(c *cobra.Command, _ []string) error {
			return runAsk(c, &o)
		},
	}
	cmd.CompletionOptions.DisableDefaultCmd = true

	f := cmd.Flags()
	f.StringVar(&o.prompt, "prompt", "", "message the human sees (sanitized; shown inside a frame that names the requester)")
	f.StringVar(&o.name, "name", "", "label for the value (letters, digits, _ . -)")
	f.BoolVar(&o.secret, "secret", true, "hide the value as it is typed (--secret=false to echo it)")
	f.DurationVar(&o.timeout, "timeout", 2*time.Minute, "how long to wait for the human")
	f.DurationVar(&o.ttl, "ttl", 15*time.Minute, "how long the delivered value survives on disk")
	f.StringVar(&o.channel, "channel", "auto", "force a channel: auto|tty|gui|rendezvous")
	f.StringVar(&o.out, "out", "", "write the value to this path instead (created 0600, never overwritten)")
	f.BoolVar(&o.stdout, "stdout", false, "print the VALUE on stdout — it will enter the calling program's transcript")
	f.BoolVar(&o.json, "json", false, "emit the result as a JSON envelope")

	cmd.AddCommand(newClaimCmd(), newAnswerCmd(), newLsCmd(), newCancelCmd())
	return cmd
}

func runAsk(c *cobra.Command, o *options) error {
	if err := validateName(o.name); err != nil {
		return err
	}
	if o.out != "" && o.stdout {
		return fmt.Errorf("ask: choose one destination — --out or --stdout, not both")
	}

	sink, err := resolveSink(o)
	if err != nil {
		return err
	}

	id, err := newID()
	if err != nil {
		return fmt.Errorf("ask: generating a request id: %w", err)
	}
	now := time.Now()
	r := Request{
		SchemaVersion: SchemaVersion,
		ID:            id,
		Prompt:        sanitizePrompt(o.prompt),
		Name:          o.name,
		Secret:        o.secret,
		Created:       now,
		Expires:       now.Add(o.timeout),
		ValueExpires:  now.Add(o.ttl),
		Sink:          sink,
		Requester:     currentRequester(),
	}

	// Reaping on the way in keeps the store self-cleaning without a daemon, and
	// costs one directory read.
	_, _ = List()

	if err := save(r); err != nil {
		return err
	}
	// Only the successful path keeps the directory: a request that was never
	// answered leaves nothing behind.
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(requestDir(r.ID))
		}
	}()

	value, err := obtain(r, ctty.Channel(o.channel))
	if err != nil {
		return err
	}

	out, err := deliver(r, value)
	if err != nil {
		return err
	}
	keep = r.Sink.Kind == SinkFile

	return report(c, r, out)
}

// obtain walks the channel ladder until one reaches a human.
//
// The probe says which rungs are worth trying; the ladder still tries the next one
// when a rung fails, because the probe and reality can diverge between the check
// and the attempt (a terminal can go away, a helper can be missing an
// already-resolved binary). What must NOT fall through is a rung that reached the
// human and got "no": re-prompting somebody who just cancelled is how a legitimate
// mechanism turns into a nuisance, and then into a phish people click through.
func obtain(r Request, requested ctty.Channel) ([]byte, error) {
	probe := ctty.CurrentProbe(requested)
	req := ctty.Request{
		Frame:   renderFrame(r),
		Prompt:  promptLine(r),
		Title:   "bashy ask" + titleSuffix(r),
		Hidden:  r.Secret,
		Timeout: time.Until(r.Expires),
	}

	var lastErr error
	for _, ch := range rungs(probe) {
		switch ch {
		case ctty.ChannelRendezvous:
			// stderr, not stdout: the request carries no secret, and under a
			// harness stderr is exactly the channel that reaches the model so it
			// can relay the instruction to the human.
			v, err := waitForAnswer(r, os.Stderr)
			if err != nil {
				return nil, err
			}
			return v, nil
		default:
			v, err := ctty.Ask(ch, req)
			if err == nil {
				return v, nil
			}
			if errors.Is(err, ctty.ErrDeclined) {
				return nil, fmt.Errorf("ask: cancelled")
			}
			if errors.Is(err, ctty.ErrTimeout) {
				return nil, fmt.Errorf("ask: nobody answered within %s",
					r.Expires.Sub(r.Created).Round(time.Second))
			}
			// Channel unavailable — try the next rung.
			lastErr = err
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("ask: no channel could reach a human")
}

// rungs orders the channels to attempt.
//
// An explicit --channel is honoured alone, with no fallback: an operator forcing a
// channel is debugging it, and silently rerouting them would hide the very failure
// they are looking at.
func rungs(p ctty.Probe) []ctty.Channel {
	if p.Requested != "" && p.Requested != ctty.ChannelAuto {
		return []ctty.Channel{p.Requested}
	}
	var out []ctty.Channel
	if p.TTY {
		out = append(out, ctty.ChannelTTY)
	}
	if p.GUI {
		out = append(out, ctty.ChannelGUI)
	}
	// Always last, and always present: it is the only rung that works everywhere,
	// so the ladder can never run out of options and leave the caller stuck.
	return append(out, ctty.ChannelRendezvous)
}

func titleSuffix(r Request) string {
	if r.Name == "" {
		return ""
	}
	return " — " + r.Name
}

func resolveSink(o *options) (Sink, error) {
	switch {
	case o.stdout:
		return Sink{Kind: SinkStdout, Detail: "the requesting program's stdout"}, nil
	case o.out != "":
		abs, err := absPath(o.out)
		if err != nil {
			return Sink{}, err
		}
		return Sink{Kind: SinkOut, Detail: abs}, nil
	default:
		return Sink{Kind: SinkFile, Detail: "a private file, path printed on stdout"}, nil
	}
}
