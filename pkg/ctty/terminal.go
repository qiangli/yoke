package ctty

import (
	"bufio"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"

	"golang.org/x/term"
)

// Terminal is an open handle on the CONTROLLING terminal — deliberately not on
// stdin/stdout, which under a harness belong to the harness.
//
// On unix in and out are the same file (/dev/tty opened O_RDWR). On Windows the
// console is split, so they are the separate CONIN$ and CONOUT$ handles; keeping
// two fields rather than one is what lets the same code serve both.
type Terminal struct {
	in  *os.File
	out *os.File
}

// Write sends to the terminal's output side, so a Terminal can be passed anywhere
// an io.Writer is wanted and the bytes still land in front of the human rather
// than in the harness's captured stdout.
func (t *Terminal) Write(p []byte) (int, error) { return t.out.Write(p) }

// Close releases the handles. The two-file case must not double-close when both
// fields alias one open file, which is why the pointers are compared rather than
// a bool being tracked.
func (t *Terminal) Close() error {
	err := t.in.Close()
	if t.out != t.in {
		if oerr := t.out.Close(); err == nil {
			err = oerr
		}
	}
	return err
}

// ReadSecret prompts and reads one line with echo DISABLED.
//
// The prompt goes to the terminal, never to stderr: stderr is the harness's pipe,
// so a prompt written there is recorded in a transcript and shown to nobody.
func (t *Terminal) ReadSecret(prompt string) ([]byte, error) {
	fd := int(t.in.Fd())
	if !term.IsTerminal(fd) {
		return nil, ErrNoTTY
	}

	state, err := term.GetState(fd)
	if err != nil {
		return nil, fmt.Errorf("ctty: reading terminal state: %w", err)
	}

	// Restoring echo is not optional and not best-effort.
	//
	// term.ReadPassword restores on a normal return, but a signal arriving while
	// the terminal is in no-echo mode kills the process with echo still off — and
	// the user is left at a shell that silently swallows everything they type,
	// with no indication why. It is one of the most user-hostile bugs a CLI can
	// ship, and it is entirely avoidable.
	//
	// So: restore on the normal path (defer), restore on a signal (handler), and
	// make the two idempotent with a Once so the racing pair cannot double-restore
	// a terminal some later prompt has since re-configured.
	restore := sync.OnceFunc(func() { _ = term.Restore(fd, state) })
	defer restore()

	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, interruptSignals()...)
	defer signal.Stop(sigc)
	go func() {
		sig, ok := <-sigc
		if !ok {
			return
		}
		restore()
		fmt.Fprintln(t.out)
		// Re-raise rather than exiting with a made-up status: a shell that sees
		// 130 knows the user pressed Ctrl-C, and a caller that sees a fabricated
		// exit 1 does not. An honest exit status is evidence; an invented one is
		// the absence of it wearing evidence's clothes.
		signal.Stop(sigc)
		reRaise(sig)
	}()

	if prompt != "" {
		if _, err := fmt.Fprint(t.out, prompt); err != nil {
			return nil, err
		}
	}
	b, err := term.ReadPassword(fd)
	fmt.Fprintln(t.out)
	if err != nil {
		return nil, fmt.Errorf("ctty: reading from the terminal: %w", err)
	}
	return trimAnswer(b), nil
}

// ReadLine prompts and reads one visible line — for questions whose answer is not
// a secret (a hostname, a branch, a yes/no).
func (t *Terminal) ReadLine(prompt string) ([]byte, error) {
	if prompt != "" {
		if _, err := fmt.Fprint(t.out, prompt); err != nil {
			return nil, err
		}
	}
	line, err := bufio.NewReader(t.in).ReadString('\n')
	if err != nil && line == "" {
		return nil, fmt.Errorf("ctty: reading from the terminal: %w", err)
	}
	return trimAnswer([]byte(line)), nil
}

// trimAnswer strips only the line terminator.
//
// Trailing whitespace is NOT stripped, and that is deliberate: some tokens
// legitimately end in a space or a tab, and silently mangling a credential is
// worse than storing an odd one — the caller gets an authentication failure it
// cannot explain. pkg/secrets settled this same question the same way for its
// pipe path.
func trimAnswer(b []byte) []byte {
	return []byte(strings.TrimRight(string(b), "\r\n"))
}
