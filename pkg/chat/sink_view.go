package chat

import (
	"context"
	"fmt"
	"strings"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/lens"
	"github.com/qiangli/yoke/pkg/reduce"
)

// applySinkView renders the reader's declared non-canonical shape
// (json/yaml/md) through the existing lens conversion. The producer's bytes are
// already redacted and home-canonicalized by the caller; the canonical form is
// spilled to the output store first so the note can point at it. Any case lens
// cannot convert (undetectable input, no route, invalid input) returns the
// bytes unchanged: a view is a courtesy, never a reason to fail or drop output.
func applySinkView(store *reduce.Store, canonical []byte, plan fleet.SinkPlan) ([]byte, string) {
	switch plan.View {
	case fleet.ViewJSON, fleet.ViewYAML, fleet.ViewMD:
	default:
		return canonical, ""
	}
	from := lens.Detect("", canonical)
	if from == "" || from == plan.View {
		return canonical, ""
	}
	res, err := lens.Convert(context.Background(), lens.Request{From: from, To: plan.View, Data: canonical})
	if err != nil {
		return canonical, ""
	}
	digest, err := store.Put(canonical)
	if err != nil {
		return canonical, ""
	}
	fidelity := "lossless"
	if res.Lossy() {
		fidelity = "lossy"
	}
	note := fmt.Sprintf("[bashy: view=%s(%s) from %s · %s · canonical: %s %s]", plan.View, plan.ViewSource, from, fidelity, reduce.DefaultRecoverVerb, digest)
	out := res.Data
	if !strings.HasSuffix(string(out), "\n") {
		out = append(out, '\n')
	}
	return out, note
}
