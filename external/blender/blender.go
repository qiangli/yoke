// Package blender runs Blender as a managed external binary (pkg/binmgr) in
// exactly one mode: a short-lived, foreground `blender --background` child
// per typed operation. Blender is GPL-3.0-or-later; this package never
// links, vendors, or modifies it — it is download + sha256-verify + exec
// only, and the sha256 is a COMMITTED pin (pkg/binmgr/pins.go). Without a
// committed pin for the current platform, provisioning fails closed: the
// release-supplied checksum is never trusted (supply-chain Rule 2: no
// server, no resident mode, no trust-on-first-use).
//
// The surface is four typed operations — scene-inspect, asset-import,
// render, output-inspect — each over explicit ABSOLUTE paths. Every
// operation runs a FIXED bpy Python program (the constants below) that
// reads its paths from argv after "--". There is no free-form Python, no
// pass-through of arbitrary Blender arguments, no daemon, listener, MCP
// server, or GUI. Package-level tests pin all of that.
package blender

import (
	"bytes"
	"context"
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
	// DefaultVersion is the pinned Blender release (4.2 LTS line). There is
	// no "latest": a different version is an explicit, reviewed bump here
	// plus a matching committed sha256 in pkg/binmgr/pins.go.
	DefaultVersion = "4.2.3"

	// toolName is the binmgr cache key and the pin name
	// ("blender@<version>/<goos>/<goarch>").
	toolName = "blender"

	// releaseBase is the official Blender Foundation download root.
	releaseBase = "https://download.blender.org/release"

	// resultMarker prefixes the single JSON result line each fixed script
	// prints; everything else Blender writes to stdout is noise.
	resultMarker = "yoke-blender-v1 "

	// pythonExitCode is passed as --python-exit-code so an exception inside
	// the fixed script fails the child loudly instead of exiting 0.
	pythonExitCode = "3"
)

// ErrNoPin is returned (wrapped) when no committed sha256 exists for the
// requested version on the current platform. Nothing is downloaded.
var ErrNoPin = errors.New("blender: no committed sha256 pin for this version/platform (pkg/binmgr/pins.go); refusing release-supplied checksum")

// ErrNoArtifact is returned (wrapped) for platforms the Blender Foundation
// publishes no archive binmgr can unpack (macOS ships only .dmg).
var ErrNoArtifact = errors.New("blender: no extractable official artifact for this platform")

