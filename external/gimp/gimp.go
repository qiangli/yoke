// Package gimp runs GIMP as a managed external binary (pkg/binmgr) in
// exactly one mode: a short-lived, foreground batch child per typed
// operation. GIMP (GNOME/gimp, GPL-3.0-or-later) is never linked, vendored,
// or modified — it is download + sha256-verify + exec only, and the sha256
// is a COMMITTED pin (pkg/binmgr/pins.go). Without a committed pin for the
// current platform, provisioning fails closed: the release-supplied
// checksum manifest is never trusted (supply-chain Rule 2: no server, no
// resident mode, no trust-on-first-use).
//
// The surface is three typed operations — export, convert, edit — each over
// explicit ABSOLUTE paths. Every operation runs a FIXED Python-Fu program
// (built by programFor from a constant template; only validated paths,
// range-checked integers and closed-vocabulary enums are substituted) passed
// via a single --batch under --batch-interpreter python-fu-eval, with
// --quit so no GIMP process outlives the child. There is no free-form
// program, no pass-through of arbitrary GIMP arguments, no plug-in
// installation, no resident plug-in host, no MCP server, no listener, and
// no GUI. Package-level tests pin all of that.
//
// maorcc/gimp-mcp is REFERENCE ONLY: its architecture (a plug-in running
// inside GIMP plus a separate Python MCP server speaking to it over a
// resident bridge) is explicitly NOT admitted here — no plug-in is
// installed, no bridge is opened, and no host GIMP is ever used. The only
// GIMP that runs is the pinned, binmgr-verified AppImage, and only as a
// bounded batch child.
//
// The batch dialect targets GIMP 3.x (pinned 3.2.6): python-fu-eval with
// the GObject API (Gimp.file_load / image.scale / Gimp.file_save /
// image.delete), which the reference ecosystem documents as the supported
// 3.x batch path now that the 2.x Scheme interpreter is deprecated.
// Program bodies that the first live run has not yet executed are marked
// as such in EVIDENCE.md; validation, argv construction, escaping,
// provisioning and the refusal surface are fully fixture-pinned here.
package gimp

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
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/binmgr"
)

const (
	// DefaultVersion is the pinned GIMP release (3.2 line; 3.2.6 is the
	// current stable per the project's 0.0_LATEST-IS-3.2.6 marker). There
	// is no "latest": a different version is an explicit, reviewed bump
	// here plus a matching committed sha256 in pkg/binmgr/pins.go.
	DefaultVersion = "3.2.6"

	// toolName is the binmgr cache key and the pin name
	// ("gimp@<version>/<goos>/<goarch>").
	toolName = "gimp"

	// releaseBase is the official GIMP release root. It answers plain
	// HTTPS GETs with a 302 into the mirror network (verified 2026-10-18:
	// linux/SHA256SUMS resolved to mirrors.ocf.berkeley.edu); binmgr
	// follows the redirect and the downloaded bytes are verified against
	// the COMMITTED pin, so the serving mirror is never the trust anchor.
	releaseBase = "https://download.gimp.org/gimp"

	// batchInterpreter is the only script engine admitted: the GIMP 3
	// Python evaluator. The 2.x Scheme interpreter is deprecated upstream
	// and is never selected.
	batchInterpreter = "python-fu-eval"

	// resultMarker prefixes the single JSON result line each fixed program
	// prints; everything else GIMP writes to stdout is noise.
	resultMarker = "yoke-gimp-v1 "
)

// ErrNoPin is returned (wrapped) when no committed sha256 exists for the
// requested version on the current platform. Nothing is downloaded.
var ErrNoPin = errors.New("gimp: no committed sha256 pin for this version/platform (pkg/binmgr/pins.go); refusing release-supplied checksum")

// ErrNoArtifact is returned (wrapped) for platforms where GIMP publishes no
// binmgr-consumable official binary: Windows ships only an installer
// executable and macOS only a disk image, neither of which binmgr unpacks
// or executes as a managed binary.
var ErrNoArtifact = errors.New("gimp: no binmgr-consumable official artifact for this platform (Windows ships an installer .exe, macOS a .dmg)")

