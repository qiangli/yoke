package cligw

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/agentlaunch"
	"github.com/qiangli/yoke/pkg/fleet"
)

// Sprint 379 story 847a4d6d: the muse seat behind cligw must launch a pure
// completion like claude's — no agent persona, no local tools — so a genie
// tool-call turn comes back as a tool call/text completion, not a Muse agent
// report about its empty worker dir. The baseline muse.yaml exec template
// carries --yolo (headless approval); cligw's read-only resolve must strip it
// and the worker must add only the tools-off isolation flags. The fake-catalog
// tests cannot see this: their exec template never contained --yolo. This test
// resolves the real embedded baseline through the exact worker path.
func TestMuseRealBindingLaunchesPureCompletion(t *testing.T) {
	cat := fleet.New(fleet.WithoutLocalStore(), fleet.WithoutCloudOverlay())
	old := agentlaunch.NewCatalog
	agentlaunch.NewCatalog = func() *fleet.Catalog { return cat }
	t.Cleanup(func() { agentlaunch.NewCatalog = old })

	cwd, launch, tool, mode, err := resolveWorkerSeat(context.Background(), "muse-spark1.3")
	if err != nil {
		t.Fatalf("resolveWorkerSeat(muse-spark1.3): %v", err)
	}
	defer func() { _ = os.RemoveAll(cwd) }()
	if mode != WarmCold {
		t.Fatalf("muse warm mode = %q, want cold (one-shot completion)", mode)
	}

	w := &Worker{launch: launch, tool: tool, mode: mode, cwd: cwd}
	argv := w.argv("PROMPT", "")
	argv[0] = strings.TrimSuffix(filepath.Base(argv[0]), ".exe") // the pinned cache path, by name
	want := []string{"muse", "exec", "--model", "muse-spark-1.3", "--json",
		"--no-session-log", "--no-foreign-personal-context", "--disable-web-tools",
		"--disable-shell", "--disable-write", "PROMPT"}
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("muse worker argv = %q, want %q", argv, want)
	}
	// muse.yaml's --yolo must not survive the read-only resolve, and no
	// workspace-trusting or sandbox-disabling spelling may sneak in: either
	// would hand the seat's agent persona back its local tools.
	for _, unsafe := range []string{"--yolo", "--disable-sandbox", "--enable-shell-tool", "--trust-workspace"} {
		if containsSequence(argv, []string{unsafe}) {
			t.Errorf("muse worker argv enables local agency with %s: %q", unsafe, argv)
		}
	}
}