var versionRe = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)$`)

// Artifact is the official release archive for one platform.
type Artifact struct {
	URL        string
	Entrypoint string // slash-separated executable path inside the archive tree
}

// ArtifactFor returns the official Blender archive for version on goos/goarch
// and the executable's path inside it. ok is false where no binmgr-unpackable
// artifact exists: darwin (dmg only) and every arch Blender does not publish.
func ArtifactFor(version, goos, goarch string) (Artifact, bool) {
	m := versionRe.FindStringSubmatch(version)
	if m == nil {
		return Artifact{}, false
	}
	dir := fmt.Sprintf("%s/Blender%s.%s", releaseBase, m[1], m[2])
	switch goos + "/" + goarch {
	case "linux/amd64":
		stem := "blender-" + version + "-linux-x64"
		return Artifact{URL: dir + "/" + stem + ".tar.xz", Entrypoint: stem + "/blender"}, true
	case "windows/amd64":
		stem := "blender-" + version + "-windows-x64"
		return Artifact{URL: dir + "/" + stem + ".zip", Entrypoint: stem + "/blender.exe"}, true
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
		return binmgr.Tool{}, fmt.Errorf("blender: invalid version %q (want MAJOR.MINOR.PATCH)", version)
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
		assets[plat] = binmgr.Asset{URL: art.URL, SHA256: sha, Tree: true, Entrypoint: art.Entrypoint}
	}
	return binmgr.Tool{Name: toolName, Version: version, Assets: assets}, nil
}

// Path provisions (download → verify against the committed pin → cache) and
// returns the Blender executable for version on the current platform. It
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
		return "", fmt.Errorf("blender: cannot provision %s/%s from %s/%s", goos, goarch, runtime.GOOS, runtime.GOARCH)
	}
	return binmgr.Ensure(ctx, tool)
}

// Kind names one of the four typed operations.
type Kind string

const (
	KindSceneInspect  Kind = "scene-inspect"
	KindAssetImport   Kind = "asset-import"
	KindRender        Kind = "render"
	KindOutputInspect Kind = "output-inspect"
)

// Kinds is the complete operation set, in registration order.
var Kinds = []Kind{KindSceneInspect, KindAssetImport, KindRender, KindOutputInspect}

// Request is one typed operation. Only the fields the Kind uses are read.
type Request struct {
	Kind Kind
	// Input is an absolute .blend file (scene-inspect, render; optional for
	// asset-import) or an absolute rendered image (output-inspect).
	Input string
	// Asset is the absolute mesh/scene file to import (asset-import).
	Asset string
	// Output is the absolute .blend to save (asset-import) or the absolute
	// .png to write (render).
	Output string
	// Frame is the frame to render (render; >= 0).
	Frame int
}

var (
	blendExt  = map[string]bool{".blend": true}
	assetExt  = map[string]bool{".obj": true, ".fbx": true, ".gltf": true, ".glb": true, ".stl": true, ".ply": true, ".usd": true, ".usda": true, ".usdc": true, ".usdz": true}
	renderExt = map[string]bool{".png": true}
	imageExt  = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".exr": true, ".tif": true, ".tiff": true, ".bmp": true, ".hdr": true, ".tga": true}
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
	case KindSceneInspect:
		if r.Asset != "" || r.Output != "" {
			return errors.New("scene-inspect: takes only --input")
		}
		return existingFile("--input", r.Input, blendExt)
	case KindAssetImport:
		if r.Input != "" {
			if err := existingFile("--input", r.Input, blendExt); err != nil {
				return err
			}
		}
		if err := existingFile("--asset", r.Asset, assetExt); err != nil {
			return err
		}
		if err := writableTarget("--output", r.Output, blendExt); err != nil {
			return err
		}
		if r.Input != "" && filepath.Clean(r.Input) == filepath.Clean(r.Output) {
			return errors.New("asset-import: --output must differ from --input")
		}
		return nil
	case KindRender:
		if r.Asset != "" {
			return errors.New("render: does not take --asset")
		}
		if err := existingFile("--input", r.Input, blendExt); err != nil {
			return err
		}
		if err := writableTarget("--output", r.Output, renderExt); err != nil {
			return err
		}
		if r.Frame < 0 {
			return fmt.Errorf("--frame: %d is negative", r.Frame)
		}
		return nil
	case KindOutputInspect:
		if r.Asset != "" || r.Output != "" {
			return errors.New("output-inspect: takes only --input")
		}
		return existingFile("--input", r.Input, imageExt)
	}
	return fmt.Errorf("blender: unknown operation %q", string(r.Kind))
}

// Argv returns the exact Blender argument vector (after argv[0]) for the
// request. It always begins with --background and carries one of the fixed
// scripts via --python-expr; paths travel only after "--".
func (r Request) Argv() ([]string, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	argv := []string{"--background", "--factory-startup"}
	switch r.Kind {
	case KindSceneInspect:
		argv = append(argv, filepath.Clean(r.Input))
		argv = append(argv, "--python-exit-code", pythonExitCode, "--python-expr", sceneInspectScript, "--")
	case KindAssetImport:
		fresh := "1"
		if r.Input != "" {
			argv = append(argv, filepath.Clean(r.Input))
			fresh = "0"
		}
		argv = append(argv, "--python-exit-code", pythonExitCode, "--python-expr", assetImportScript, "--",
			filepath.Clean(r.Asset), filepath.Clean(r.Output), fresh)
	case KindRender:
		argv = append(argv, filepath.Clean(r.Input))
		argv = append(argv, "--python-exit-code", pythonExitCode, "--python-expr", renderScript, "--",
			filepath.Clean(r.Output), strconv.Itoa(r.Frame))
	case KindOutputInspect:
		argv = append(argv, "--python-exit-code", pythonExitCode, "--python-expr", outputInspectScript, "--",
			filepath.Clean(r.Input))
	}
	return argv, nil
}

// Scripts returns the fixed bpy programs keyed by operation. Exported so
// tests (and reviewers) can pin that Argv only ever carries one of these.
func Scripts() map[Kind]string {
	return map[Kind]string{
		KindSceneInspect:  sceneInspectScript,
		KindAssetImport:   assetImportScript,
		KindRender:        renderScript,
		KindOutputInspect: outputInspectScript,
	}
}

// The fixed bpy programs. Each reads its arguments from sys.argv after "--",
// does one thing, and prints exactly one resultMarker-prefixed JSON line.
const (
	sceneInspectScript = `import bpy, json
