package shape

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/shape/slot"
	shapeverify "github.com/qiangli/yoke/pkg/shape/verify"
)

func TestShapePreviewCaptureLimit(t *testing.T) {
	var out shapePreviewBuffer
	// Hide WriterTo to exercise the generic streaming path; the buffer must
	// not expose an embedded ReadFrom that bypasses its bounded Write method.
	stream := struct{ io.Reader }{strings.NewReader(strings.Repeat("x", shapePreviewOutputLimit+4096))}
	n, err := io.Copy(&out, stream)
	if err != nil || n != shapePreviewOutputLimit+4096 || len(out.String()) != shapePreviewOutputLimit || !out.truncated {
		t.Fatalf("n=%d captured=%d truncated=%v err=%v", n, len(out.String()), out.truncated, err)
	}
}
func TestShapePreviewExecutesExactArtifactInPrivateContext(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	digest, err := shapeverify.ProgramArtifactDigest("cli", exe)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASHY_SHAPE_TEST_SECRET", "must-not-inherit")
	result, err := runShapeCLIPreview(context.Background(), ProgramPreview{Kind: "cli", Path: exe, Artifact: digest}, []string{"-test.run=^TestShapePreviewChild$", "--", "shape-preview-child"})
	if err != nil {
		t.Fatalf("preview err=%v stderr=%s stdout=%s", err, result.Stderr, result.Stdout)
	}
	if result.ExitCode != 0 || !strings.Contains(result.Stdout, "preview-child-ok") {
		t.Fatalf("preview result=%+v", result)
	}
	_, err = runShapeCLIPreview(context.Background(), ProgramPreview{Kind: "cli", Path: exe, Artifact: "sha256:wrong"}, nil)
	if err == nil {
		t.Fatal("mismatched artifact executed")
	}
}
func TestShapePreviewChild(t *testing.T) {
	if len(flag.Args()) != 1 || flag.Args()[0] != "shape-preview-child" {
		return
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("BASHY_SHAPE_TEST_SECRET") != "" {
		t.Fatal("parent secret inherited")
	}
	if os.Getenv("BASHY_HOME") != filepath.Join(cwd, ".bashy") {
		t.Fatal("shared app home exposed")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if !slot.PinnedEntry(exe) {
		t.Fatal("preview image is not pinned")
	}
	fmt.Println("preview-child-ok")
}
