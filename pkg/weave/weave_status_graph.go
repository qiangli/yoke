package weave

import (
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/qiangli/coreutils/pkg/weavecli"
	"github.com/qiangli/yoke/pkg/lens/provider"
	"github.com/qiangli/yoke/pkg/lens/stream"
	"github.com/qiangli/yoke/pkg/ref"
	"github.com/spf13/cobra"
)

// runWeaveStatusGraph is `weave status <issue> --graph`: the run's relations
// as a bashy-graph-v1 stream (Sprint 446 S19) — the runs it depends on, its
// parent container and children, and the sprint stories it implements. The
// record is the reconciled one status prints (a submitted run already in
// base reads as done), minus everything that varies with the clock or the
// machine: health, liveness, pids and the absolute workspace path. The
// status envelope (--json) is unchanged.
func runWeaveStatusGraph(cmd *cobra.Command, id int64, flags *weaveOutputFlags) error {
	mode := flags.mode()
	cwd, _ := os.Getwd()
	root, err := weaveRepoRoot(cwd)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave status",
			weavecli.ExitPrecondFail, err))
	}
	dir, _ := weaveQueueDir(root)
	q, err := readWeaveQueue(dir)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave status",
			weavecli.ExitGenericFail, err))
	}
	it := findWeaveItem(q, id)
	if it == nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave status",
			weavecli.ExitInvalidArg, fmt.Errorf("run #%d not found%s", id, weaveOtherActiveQueuesHintSuffix(dir))))
	}
	base := weaveBaseBranch(root)
	merged := weaveItemMerged(root, base, it)
	ahead, head := weaveLiveAhead(base, it)
	return writeWeaveStatusStream(cmd.OutOrStdout(), q, it, base, merged, ahead, head)
}

func weaveRunRef(id int64) string {
	return ref.Locator{Scheme: string(ref.Run), Target: strconv.FormatInt(id, 10)}.String()
}

// weaveRunNode is a related run as the queue records it; the subject run
// carries the reconciled attrs instead.
func weaveRunNode(it *weaveItem, id int64) provider.Node {
	n := provider.Node{Ref: weaveRunRef(id), Path: strconv.FormatInt(id, 10), Name: "#" + strconv.FormatInt(id, 10), Kind: "run", Type: "weave/run", Attrs: map[string]any{}}
	if it != nil {
		n.Attrs["title"] = it.Title
		n.Attrs["state"] = it.State
	}
	return n
}

func writeWeaveStatusStream(w io.Writer, q *weaveQueue, it *weaveItem, base string, merged bool, ahead int, head string) error {
	self := weaveRunRef(it.ID)
	b := stream.NewBuilder(self)
	state := it.State
	attrs := map[string]any{"title": it.Title, "recorded_state": it.State, "merged": merged, "base": base, "commits_ahead": ahead}
	if it.State == "submitted" && merged {
		state = "done"
	}
	attrs["state"] = state
	for k, v := range map[string]string{"branch": it.Branch, "base_sha": it.BaseSHA, "head": head, "tool": it.Tool,
		"register": it.Register, "stage": it.Stage, "priority": it.Priority} {
		if v != "" {
			attrs[k] = v
		}
	}
	if it.Points > 0 {
		attrs["points"] = it.Points
	}
	node := weaveRunNode(it, it.ID)
	node.Attrs = attrs
	b.Node(node)

	related := func(id int64) string {
		b.Node(weaveRunNode(findWeaveItem(q, id), id))
		return weaveRunRef(id)
	}
	for _, d := range it.DependsOn {
		b.Edge(self, "depends-on", related(d), nil)
	}
	if it.Parent > 0 {
		b.Edge(related(it.Parent), "contains", self, nil)
	}
	for _, c := range q.Items {
		if c.Parent == it.ID {
			b.Edge(self, "contains", related(c.ID), nil)
		}
	}
	for _, s := range it.WorkerStories {
		if s.ID == "" {
			continue
		}
		r := ref.Locator{Scheme: "story", Target: s.ID}.String()
		sa := map[string]any{"sprint": s.Sprint}
		if s.Seq > 0 {
			sa["seq"] = s.Seq
		}
		b.Node(provider.Node{Ref: r, Path: s.ID, Name: s.ID, Kind: "story", Type: "sprint/story", Attrs: sa})
		b.Edge(self, "implements", r, nil)
	}
	return b.Encode(w, "weave", "op=status")
}