s = bpy.context.scene
objs = sorted(bpy.data.objects, key=lambda o: o.name)
out = {
  "schema_version": "yoke-blender-scene-v1",
  "blender": bpy.app.version_string,
  "file": bpy.data.filepath,
  "scene": s.name,
  "frame_start": s.frame_start, "frame_end": s.frame_end, "frame_current": s.frame_current,
  "render": {"engine": s.render.engine, "resolution_x": s.render.resolution_x, "resolution_y": s.render.resolution_y,
             "resolution_percentage": s.render.resolution_percentage, "fps": s.render.fps},
  "camera": s.camera.name if s.camera else None,
  "objects": [{"name": o.name, "type": o.type, "data": o.data.name if o.data else None,
               "location": [round(float(v), 6) for v in o.location]} for o in objs],
  "collections": sorted(c.name for c in bpy.data.collections),
  "materials": sorted(m.name for m in bpy.data.materials),
}
print("` + resultMarker + `" + json.dumps(out, sort_keys=True))
`

	assetImportScript = `import bpy, json, os, sys
argv = sys.argv[sys.argv.index("--") + 1:]
asset, output, fresh = argv[0], argv[1], argv[2]
if fresh == "1":
    bpy.ops.wm.read_homefile(use_empty=True)
ext = os.path.splitext(asset)[1].lower()
before = set(o.name for o in bpy.data.objects)
if ext == ".obj":
    bpy.ops.wm.obj_import(filepath=asset)
elif ext == ".fbx":
    bpy.ops.import_scene.fbx(filepath=asset)
elif ext in (".gltf", ".glb"):
    bpy.ops.import_scene.gltf(filepath=asset)
elif ext == ".stl":
    bpy.ops.wm.stl_import(filepath=asset)
elif ext == ".ply":
    bpy.ops.wm.ply_import(filepath=asset)
elif ext in (".usd", ".usda", ".usdc", ".usdz"):
    bpy.ops.wm.usd_import(filepath=asset)
else:
    raise SystemExit("yoke-blender: unsupported asset " + ext)
added = sorted(set(o.name for o in bpy.data.objects) - before)
bpy.ops.wm.save_as_mainfile(filepath=output)
print("` + resultMarker + `" + json.dumps({"schema_version": "yoke-blender-import-v1", "asset": asset, "output": output,
      "fresh_scene": fresh == "1", "imported": added, "object_count": len(bpy.data.objects)}, sort_keys=True))
`

	renderScript = `import bpy, json, sys
argv = sys.argv[sys.argv.index("--") + 1:]
output, frame = argv[0], int(argv[1])
s = bpy.context.scene
if s.camera is None:
    raise SystemExit("yoke-blender: scene has no active camera")
s.frame_set(frame)
s.render.image_settings.file_format = "PNG"
s.render.filepath = output
s.render.use_file_extension = False
bpy.ops.render.render(write_still=True)
print("` + resultMarker + `" + json.dumps({"schema_version": "yoke-blender-render-v1", "output": output, "frame": frame,
      "engine": s.render.engine, "resolution_x": s.render.resolution_x, "resolution_y": s.render.resolution_y,
      "resolution_percentage": s.render.resolution_percentage}, sort_keys=True))
`

	outputInspectScript = `import bpy, json, sys
argv = sys.argv[sys.argv.index("--") + 1:]
path = argv[0]
img = bpy.data.images.load(path, check_existing=False)
print("` + resultMarker + `" + json.dumps({"schema_version": "yoke-blender-output-v1", "file": path,
      "width": img.size[0], "height": img.size[1], "channels": img.channels, "depth": img.depth,
      "file_format": img.file_format, "colorspace": img.colorspace_settings.name, "is_float": img.is_float,
      "alpha_mode": img.alpha_mode, "has_data": img.has_data}, sort_keys=True))
