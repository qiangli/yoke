package lenscmd

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/ref"
)

// messageOperand follows `mb:<seq>` to the one locator its body names, using
// the ref bus.MessageLocators emits (a writer-host locator already carries its
// @host scope, so it reads through the peer transport, never this filesystem).
// The post is only read; zero or several followable locators are refused
// rather than guessed between.
func messageOperand(rc *tool.RunContext, op string) (ref.Locator, bool, int) {
	rest, ok := strings.CutPrefix(op, "mb:")
	if !ok {
		return ref.Locator{}, false, 0
	}
	seq, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || seq < 1 {
		return ref.Locator{}, false, 0
	}
	posts, err := bus.Posts()
	if err != nil {
		fmt.Fprintf(rc.Err, "lens: %s: %v\n", op, err)
		return ref.Locator{}, true, 1
	}
	for _, p := range posts {
		if p.Seq != seq {
			continue
		}
		var found []bus.MessageRef
		for _, r := range bus.MessageLocators(p.Body, p.From) {
			if _, ok := providers[r.Scheme]; ok {
				found = append(found, r)
			}
		}
		switch len(found) {
		case 0:
			fmt.Fprintf(rc.Err, "lens: %s: message names no followable locator\n", op)
			return ref.Locator{}, true, 1
		case 1:
			loc, err := ref.ParseLocator(found[0].Ref)
			if err != nil {
				fmt.Fprintf(rc.Err, "lens: %s: %v\n", op, err)
				return ref.Locator{}, true, 1
			}
			return loc, true, 0
		}
		refs := make([]string, len(found))
		for i, r := range found {
			refs[i] = r.Ref
		}
		fmt.Fprintf(rc.Err, "lens: %s: ambiguous, message names %d locators: %s\n", op, len(found), strings.Join(refs, ", "))
		return ref.Locator{}, true, 1
	}
	fmt.Fprintf(rc.Err, "lens: %s: no such message\n", op)
	return ref.Locator{}, true, 1
}