var versionRe = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)$`)

// Artifact is the official release binary for one platform.
type Artifact struct {
	URL string
	// Raw is always true: the AppImage IS the executable (download →
	// verify → chmod +x → exec). There is no archive and no entrypoint.
	Raw bool
}

// ArtifactFor returns the official GIMP binary for version on goos/goarch.
// ok is false where no binmgr-consumable artifact exists: Windows (Inno
// Setup installer) and darwin (dmg), plus every arch GIMP does not publish.
func ArtifactFor(version, goos, goarch string) (Artifact, bool) {
	m := versionRe.FindStringSubmatch(version)
	if m == nil {
		return Artifact{}, false
	}
	dir := fmt.Sprintf("%s/v%s.%s/linux", releaseBase, m[1], m[2])
	switch goos + "/" + goarch {
	case "linux/amd64":
		return Artifact{URL: dir + "/GIMP-" + version + "-x86_64.AppImage", Raw: true}, true
	case "linux/arm64":
		return Artifact{URL: dir + "/GIMP-" + version + "-aarch64.AppImage", Raw: true}, true
	}
	return Artifact{}, false
}

// Spec is the binmgr tool declaration for version, carrying an asset ONLY
// for platforms that have both an official binary and a committed sha256
// pin. A platform without a pin is absent from Assets, so binmgr.Ensure
// cannot resolve (let alone download) it — fail closed by construction.
func Spec(version string) (binmgr.Tool, error) {
	if version == "" {
		version = DefaultVersion
	}
	if !versionRe.MatchString(version) {
		return binmgr.Tool{}, fmt.Errorf("gimp: invalid version %q (want MAJOR.MINOR.PATCH)", version)
	}
	assets := map[string]binmgr.Asset{}
	for _, plat := range []string{"linux/amd64", "linux/arm64"} {
		goos, goarch, _ := strings.Cut(plat, "/")
		art, ok := ArtifactFor(version, goos, goarch)
		if !ok || !art.Raw {
			continue
		}
		sha, ok := binmgr.PinnedSHA256(toolName, version, plat)
		if !ok {
			continue
		}
		assets[plat] = binmgr.Asset{URL: art.URL, SHA256: sha}
	}
	return binmgr.Tool{Name: toolName, Version: version, Assets: assets}, nil
}

// Path provisions (download → verify against the committed pin → cache) and
// returns the GIMP executable for version on the current platform. It
// fails closed, with no network, when the platform has no official
// consumable binary or no committed pin.
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
		return "", fmt.Errorf("gimp: cannot provision %s/%s from %s/%s", goos, goarch, runtime.GOOS, runtime.GOARCH)
	}
	return binmgr.Ensure(ctx, tool)
}

// Kind names one of the three typed operations.
type Kind string

const (
	// KindExport loads an explicit input file and writes it to an
	// explicit output path (format conversion; no pixel change).
	KindExport Kind = "export"
	// KindConvert converts the image mode (rgb|gray) and writes an
	// explicit output file.
	KindConvert Kind = "convert"
	// KindEdit applies bounded scripted edits (scale and/or quarter-turn
	// rotate) and writes an explicit output file.
	KindEdit Kind = "edit"
)

// Kinds is the complete operation set, in registration order.
var Kinds = []Kind{KindExport, KindConvert, KindEdit}

// Modes admitted by convert.
const (
	ModeRGB  = "rgb"
	ModeGray = "gray"
)

// Request is one typed operation. Only the fields the Kind uses are read.
type Request struct {
	Kind Kind
	// Input is an absolute image file to load (all operations).
	Input string
	// Output is the absolute flat-image file to write (all operations).
	Output string
	// Mode is the target image mode (convert; rgb|gray).
	Mode string
	// Scale is "WxH" (edit; each side 1..maxScaleDim).
	Scale string
	// Rotate is a quarter-turn in degrees (edit; 0 = none, else 90|180|270).
	Rotate int
}

const maxScaleDim = 16384

var (
	inputExt  = map[string]bool{".xcf": true, ".png": true, ".jpg": true, ".jpeg": true, ".tif": true, ".tiff": true, ".bmp": true, ".gif": true, ".webp": true}
	outputExt = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".tif": true, ".tiff": true, ".bmp": true, ".webp": true}
)

var scaleRe = regexp.MustCompile(`^([0-9]{1,5})x([0-9]{1,5})$`)

func extAllowed(path string, allowed map[string]bool) error {
	ext := strings.ToLower(filepath.Ext(path))
	if allowed[ext] {
		return nil
	}
	keys := make([]string, 0, len(allowed))
	for k := range allowed {
		keys = append(keys, k)
	}
	sortStrings(keys)
	return fmt.Errorf("extension %q not allowed (want one of %s)", ext, strings.Join(keys, ", "))
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// absPath checks the shape every path argument must have: non-empty,
// absolute, no NUL/newline, and not an option-looking token. Paths never
// become argv (they travel inside the single fixed --batch program), but
// the shape check still applies before anything is resolved or executed.
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

// parseScale validates "WxH" and returns the sides. Both sides must be in
// 1..maxScaleDim; the decimal shape is pinned so only digits reach %d.
func parseScale(s string) (w, h int, err error) {
	m := scaleRe.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, fmt.Errorf("--scale: %q is not WxH (decimal digits, e.g. 800x600)", s)
	}
	w, err = strconv.Atoi(m[1])
	if err != nil {
		return 0, 0, fmt.Errorf("--scale: %w", err)
	}
	h, err = strconv.Atoi(m[2])
	if err != nil {
		return 0, 0, fmt.Errorf("--scale: %w", err)
	}
	if w < 1 || w > maxScaleDim || h < 1 || h > maxScaleDim {
		return 0, 0, fmt.Errorf("--scale: %dx%d out of range 1..%d", w, h, maxScaleDim)
	}
	return w, h, nil
}

// Validate checks every argument BEFORE anything is resolved or executed.
func (r Request) Validate() error {
	if err := existingFile("--input", r.Input, inputExt); err != nil {
		return err
	}
	if err := writableTarget("--output", r.Output, outputExt); err != nil {
		return err
	}
	switch r.Kind {
	case KindExport:
		if r.Mode != "" || r.Scale != "" || r.Rotate != 0 {
			return errors.New("export: takes only --input and --output")
		}
		return nil
	case KindConvert:
		if r.Scale != "" || r.Rotate != 0 {
			return errors.New("convert: takes only --input, --output and --mode")
		}
		if r.Mode != ModeRGB && r.Mode != ModeGray {
			return fmt.Errorf("--mode: %q not allowed (want %q or %q)", r.Mode, ModeRGB, ModeGray)
		}
		return nil
	case KindEdit:
		if r.Mode != "" {
			return errors.New("edit: does not take --mode")
		}
		if r.Scale == "" && r.Rotate == 0 {
			return errors.New("edit: requires at least one of --scale or --rotate")
		}
		if r.Scale != "" {
			if _, _, err := parseScale(r.Scale); err != nil {
				return err
			}
		}
		switch r.Rotate {
		case 0, 90, 180, 270:
		default:
			return fmt.Errorf("--rotate: %d not allowed (want 90, 180 or 270)", r.Rotate)
		}
		return nil
	}
	return fmt.Errorf("gimp: unknown operation %q", string(r.Kind))
}

// pyString renders s as a Python single-quoted string literal. The caller
// must have validated s already (Validate rejects control characters, so
// no newline or NUL can reach this point); the escaper handles backslash
// and quote, and any other control character is refused rather than
// passed through. There is no other interpolation into the program.
func pyString(s string) (string, error) {
	var b strings.Builder
	b.WriteByte('\'')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString("\\\\")
		case '\'':
			b.WriteString("\\'")
		default:
			if r < 0x20 || r == 0x7f {
				return "", fmt.Errorf("value contains a control character")
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('\'')
	return b.String(), nil
}

// rotateFragment maps an admitted --rotate degree to the fixed
// GObject-enum call, spelled exactly as the GIMP 3.2 API reference
// declares GimpRotationType (GIMP_ROTATE_DEGREES90 and siblings; the
// GIMP_ prefix is stripped by introspection). The enum member names (not
// raw ints) travel, so no caller-controlled number ever reaches the
// program.
var rotateFragment = map[int]string{
	90:  "Gimp.RotationType.ROTATE_DEGREES90",
	180: "Gimp.RotationType.ROTATE_DEGREES180",
	270: "Gimp.RotationType.ROTATE_DEGREES270",
}

// modeFragment maps an admitted --mode to the fixed conversion call.
var modeFragment = map[string]string{
	ModeRGB:  "_image.convert_rgb()",
	ModeGray: "_image.convert_grayscale()",
}

// schemaVersions pins the result envelope each fixed program prints.
var schemaVersions = map[Kind]string{
	KindExport:  "yoke-gimp-export-v1",
	KindConvert: "yoke-gimp-convert-v1",
	KindEdit:    "yoke-gimp-edit-v1",
}

// programFor builds the single fixed Python-Fu program for the request.
// The template is constant; substitution is limited to escaped path
// literals, range-checked integers and closed-vocabulary call fragments,
// so a request can never smuggle an extra statement into the batch.
func programFor(r Request) (string, error) {
	if err := r.Validate(); err != nil {
		return "", err
	}
	in, err := pyString(filepath.Clean(r.Input))
	if err != nil {
		return "", fmt.Errorf("--input: %w", err)
	}
	out, err := pyString(filepath.Clean(r.Output))
	if err != nil {
		return "", fmt.Errorf("--output: %w", err)
	}
	var steps strings.Builder
	switch r.Kind {
	case KindExport:
	case KindConvert:
		frag, ok := modeFragment[r.Mode]
		if !ok {
			return "", fmt.Errorf("--mode: %q not allowed", r.Mode)
		}
		steps.WriteString("    " + frag + "\n")
	case KindEdit:
		if r.Scale != "" {
			w, h, err := parseScale(r.Scale)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(&steps, "    _image.scale(%d, %d)\n", w, h)
		}
		if r.Rotate != 0 {
			frag, ok := rotateFragment[r.Rotate]
			if !ok {
				return "", fmt.Errorf("--rotate: %d not allowed", r.Rotate)
			}
			steps.WriteString("    _image.rotate(" + frag + ")\n")
		}
	default:
		return "", fmt.Errorf("gimp: unknown operation %q", string(r.Kind))
	}
	payload, err := json.Marshal(map[string]string{
		"operation":      string(r.Kind),
		"schema_version": schemaVersions[r.Kind],
		"input":          filepath.Clean(r.Input),
		"output":         filepath.Clean(r.Output),
		"mode":           r.Mode,
		"scale":          r.Scale,
		"rotate":         strconv.Itoa(r.Rotate),
	})
	if err != nil {
		return "", err
	}
	marker, err := pyString(resultMarker)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("from gi.repository import Gimp, Gio\n")
	b.WriteString("_image = Gimp.file_load(Gimp.RunMode.NONINTERACTIVE, Gio.File.new_for_path(" + in + "))\n")
	b.WriteString("try:\n")
	b.WriteString(steps.String())
	b.WriteString("    Gimp.file_save(Gimp.RunMode.NONINTERACTIVE, _image, Gio.File.new_for_path(" + out + "), None)\n")
	// The payload is already a complete JSON object (Go-marshalled with
	// sorted keys); it is printed verbatim, not re-encoded — a dumps()
	// here would double-encode it into a quoted string the wrapper
	// could not parse as a result object.
	b.WriteString("    print(" + marker + " + " + string(pyLiteral(payload)) + ")\n")
	b.WriteString("finally:\n    _image.delete()\n")
	return b.String(), nil
}

// pyLiteral renders already-JSON bytes as a Python string literal the
// program prints verbatim after the marker. The bytes come from
// encoding/json over validated fields; the escaper keeps them a single
// literal.
func pyLiteral(p []byte) []byte {
	var b strings.Builder
	b.WriteByte('\'')
	for _, c := range p {
		switch c {
		case '\\':
			b.WriteString("\\\\")
		case '\'':
			b.WriteString("\\'")
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('\'')
	return []byte(b.String())
}

// fixedPrograms builds the programs for the representative request matrix
// over one fixture pair. Tests use it to pin that Argv only ever carries a
// program built from the fixed template.
func fixedPrograms(in, out string) map[string]string {
	fx := func(kind Kind, mode, scale string, rotate int) string {
		p, err := programFor(Request{Kind: kind, Input: in, Output: out, Mode: mode, Scale: scale, Rotate: rotate})
		if err != nil {
			panic(err)
		}
		return p
	}
	return map[string]string{
		"export":       fx(KindExport, "", "", 0),
		"convert-rgb":  fx(KindConvert, ModeRGB, "", 0),
		"convert-gray": fx(KindConvert, ModeGray, "", 0),
		"edit-scale":   fx(KindEdit, "", "800x600", 0),
		"edit-rotate":  fx(KindEdit, "", "", 90),
		"edit-both":    fx(KindEdit, "", "800x600", 270),
	}
}

// Argv returns the exact GIMP argument vector (after argv[0]) for the
// request: headless flags, exactly one --batch-interpreter, exactly one
// --batch carrying the fixed program, and --quit. No user value ever
// becomes an argument: paths travel inside the program literal only.
func (r Request) Argv() ([]string, error) {
	prog, err := programFor(r)
	if err != nil {
		return nil, err
	}
	return []string{
		"--no-interface", "--no-data", "--no-fonts", "--new-instance",
		"--batch-interpreter", batchInterpreter,
		"--batch", prog,
		"--quit",
	}, nil
}

// Executor runs one foreground child to completion under ctx. The default
// is runCommand (binmgr.Command + Run); tests inject a fake.
type Executor func(ctx context.Context, bin string, args []string, stdout, stderr io.Writer) error

// Resolver returns the GIMP executable for a version (default: Path).
type Resolver func(ctx context.Context, version string) (string, error)

// runCommand is the only place a GIMP process is created: one
// exec via binmgr.Command, waited on with Run. No Start, no detach, no
// process group of its own — ctx cancellation kills the child.
func runCommand(ctx context.Context, bin string, args []string, stdout, stderr io.Writer) error {
	cmd := binmgr.Command(ctx, bin, args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Stdin = nil
	err := cmd.Run()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

type runner struct {
	resolve Resolver
	execute Executor
}

var defaultRunner = &runner{resolve: Path, execute: runCommand}

// Run validates r, provisions GIMP, runs exactly one foreground batch
// child under ctx, verifies the child printed its result line AND left a
// non-empty output file, and writes the operation's JSON result (plus the
// observed output_bytes) to out.
func Run(ctx context.Context, version string, r Request, out io.Writer) error {
	return defaultRunner.run(ctx, version, r, out)
}

func (rn *runner) run(ctx context.Context, version string, r Request, out io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	argv, err := r.Argv()
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if version == "" {
		version = DefaultVersion
	}
	bin, err := rn.resolve(ctx, version)
	if err != nil {
		return err
	}
	var stdout, stderr bytes.Buffer
	if err := rn.execute(ctx, bin, argv, &stdout, &stderr); err != nil {
		return fmt.Errorf("gimp %s: %w%s", r.Kind, err, tail(stderr.String()))
	}
	result, ok := findResult(stdout.String())
	if !ok {
		return fmt.Errorf("gimp %s: child exited 0 but printed no result line%s", r.Kind, tail(stderr.String()))
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(result), &decoded); err != nil {
		return fmt.Errorf("gimp %s: result is not JSON: %v", r.Kind, err)
	}
	st, err := os.Stat(r.Output)
	if err != nil {
		return fmt.Errorf("gimp %s: output missing after success: %w", r.Kind, err)
	}
	if st.IsDir() || st.Size() == 0 {
		return fmt.Errorf("gimp %s: output %q is empty", r.Kind, r.Output)
	}
	decoded["output_bytes"] = st.Size()
	final, err := json.Marshal(decoded)
	if err != nil {
		return err
	}
	_, err = io.WriteString(out, string(final)+"\n")
	return err
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

func tail(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > 20 {
		lines = lines[len(lines)-20:]
	}
	return "\n" + strings.Join(lines, "\n")
}

// NewCmd is the `gimp` front door: a parent with exactly the three typed
// subcommands. No pass-through, no `run`, no `program`, no `serve`.
func NewCmd() *cobra.Command { return newCmd(defaultRunner) }

func newCmd(rn *runner) *cobra.Command {
	root := &cobra.Command{
		Use:   "gimp",
		Short: "Typed GIMP batch operations (export, convert, edit)",
		Long: `Run GIMP as a managed external (pkg/binmgr) in exactly one mode: one
