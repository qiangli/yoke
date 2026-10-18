// Package audacity runs Audacity 3.x as a managed external binary (pkg/binmgr)
// in exactly one mode: a short-lived, same-user local mod-script-pipe
// transaction per typed operation. Audacity is GPL-2.0-or-later; this package
// never links, vendors, or modifies it — it is download + sha256-verify +
// exec/pipe only, and the sha256 is a COMMITTED pin (pkg/binmgr/pins.go).
// Without a committed pin for the current platform, provisioning fails closed:
// the release-supplied checksum is never trusted (supply-chain Rule 2: no
// server, no resident mode, no trust-on-first-use).
//
// The surface is four typed operations — info, trim, normalize, export — each
// over explicit ABSOLUTE paths. Every operation runs a FIXED set of Audacity
// scripting commands sent over mod-script-pipe (Audacity 3.x's local named
// pipe), not arbitrary pipe input. There is no network listener, no resident
// bridge/MCP server, no daemon, no Audacity 4 path, and no free-form script
// escape. Package-level tests pin all of that.
//
// Pipe locality: the scripting pipe is a same-user local IPC object
// (/tmp/audacity_script_pipe.* on Unix, \\.\pipe\ToSrvPipe on Windows) owned
// by the same UID that started Audacity. The wrapper verifies ownership
// before connecting and refuses a pipe owned by another user. The pipe is
// never exposed as a TCP listener and the implementation imports no net*.
package audacity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/binmgr"
)

const (
	// DefaultVersion is the pinned Audacity 3.x release. There is
	// no "latest": a different version is an explicit, reviewed bump here
	// plus a matching committed sha256 in pkg/binmgr/pins.go. Audacity 4
	// is explicitly unsupported (separate evaluation).
	DefaultVersion = "3.7.3"

	toolName = "audacity"

	// releaseBase is the official Audacity release root on GitHub.
	releaseBase = "https://github.com/audacity/audacity/releases/download"

	resultMarker = "yoke-audacity-v1 "
)

// ErrNoPin is returned (wrapped) when no committed sha256 exists for the
// requested version on the current platform. Nothing is downloaded.
var ErrNoPin = errors.New("audacity: no committed sha256 pin for this version/platform (pkg/binmgr/pins.go); refusing release-supplied checksum")

// ErrNoArtifact is returned (wrapped) for platforms Audacity publishes no
// archive binmgr can consume (macOS ships only .dmg/.pkg).
var ErrNoArtifact = errors.New("audacity: no extractable official artifact for this platform")

// ErrUnsupportedVersion is returned for Audacity 4.x and other unsupported
// major versions.
var ErrUnsupportedVersion = errors.New("audacity: only Audacity 3.x is supported; Audacity 4 requires separate evaluation")