`
)

// Executor runs one foreground child to completion under ctx. The default
// is runCommand (binmgr.Command + Run); tests inject a fake.
type Executor func(ctx context.Context, bin string, args []string, stdout, stderr io.Writer) error

// Resolver returns the Blender executable for a version (default: Path).
type Resolver func(ctx context.Context, version string) (string, error)

// runCommand is the only place a Blender process is created: one
// exec.CommandContext via binmgr.Command, waited on with Run. No Start, no
// detach, no process group of its own — ctx cancellation kills the child.
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

// Run validates r, provisions Blender, runs exactly one foreground
// `blender --background` child under ctx, and writes the operation's JSON
// result line to out.
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
		return fmt.Errorf("blender %s: %w%s", r.Kind, err, tail(stderr.String()))
	}
	result, ok := findResult(stdout.String())
	if !ok {
		return fmt.Errorf("blender %s: child exited 0 but printed no result line%s", r.Kind, tail(stderr.String()))
	}
	_, err = io.WriteString(out, result+"\n")
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

// NewCmd is the `blender` front door: a parent with exactly the four typed
// subcommands. No pass-through, no `run`, no `python`, no `serve`.
func NewCmd() *cobra.Command { return newCmd(defaultRunner) }

func newCmd(rn *runner) *cobra.Command {
	root := &cobra.Command{
		Use:   "blender",
		Short: "Typed Blender operations (scene-inspect, asset-import, render, output-inspect)",
		Long: `Run Blender as a managed external (pkg/binmgr) in exactly one mode: one
foreground 'blender --background' child per typed operation, under the
caller's context. Blender is GPL-3.0-or-later and is downloaded and executed,
never linked. Provisioning fails closed without a committed sha256 pin.

Each operation runs a fixed bpy program and prints one JSON result line.
There is no free-form Python, no arbitrary Blender arguments, no daemon,
listener, MCP server, or GUI.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		RunE:          func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	var version string
	var timeout time.Duration
	root.PersistentFlags().StringVar(&version, "blender-version", DefaultVersion, "pinned Blender release (must have a committed sha256)")
	root.PersistentFlags().DurationVar(&timeout, "timeout", 0, "optional deadline for the single Blender child (0 = caller's context only)")

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

	var si Request
	sceneInspect := &cobra.Command{
		Use:   "scene-inspect --input /abs/scene.blend",
		Short: "Print scene, render settings, objects, collections and materials of a .blend as JSON",
		Args:  cobra.NoArgs,
		RunE:  func(c *cobra.Command, _ []string) error { si.Kind = KindSceneInspect; return exec(c, si) },
	}
	sceneInspect.Flags().StringVar(&si.Input, "input", "", "absolute path to the .blend to inspect (required)")
	_ = sceneInspect.MarkFlagRequired("input")

	var ai Request
	assetImport := &cobra.Command{
		Use:   "asset-import --asset /abs/model.obj --output /abs/out.blend [--input /abs/base.blend]",
		Short: "Import an OBJ/FBX/glTF/STL/PLY/USD asset into a fresh or given .blend and save it",
		Args:  cobra.NoArgs,
		RunE:  func(c *cobra.Command, _ []string) error { ai.Kind = KindAssetImport; return exec(c, ai) },
	}
	assetImport.Flags().StringVar(&ai.Asset, "asset", "", "absolute path to the asset file to import (required)")
	assetImport.Flags().StringVar(&ai.Output, "output", "", "absolute path of the .blend to write (required)")
	assetImport.Flags().StringVar(&ai.Input, "input", "", "absolute path of a .blend to import into (default: empty scene)")
	_ = assetImport.MarkFlagRequired("asset")
	_ = assetImport.MarkFlagRequired("output")

	var rd Request
	render := &cobra.Command{
		Use:   "render --input /abs/scene.blend --output /abs/frame.png [--frame N]",
		Short: "Render one frame of a .blend to a PNG",
		Args:  cobra.NoArgs,
		RunE:  func(c *cobra.Command, _ []string) error { rd.Kind = KindRender; return exec(c, rd) },
	}
	render.Flags().StringVar(&rd.Input, "input", "", "absolute path to the .blend to render (required)")
	render.Flags().StringVar(&rd.Output, "output", "", "absolute path of the .png to write (required)")
	render.Flags().IntVar(&rd.Frame, "frame", 1, "frame number to render")
	_ = render.MarkFlagRequired("input")
	_ = render.MarkFlagRequired("output")

	var oi Request
	outputInspect := &cobra.Command{
		Use:   "output-inspect --input /abs/frame.png",
		Short: "Print size, channels, depth, format and colorspace of a rendered image as JSON",
		Args:  cobra.NoArgs,
		RunE:  func(c *cobra.Command, _ []string) error { oi.Kind = KindOutputInspect; return exec(c, oi) },
	}
	outputInspect.Flags().StringVar(&oi.Input, "input", "", "absolute path to the image to inspect (required)")
	_ = outputInspect.MarkFlagRequired("input")

	root.AddCommand(sceneInspect, assetImport, render, outputInspect)
	return root
}