foreground batch child per typed operation, under the caller's context.
GIMP is GPL-3.0-or-later and is downloaded and executed, never linked.
Provisioning fails closed without a committed sha256 pin.

Each operation runs a fixed Python-Fu program and prints one JSON result
line. There is no free-form program, no arbitrary GIMP arguments, no
plug-in installation, no resident plug-in host, no MCP server, no
listener, and no GUI.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		RunE:          func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	var version string
	var timeout time.Duration
	root.PersistentFlags().StringVar(&version, "gimp-version", DefaultVersion, "pinned GIMP release (must have a committed sha256)")
	root.PersistentFlags().DurationVar(&timeout, "timeout", 0, "optional deadline for the single GIMP child (0 = caller's context only)")

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

	var ex Request
	export := &cobra.Command{
		Use:   "export --input /abs/in --output /abs/out.png",
		Short: "Load an image file and write it to an explicit output path (format conversion)",
		Args:  cobra.NoArgs,
		RunE:  func(c *cobra.Command, _ []string) error { ex.Kind = KindExport; return exec(c, ex) },
	}
	export.Flags().StringVar(&ex.Input, "input", "", "absolute path to the image to load (required)")
	export.Flags().StringVar(&ex.Output, "output", "", "absolute path of the flat image to write (required)")
	_ = export.MarkFlagRequired("input")
	_ = export.MarkFlagRequired("output")

	var cv Request
	convert := &cobra.Command{
		Use:   "convert --input /abs/in --output /abs/out.png --mode rgb|gray",
		Short: "Convert the image mode and write an explicit output file",
		Args:  cobra.NoArgs,
		RunE:  func(c *cobra.Command, _ []string) error { cv.Kind = KindConvert; return exec(c, cv) },
	}
	convert.Flags().StringVar(&cv.Input, "input", "", "absolute path to the image to load (required)")
	convert.Flags().StringVar(&cv.Output, "output", "", "absolute path of the flat image to write (required)")
	convert.Flags().StringVar(&cv.Mode, "mode", "", "target image mode: rgb or gray (required)")
	_ = convert.MarkFlagRequired("input")
	_ = convert.MarkFlagRequired("output")
	_ = convert.MarkFlagRequired("mode")

	var ed Request
	edit := &cobra.Command{
		Use:   "edit --input /abs/in --output /abs/out.png [--scale WxH] [--rotate 90|180|270]",
		Short: "Apply bounded scripted edits (scale and/or quarter-turn rotate) and write an explicit output file",
		Args:  cobra.NoArgs,
		RunE:  func(c *cobra.Command, _ []string) error { ed.Kind = KindEdit; return exec(c, ed) },
	}
	edit.Flags().StringVar(&ed.Input, "input", "", "absolute path to the image to load (required)")
	edit.Flags().StringVar(&ed.Output, "output", "", "absolute path of the flat image to write (required)")
	edit.Flags().StringVar(&ed.Scale, "scale", "", "scale to WxH pixels, each side 1..16384 (at least one of --scale/--rotate)")
	edit.Flags().IntVar(&ed.Rotate, "rotate", 0, "quarter-turn in degrees: 90, 180 or 270 (at least one of --scale/--rotate)")
	_ = edit.MarkFlagRequired("input")
	_ = edit.MarkFlagRequired("output")

	root.AddCommand(export, convert, edit)
	return root
}

