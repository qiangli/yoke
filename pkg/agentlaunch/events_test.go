package agentlaunch

import (
	"reflect"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
)

func TestEventArgsComeFromFleetDeclaration(t *testing.T) {
	root := t.TempDir()
	cat := fleet.New(fleet.WithRoot(root))
	if err := cat.SaveTool(fleet.Tool{
		Name: "probe", Kind: "cli",
		CLI: fleet.ToolCLI{Launch: fleet.ToolLaunch{
			Exec:         "probe -p {prompt}",
			EventsStdout: "--format stream-json",
			EventsArg:    "--events {path}",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	newCat := func() *fleet.Catalog { return fleet.New(fleet.WithRoot(root)) }
	l := Launch{ToolName: "probe"}
	if got, want := EventStdoutArgsWithCatalog(l, newCat), []string{"--format", "stream-json"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("stdout args = %q, want %q", got, want)
	}
	if got, want := EventFileArgsWithCatalog(l, "/tmp/e.jsonl", newCat), []string{"--events", "/tmp/e.jsonl"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("file args = %q, want %q", got, want)
	}
}

func TestInsertBeforePromptKeepsConsumingFlagAdjacent(t *testing.T) {
	got := InsertBeforePrompt(
		[]string{"agy", "--model", "m", "-p", "THE TASK"},
		[]string{"--output-format", "stream-json"},
	)
	want := []string{"agy", "--model", "m", "--output-format", "stream-json", "-p", "THE TASK"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %q, want %q", got, want)
	}
}