var versionRe = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)$`)

// Artifact is the official release archive for one platform.
type Artifact struct {
	URL        string
	SHA256     string // official digest from CHECKSUMS.txt, never used as pin (pin in pins.go is authoritative)
	Entrypoint string // slash-separated executable path inside the archive tree; empty for raw binary
	IsTree     bool
}

// ArtifactFor returns the official Audacity archive for version on goos/goarch
// and the executable's path inside it. ok is false where no binmgr-consumable
// artifact exists: darwin (dmg/pkg only) and every arch Audacity does not publish.
func ArtifactFor(version, goos, goarch string) (Artifact, bool) {
	m := versionRe.FindStringSubmatch(version)
	if m == nil {
		return Artifact{}, false
	}
	maj, _ := strconv.Atoi(m[1])
	if maj != 3 {
		return Artifact{}, false
	}
	// Only 3.7.3 is admitted in this story; other 3.x would need a separate
	// reviewed pin. Keep the URL construction generic but let Spec gate on pin.
	tag := "Audacity-" + version
	switch goos + "/" + goarch {
	case "linux/amd64":
		name := fmt.Sprintf("audacity-linux-%s-x64-22.04.AppImage", version)
		return Artifact{URL: fmt.Sprintf("%s/%s/%s", releaseBase, tag, name), IsTree: false}, true
	case "windows/amd64":
		name := fmt.Sprintf("audacity-win-%s-64bit.zip", version)
		stem := fmt.Sprintf("audacity-win-%s-64bit", version)
		return Artifact{
			URL:        fmt.Sprintf("%s/%s/%s", releaseBase, tag, name),
			Entrypoint: stem + "/Audacity.exe",
			IsTree:     true,
		}, true
	}
	return Artifact{}, false
}

// Spec is the binmgr tool declaration for version, carrying an asset ONLY
// for platforms that have both an official archive and a committed sha256
// pin. A platform without a pin is absent from Assets, so binmgr.Ensure
// cannot resolve (let alone download) it — fail closed by construction.
func Spec(version string) (binmgr.Tool, error) {
	if version == "" {
		version = DefaultVersion
	}
	if !versionRe.MatchString(version) {
		return binmgr.Tool{}, fmt.Errorf("audacity: invalid version %q (want MAJOR.MINOR.PATCH)", version)
	}
	m := versionRe.FindStringSubmatch(version)
	maj, _ := strconv.Atoi(m[1])
	if maj != 3 {
		return binmgr.Tool{}, fmt.Errorf("%w: %q", ErrUnsupportedVersion, version)
	}
	assets := map[string]binmgr.Asset{}
	for _, plat := range []string{"linux/amd64", "windows/amd64"} {
		goos, goarch, _ := strings.Cut(plat, "/")
		art, ok := ArtifactFor(version, goos, goarch)
		if !ok {
			continue
		}
		sha, ok := binmgr.PinnedSHA256(toolName, version, plat)
		if !ok {
			continue
		}
		if art.IsTree {
			assets[plat] = binmgr.Asset{URL: art.URL, SHA256: sha, Tree: true, Entrypoint: art.Entrypoint}
		} else {
			assets[plat] = binmgr.Asset{URL: art.URL, SHA256: sha}
		}
	}
	return binmgr.Tool{Name: toolName, Version: version, Assets: assets}, nil
}

// Path provisions (download → verify against the committed pin → cache) and
// returns the Audacity executable for version on the current platform. It
// fails closed, with no network, when the platform has no official
// extractable artifact or no committed pin.
func Path(ctx context.Context, version string) (string, error) {
	return pathFor(ctx, version, runtime.GOOS, runtime.GOARCH)
}

func pathFor(ctx context.Context, version, goos, goarch string) (string, error) {
	tool, err := Spec(version)
	if err != nil {
		return "", err
	}
	if _, ok := ArtifactFor(tool.Version, goos, goarch); !ok {
		return "", fmt.Errorf("%w: %s %s on %s/%s", ErrNoArtifact, toolName, tool.Version, goos, goarch)
	}
	if _, ok := tool.Assets[goos+"/"+goarch]; !ok {
		return "", fmt.Errorf("%w: %s@%s/%s/%s", ErrNoPin, toolName, tool.Version, goos, goarch)
	}
	if goos != runtime.GOOS || goarch != runtime.GOARCH {
		return "", fmt.Errorf("audacity: cannot provision %s/%s from %s/%s", goos, goarch, runtime.GOOS, runtime.GOARCH)
	}
	return binmgr.Ensure(ctx, tool)
}

// Kind names one of the four typed operations.
type Kind string

const (
	KindInfo      Kind = "info"
	KindTrim      Kind = "trim"
	KindNormalize Kind = "normalize"
	KindExport    Kind = "export"
)

// Kinds is the complete operation set, in registration order.
var Kinds = []Kind{KindInfo, KindTrim, KindNormalize, KindExport}

// Request is one typed operation. Only the fields the Kind uses are read.
type Request struct {
	Kind Kind
	// Input is an absolute audio/project file.
	Input string
	// Output is the absolute file to write (trim, normalize, export).
	Output string
	// Start and End are seconds for trim (0 <= Start < End).
	Start float64
	End   float64
	// Level is dB for normalize (-60..0, default -3).
	Level float64
	// LevelSet tracks whether Level was explicitly set (to distinguish 0).
	LevelSet bool
	// Format is export container (wav, mp3, ogg, flac, aiff).
	Format string
}

var (
	audioExt  = map[string]bool{".wav": true, ".mp3": true, ".flac": true, ".ogg": true, ".aiff": true, ".aif": true, ".aup3": true}
	infoExt   = map[string]bool{".wav": true, ".mp3": true, ".flac": true, ".ogg": true, ".aiff": true, ".aif": true, ".aup3": true}
	exportExt = map[string]bool{".wav": true, ".mp3": true, ".ogg": true, ".flac": true, ".aiff": true, ".aif": true}
	exportFmt = map[string]bool{"wav": true, "mp3": true, "ogg": true, "flac": true, "aiff": true, "aif": true}
)

func extAllowed(path string, allowed map[string]bool) error {
	ext := strings.ToLower(filepath.Ext(path))
	if allowed[ext] {
		return nil
	}
	keys := make([]string, 0, len(allowed))
	for k := range allowed {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return fmt.Errorf("extension %q not allowed (want one of %s)", ext, strings.Join(keys, ", "))
}

// absPath checks the shape every path argument must have: non-empty,
// absolute, no NUL/newline, and not an option-looking token.
func absPath(what, p string) error {
	if p == "" {
		return fmt.Errorf("%s: required", what)
	}
	if strings.ContainsAny(p, "\x00\n\r") {
		return fmt.Errorf("%s: contains a control character", what)
	}
	if !filepath.IsAbs(p) {
		return fmt.Errorf("%s: %q is not an absolute path", what, p)
	}
	if strings.HasPrefix(p, "-") {
		return fmt.Errorf("%s: %q looks like an option", what, p)
	}
	return nil
}

func existingFile(what, p string, allowed map[string]bool) error {
	if err := absPath(what, p); err != nil {
		return err
	}
	if err := extAllowed(p, allowed); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	st, err := os.Stat(p)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("%s: %q is not a regular file", what, p)
	}
	return nil
}

func writableTarget(what, p string, allowed map[string]bool) error {
	if err := absPath(what, p); err != nil {
		return err
	}
	if err := extAllowed(p, allowed); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	st, err := os.Stat(filepath.Dir(p))
	if err != nil {
		return fmt.Errorf("%s: parent directory: %w", what, err)
	}
	if !st.IsDir() {
		return fmt.Errorf("%s: parent %q is not a directory", what, filepath.Dir(p))
	}
	if st, err := os.Stat(p); err == nil && st.IsDir() {
		return fmt.Errorf("%s: %q is a directory", what, p)
	}
	return nil
}

// Validate checks every argument BEFORE anything is resolved or executed.
func (r Request) Validate() error {
	switch r.Kind {
	case KindInfo:
		if r.Output != "" || r.Start != 0 || r.End != 0 || r.LevelSet || r.Format != "" {
			return errors.New("info: takes only --input")
		}
		return existingFile("--input", r.Input, infoExt)
	case KindTrim:
		if r.Format != "" || r.LevelSet {
			return errors.New("trim: does not take --format or --level")
		}
		if err := existingFile("--input", r.Input, audioExt); err != nil {
			return err
		}
		if err := writableTarget("--output", r.Output, exportExt); err != nil {
			return err
		}
		if filepath.Clean(r.Input) == filepath.Clean(r.Output) {
			return errors.New("trim: --output must differ from --input")
		}
		if r.Start < 0 {
			return fmt.Errorf("--start: %v is negative", r.Start)
		}
		if r.End <= r.Start {
			return fmt.Errorf("--end: %v must be greater than --start %v", r.End, r.Start)
		}
		if r.End-r.Start < 0.01 {
			return fmt.Errorf("--end: interval too small (need >= 0.01s)")
		}
		return nil
	case KindNormalize:
		if r.Format != "" || r.Start != 0 || r.End != 0 {
			return errors.New("normalize: does not take --format, --start or --end")
		}
		if err := existingFile("--input", r.Input, audioExt); err != nil {
			return err
		}
		if err := writableTarget("--output", r.Output, exportExt); err != nil {
			return err
		}
		if filepath.Clean(r.Input) == filepath.Clean(r.Output) {
			return errors.New("normalize: --output must differ from --input")
		}
		lvl := r.Level
		if !r.LevelSet {
			lvl = -3
		}
		if lvl > 0 || lvl < -60 {
			return fmt.Errorf("--level: %v out of range [-60, 0] dB", lvl)
		}
		return nil
	case KindExport:
		if r.Start != 0 || r.End != 0 || r.LevelSet {
			return errors.New("export: does not take --start, --end or --level")
		}
		if err := existingFile("--input", r.Input, audioExt); err != nil {
			return err
		}
		if err := writableTarget("--output", r.Output, exportExt); err != nil {
			return err
		}
		if filepath.Clean(r.Input) == filepath.Clean(r.Output) {
			return errors.New("export: --output must differ from --input")
		}
		if r.Format != "" {
			f := strings.ToLower(r.Format)
			if !exportFmt[f] {
				keys := make([]string, 0, len(exportFmt))
				for k := range exportFmt {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				return fmt.Errorf("--format: %q not allowed (want one of %s)", r.Format, strings.Join(keys, ", "))
			}
			if ext := strings.ToLower(filepath.Ext(r.Output)); ext != "."+f && !(f == "aiff" && ext == ".aif") && !(f == "aif" && ext == ".aiff") {
				return fmt.Errorf("--format %q does not match --output extension %q", f, ext)
			}
		}
		return nil
	}
	return fmt.Errorf("audacity: unknown operation %q", string(r.Kind))
}

// PipeCommands returns the FIXED Audacity scripting command sequence for the
// request. Each string is one pipe line (e.g. `Import2: Filename=...`).
// Paths travel as quoted Filename values; there is no free-form pipe input.
func (r Request) PipeCommands() ([]string, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	switch r.Kind {
	case KindInfo:
		return []string{
			fmt.Sprintf("Import2: Filename=%s", quotePipeArg(filepath.Clean(r.Input))),
			"GetInfo: Type=Tracks Format=JSON",
		}, nil
	case KindTrim:
		return []string{
			fmt.Sprintf("Import2: Filename=%s", quotePipeArg(filepath.Clean(r.Input))),
			fmt.Sprintf("Select: Start=%s End=%s", formatSec(r.Start), formatSec(r.End)),
			"Trim:",
			fmt.Sprintf("Export2: Filename=%s", quotePipeArg(filepath.Clean(r.Output))),
		}, nil
	case KindNormalize:
		lvl := -3.0
		if r.LevelSet {
			lvl = r.Level
		}
		return []string{
			fmt.Sprintf("Import2: Filename=%s", quotePipeArg(filepath.Clean(r.Input))),
			"SelectAll:",
			fmt.Sprintf("LoudnessNormalization: Level=%s DualMono=0", formatDB(lvl)),
			fmt.Sprintf("Export2: Filename=%s", quotePipeArg(filepath.Clean(r.Output))),
		}, nil
	case KindExport:
		return []string{
			fmt.Sprintf("Import2: Filename=%s", quotePipeArg(filepath.Clean(r.Input))),
			fmt.Sprintf("Export2: Filename=%s", quotePipeArg(filepath.Clean(r.Output))),
		}, nil
	}
	return nil, fmt.Errorf("audacity: unknown operation %q", string(r.Kind))
}

func quotePipeArg(p string) string {
	// Audacity pipe quoting: double-quoted with internal double quotes escaped
	// by doubling? The pipe docs use: Filename="C:\path with spaces\file.wav"
	// We use %q style but ensure backslashes preserved.
	return strconv.Quote(p)
}

func formatSec(v float64) string {
	return strconv.FormatFloat(v, 'f', 6, 64)
}

func formatDB(v float64) string {
	return strconv.FormatFloat(v, 'f', 2, 64)
}

// EffectsFor returns the audacity operation's effect tokens for the
// registered-command contract. Every op is at least a local read; writes
// add a local write; each involves a local process (the Audacity pipe
// transaction) but no network or remote write.
func EffectsFor(k Kind) []string {
	switch k {
	case KindInfo:
		return []string{"read", "process"}
	case KindTrim, KindNormalize, KindExport:
		return []string{"read", "write", "process"}
	default:
		return []string{"read"}
	}
}

// PipeClient is the same-user local mod-script-pipe transport.
// Implementations must refuse pipes owned by another UID and must never
// dial a network address.
type PipeClient func(ctx context.Context, commands []string) (string, error)

// Resolver returns the Audacity executable for a version (default: Path).
type Resolver func(ctx context.Context, version string) (string, error)

// pipeDirOverride, when non-empty, replaces /tmp for pipe resolution.
// Tests set this to t.TempDir() to avoid touching the global /tmp.
var pipeDirOverride string

func pipePaths() (string, string) {
	dir := "/tmp"
	if pipeDirOverride != "" {
		dir = pipeDirOverride
	}
	uid := os.Getuid()
	return filepath.Join(dir, fmt.Sprintf("audacity_script_pipe.to.%d", uid)),
		filepath.Join(dir, fmt.Sprintf("audacity_script_pipe.from.%d", uid))
}

// verifyPipe and the fifo type are platform-specific. See pipe_unix.go
// (Unix FIFO: ownership via syscall.Stat_t UID; raw O_NONBLOCK descriptors
// so every wait is a select on ctx.Done() and no goroutine is ever parked
// in a blocking open/read/write) and pipe_windows.go (explicit fail-closed:
// Windows named-pipe transport is not implemented in this story, so
// defaultPipeClient rejects before reaching the pipe layer; see EVIDENCE.md).

const (
	// batchFinishedPrefix is Audacity's documented per-command reply
	// terminator line: "BatchCommand finished: OK" or
	// "BatchCommand finished: Failed!" (mod-script-pipe, ScriptCommandRelay).
	batchFinishedPrefix = "BatchCommand finished:"

	// maxReplyBytes bounds ONE command's reply. A reply that long without
	// its finished marker is not Audacity talking; the session aborts
	// rather than buffer without bound.
	maxReplyBytes = 64 * 1024

	// maxLineBytes bounds a single reply line (GetInfo JSON is one line).
	maxLineBytes = maxReplyBytes
)

// errEOF is the FIFO's zero-length read; kept distinct from io.EOF so the
// error text names the pipe rather than a generic stream.
var errEOF = errors.New("audacity: pipe closed by Audacity")

// errReplyTooLarge is returned when one command's reply exceeds maxReplyBytes
// without a BatchCommand finished marker.
var errReplyTooLarge = errors.New("audacity: pipe reply exceeded bound without BatchCommand finished marker")

// pipeSession is ONE open same-user FIFO pair, held for the duration of one
// typed operation. It is the shape of Audacity's official reference client
// (au3/scripts/piped-work/pipe_test.py): open TOFILE and FROMFILE once,
// then for EACH command do_command = send_command + get_response — write
// the command plus newline, read that command's reply through its
// "BatchCommand finished:" line, and only then send the next command.
//
// Audacity's pipe server is strictly request/reply: mod-script-pipe reads
// one line, executes it on the GUI thread, writes the reply, and only then
// reads the next line. Writing every command up front and reading once
// (the rejected prior protocol) interleaves unread replies with pending
// commands, loses every reply but the first to the 64 KiB bound, and
// cannot stop at a Failed step — so Export2 still runs after a failed
// Import2. The session enforces: no command N+1 is written before reply N
// has been read in full, and a Failed reply aborts the operation at once.
type pipeSession struct {
	to, from *fifo
	// pending holds bytes read from the FIFO that belong to a line not yet
	// consumed; it carries across replies (Audacity terminates each reply
	// with an extra blank line, which the next readReply skips).
	pending []byte
}

// openPipeSession opens the to/from FIFO pair once, under ctx.
func openPipeSession(ctx context.Context, toPipe, fromPipe string) (*pipeSession, error) {
	to, err := openFIFOForWrite(ctx, toPipe)
	if err != nil {
		return nil, fmt.Errorf("audacity: open pipe for write %q: %w", toPipe, err)
	}
	from, err := openFIFOForRead(ctx, fromPipe)
	if err != nil {
		to.Close()
		return nil, fmt.Errorf("audacity: open pipe for read %q: %w", fromPipe, err)
	}
	return &pipeSession{to: to, from: from}, nil
}

func (s *pipeSession) Close() {
	if s == nil {
		return
	}
	if s.to != nil {
		s.to.Close()
	}
	if s.from != nil {
		s.from.Close()
	}
}

// doCommand is pipe_test.py's do_command: send ONE command, then read ITS
// reply through the BatchCommand finished marker. It returns the reply
// (marker line included) and the status text after the marker.
func (s *pipeSession) doCommand(ctx context.Context, command string) (reply, status string, err error) {
	if err := s.to.writeAll(ctx, []byte(command+"\n")); err != nil {
		return "", "", fmt.Errorf("audacity: pipe write failed: %w", err)
	}
	return s.readReply(ctx)
}

// readLine returns the next newline-terminated line (newline included),
// pulling more bytes from the FIFO as needed, bounded by ctx and maxLineBytes.
func (s *pipeSession) readLine(ctx context.Context) (string, error) {
	buf := make([]byte, 4096)
	for {
		if i := bytes.IndexByte(s.pending, '\n'); i >= 0 {
			line := string(s.pending[:i+1])
			s.pending = append([]byte(nil), s.pending[i+1:]...)
			return line, nil
		}
		if len(s.pending) > maxLineBytes {
			return "", errReplyTooLarge
		}
		n, err := s.from.readSome(ctx, buf)
		if n > 0 {
			s.pending = append(s.pending, buf[:n]...)
		}
		if err != nil {
			return "", err
		}
	}
}

// readReply reads one command's reply: every line up to and including the
// first line that begins with "BatchCommand finished:". Blank lines BEFORE
// any content are the previous reply's trailing terminator and are skipped;
// blank lines inside a reply are kept.
func (s *pipeSession) readReply(ctx context.Context) (reply, status string, err error) {
	var out strings.Builder
	for {
		line, err := s.readLine(ctx)
		if err != nil {
			return out.String(), "", fmt.Errorf("audacity: pipe read failed: %w", err)
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(trimmed, batchFinishedPrefix) {
			out.WriteString(line)
			return out.String(), strings.TrimSpace(strings.TrimPrefix(trimmed, batchFinishedPrefix)), nil
		}
		if trimmed == "" && out.Len() == 0 {
			continue
		}
		out.WriteString(line)
		if out.Len() > maxReplyBytes {
			return out.String(), "", errReplyTooLarge
		}
	}
}

// containsBatchFinished reports whether s contains a line that begins with
// "BatchCommand finished:" (the documented Audacity mod-script-pipe reply
// terminator). It tolerates the blank line that precedes the marker and is
// safe when the marker is split across read chunks because s is the
// accumulated buffer, not a single chunk.
func containsBatchFinished(s string) bool {
	for _, line := range strings.Split(s, "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, batchFinishedPrefix) {
			return true
		}
	}
	return false
}

// runCommands drives the fixed command list over one session in strict
// request/reply order and returns the concatenated per-command replies.
// The first "Failed" reply aborts the operation immediately: the remaining
// commands are never written. Exported for tests through defaultPipeClient.
func runCommands(ctx context.Context, s *pipeSession, commands []string) (string, error) {
	var all strings.Builder
	for i, cmd := range commands {
		reply, status, err := s.doCommand(ctx, cmd)
		all.WriteString(reply)
		if err != nil {
			return all.String(), fmt.Errorf("command %d/%d (%s): %w", i+1, len(commands), commandID(cmd), err)
		}
		if strings.HasPrefix(status, "Failed") {
			return all.String(), fmt.Errorf("audacity: command %d/%d (%s) failed: BatchCommand finished: %s", i+1, len(commands), commandID(cmd), status)
		}
	}
	return all.String(), nil
}

// commandID is the scripting ID before the colon (never the quoted paths),
// safe to put in an error message.
func commandID(cmd string) string {
	if i := strings.IndexByte(cmd, ':'); i >= 0 {
		return cmd[:i]
	}
	return cmd
}

// defaultPipeClient is the production PipeClient: a bounded, same-user local
// FIFO session. It verifies both pipes are FIFOs owned by the caller UID,
// opens the pair once, then sends the fixed commands one at a time, each
// followed by reading its own reply (pipe_test.py protocol). No network, no
// daemon.
func defaultPipeClient(ctx context.Context, commands []string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if len(commands) == 0 {
		return "", errors.New("audacity: no commands")
	}
	if runtime.GOOS == "windows" {
		return "", errors.New("audacity: no running Audacity 3.x with mod-script-pipe found (start Audacity, enable mod-script-pipe in Preferences → Modules, restart)")
	}
	uid := os.Getuid()
	toPipe, fromPipe := pipePaths()
	if err := verifyPipe(toPipe, uid); err != nil {
		return "", err
	}
	if err := verifyPipe(fromPipe, uid); err != nil {
		return "", err
	}
	// Enforce a bounded timeout if caller provided none (30s default).
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
	}
	s, err := openPipeSession(ctx, toPipe, fromPipe)
	if err != nil {
		return "", err
	}
	defer s.Close()
	resp, err := runCommands(ctx, s, commands)
	if err != nil {
		return "", err
	}
	if resp == "" {
		return "", errors.New("audacity: empty response from Audacity pipe")
	}
	return resp, nil
}

type runner struct {
	resolve Resolver
	pipe    PipeClient
}

var defaultRunner = &runner{resolve: Path, pipe: defaultPipeClient}

// Run validates r, provisions Audacity, executes the fixed pipe transaction
// under ctx, and writes a single yoke-audacity-v1 JSON line to out.
func Run(ctx context.Context, version string, r Request, out io.Writer) error {
	return defaultRunner.run(ctx, version, r, out)
}

func (rn *runner) run(ctx context.Context, version string, r Request, out io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := r.Validate(); err != nil {
		return err
	}
	if cmds, err := r.PipeCommands(); err != nil {
		return err
	} else if len(cmds) == 0 {
		return errors.New("audacity: no commands for operation")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if version == "" {
		version = DefaultVersion
	}
	// Reject Audacity 4 explicitly.
	if m := versionRe.FindStringSubmatch(version); m != nil {
		if maj, _ := strconv.Atoi(m[1]); maj != 3 {
			return fmt.Errorf("%w: %q", ErrUnsupportedVersion, version)
		}
	}
	bin, err := rn.resolve(ctx, version)
	if err != nil {
		return err
	}
	_ = bin // provisioning verified; pipe operations use the running instance
	cmds, _ := r.PipeCommands()
	var buf bytes.Buffer
	// Timeout for the pipe transaction (bounded; does not create a daemon).
	pipeCtx := ctx
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		pipeCtx, cancel = context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
	}
	resp, err := rn.pipe(pipeCtx, cmds)
	if err != nil {
		return fmt.Errorf("audacity %s: %w", r.Kind, err)
	}
	// Normalize response to a single JSON line with schema discriminator.
	result, err := buildResult(r, resp)
	if err != nil {
		return err
	}
	_, err = io.WriteString(&buf, result+"\n")
	if err != nil {
		return err
	}
	_, err = io.WriteString(out, buf.String())
	return err
}

func buildResult(r Request, pipeResponse string) (string, error) {
	// The pipe response is Audacity's "BatchCommand finished: OK" plus
	// JSON payloads for GetInfo etc. We wrap whatever we got into a
	// versioned envelope. Live verification will assert the envelope parses
	// and carries the expected fields; fixture tests pin the envelope shape.
	m := map[string]any{
		"schema_version": "yoke-audacity-v1",
		"op":             string(r.Kind),
		"input":          filepath.Clean(r.Input),
	}
	if r.Output != "" {
		m["output"] = filepath.Clean(r.Output)
	}
	switch r.Kind {
	case KindTrim:
		m["start"] = r.Start
		m["end"] = r.End
	case KindNormalize:
		lvl := -3.0
		if r.LevelSet {
			lvl = r.Level
		}
		m["level"] = lvl
	case KindExport:
		if r.Format != "" {
			m["format"] = strings.ToLower(r.Format)
		}
	}
	// Include truncated pipe output for debugging (first 2k).
	if pipeResponse != "" {
		trunc := pipeResponse
		if len(trunc) > 2048 {
			trunc = trunc[:2048] + "…"
		}
		m["pipe_response"] = trunc
	}
	// Validate it marshals.
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	// Ensure the line is prefixed for filtering like blender.
	return resultMarker + string(b), nil
}

func findResult(stdout string) (string, bool) {
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, resultMarker) {
			return strings.TrimPrefix(line, resultMarker), true
		}
	}
	return "", false
}

// NewCmd is the `audacity` front door: a parent with exactly the four typed
// subcommands. No pass-through, no `run`, no `python`, no `serve`.
func NewCmd() *cobra.Command { return newCmd(defaultRunner) }

func newCmd(rn *runner) *cobra.Command {
	root := &cobra.Command{
		Use:   "audacity",
		Short: "Typed Audacity 3.x operations (info, trim, normalize, export) over mod-script-pipe",
		Long: `Run Audacity 3.x as a managed external (pkg/binmgr) via its same-user