// Arg declares one operation input field for the registered-command
// contract: its CLI flag, value shape and bounds.
type Arg struct {
	Flag     string   // CLI flag name, without dashes
	Type     string   // "abspath(image-in)", "abspath(image-out)", "enum", "dimensions", "degrees"
	Required bool     // the operation refuses without it
	Choices  []string // set for Type enum
	Pattern  string   // set for Type dimensions
	Desc     string   // one-line bound description
}

// Op declares one admitted batch operation for the registered-command
// contract: its input schema, its result envelope, and its author-declared
// security effects (atlas vocabulary, curated — the mechanism adds nothing).
type Op struct {
	Name         string   // operation (subcommand) name
	Summary      string   // one line
	Args         []Arg    // input schema, in flag order
	Effects      []string // declared security effects (atlas atoms)
	ResultSchema string   // schema_version the result line carries
}

// Operations is the complete admitted set with per-operation input schemas
// and declared effects. Every operation reads explicit input files, writes
// one explicit output file, and spawns the GIMP batch child: read, write,
// exec — and nothing else (no net at operation time, no cred, priv,
// remote, persist, spend, destroy).
func Operations() []Op {
	rw := []string{"exec", "read", "write"}
	in := Arg{Flag: "input", Type: "abspath(image-in)", Required: true, Desc: "existing absolute image file (.xcf/.png/.jpg/.jpeg/.tif/.tiff/.bmp/.gif/.webp)"}
	out := Arg{Flag: "output", Type: "abspath(image-out)", Required: true, Desc: "absolute flat-image path to write (.png/.jpg/.jpeg/.tif/.tiff/.bmp/.webp), parent must exist"}
	return []Op{
		{
			Name:         string(KindExport),
			Summary:      "load an image file and write it to an explicit output path (format conversion, no pixel change)",
			Args:         []Arg{in, out},
			Effects:      rw,
			ResultSchema: schemaVersions[KindExport],
		},
		{
			Name:    string(KindConvert),
			Summary: "convert the image mode and write an explicit output file",
			Args: []Arg{in, out,
				{Flag: "mode", Type: "enum", Required: true, Choices: []string{ModeRGB, ModeGray}, Desc: "target image mode"},
			},
			Effects:      rw,
			ResultSchema: schemaVersions[KindConvert],
		},
		{
			Name:    string(KindEdit),
			Summary: "apply bounded scripted edits and write an explicit output file (at least one of scale/rotate)",
			Args: []Arg{in, out,
				{Flag: "scale", Type: "dimensions", Desc: "scale to WxH pixels, each side 1..16384, decimal WxH", Pattern: scaleRe.String()},
				{Flag: "rotate", Type: "degrees", Choices: []string{"90", "180", "270"}, Desc: "quarter-turn in degrees"},
			},
			Effects:      rw,
			ResultSchema: schemaVersions[KindEdit],
		},
	}
}

// Registration declares the `gimp` command for the registered-command
// contract (pkg/atlas RegisteredSpec shape, in plain strings so this
// package keeps its binmgr+cobra+stdlib-only dependency posture):
// a downloaded (binmgr-provisioned, pinned) userland fileutils command,
// cross-stage, JSON result envelopes, OS-scoped to the single provisionable
// platform. Reversibility and stability are the contract's fail-closed
// defaults (irreversible, experimental) — a registration never promises
// them. Mounting this declaration on the command/MCP surface is a
// separate, reviewed change; this story only declares it.
type Registration struct {
	Kind    string   // registered-command kind: download
	Group   string   // atlas group: fileutils
	Tier    string   // atlas tier: userland (a general image utility)
	Stage   string   // atlas stage: cross
	Shape   string   // output shape: json
	Caps    []string // mechanism-derived at mount time; declared empty here
	Effects []string // union over Operations: exec, read, write
	OS      []string // provisionable platforms only
}

// Registered returns the command-level declaration. Effects is exactly the
// union of the per-operation effects in Operations.
func Registered() Registration {
	seen := map[string]bool{}
	var effects []string
	for _, op := range Operations() {
		for _, e := range op.Effects {
			if !seen[e] {
				seen[e] = true
				effects = append(effects, e)
			}
		}
	}
	sortStrings(effects)
	return Registration{
		Kind:    "download",
		Group:   "fileutils",
		Tier:    "userland",
		Stage:   "cross",
		Shape:   "json",
		Effects: effects,
		OS:      []string{"linux/amd64"},
	}
}