local mod-script-pipe. Audacity is GPL-2.0-or-later and is downloaded and
executed, never linked. Provisioning fails closed without a committed sha256
pin. Audacity 4 is not supported.

Each operation runs a FIXED set of Audacity scripting commands over the
local pipe (no free-form pipe input, no network listener, no daemon/MCP
server). The pipe is same-user local only.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		RunE:          func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	var version string
	var timeout time.Duration
	root.PersistentFlags().StringVar(&version, "audacity-version", DefaultVersion, "pinned Audacity 3.x release (must have a committed sha256)")
	root.PersistentFlags().DurationVar(&timeout, "timeout", 0, "optional deadline for the pipe transaction (0 = 30s default)")

	exec := func(c *cobra.Command, r Request) error {
		ctx := c.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		if timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}
		return rn.run(ctx, version, r, c.OutOrStdout())
	}

	var info Request
	infoCmd := &cobra.Command{
		Use:   "info --input /abs/audio.wav",
		Short: "Print track/clip info of an audio file or .aup3 project as JSON",
		Args:  cobra.NoArgs,
		RunE:  func(c *cobra.Command, _ []string) error { info.Kind = KindInfo; return exec(c, info) },
	}
	infoCmd.Flags().StringVar(&info.Input, "input", "", "absolute path to the audio file or .aup3 to inspect (required)")
	_ = infoCmd.MarkFlagRequired("input")

	var trim Request
	trimCmd := &cobra.Command{
		Use:   "trim --input /abs/in.wav --output /abs/out.wav --start 1.0 --end 5.0",
		Short: "Trim an audio file to [start,end) seconds and export",
		Args:  cobra.NoArgs,
		RunE:  func(c *cobra.Command, _ []string) error { trim.Kind = KindTrim; return exec(c, trim) },
	}
	trimCmd.Flags().StringVar(&trim.Input, "input", "", "absolute path to the input audio (required)")
	trimCmd.Flags().StringVar(&trim.Output, "output", "", "absolute path of the output audio to write (required)")
	trimCmd.Flags().Float64Var(&trim.Start, "start", 0, "start time in seconds (>=0)")
	trimCmd.Flags().Float64Var(&trim.End, "end", 0, "end time in seconds (> start)")
	_ = trimCmd.MarkFlagRequired("input")
	_ = trimCmd.MarkFlagRequired("output")
	_ = trimCmd.MarkFlagRequired("end")

	var norm Request
	normCmd := &cobra.Command{
		Use:   "normalize --input /abs/in.wav --output /abs/out.wav [--level -3.0]",
		Short: "Normalize loudness of an audio file and export",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			norm.Kind = KindNormalize
			// Detect whether --level was explicitly set.
			norm.LevelSet = c.Flags().Changed("level")
			return exec(c, norm)
		},
	}
	normCmd.Flags().StringVar(&norm.Input, "input", "", "absolute path to the input audio (required)")
	normCmd.Flags().StringVar(&norm.Output, "output", "", "absolute path of the output audio to write (required)")
	normCmd.Flags().Float64Var(&norm.Level, "level", -3, "target level in dB [-60,0] (default -3)")
	_ = normCmd.MarkFlagRequired("input")
	_ = normCmd.MarkFlagRequired("output")

	var exp Request
	exportCmd := &cobra.Command{
		Use:   "export --input /abs/project.aup3 --output /abs/out.wav [--format wav]",
		Short: "Export/convert an audio file or project to wav/mp3/ogg/flac/aiff",
		Args:  cobra.NoArgs,
		RunE:  func(c *cobra.Command, _ []string) error { exp.Kind = KindExport; return exec(c, exp) },
	}
	exportCmd.Flags().StringVar(&exp.Input, "input", "", "absolute path to the input audio or .aup3 (required)")
	exportCmd.Flags().StringVar(&exp.Output, "output", "", "absolute path of the output audio to write (required)")
	exportCmd.Flags().StringVar(&exp.Format, "format", "", "export format (wav, mp3, ogg, flac, aiff); default inferred from --output extension")
	_ = exportCmd.MarkFlagRequired("input")
	_ = exportCmd.MarkFlagRequired("output")

	root.AddCommand(infoCmd, trimCmd, normCmd, exportCmd)
	return root
}
