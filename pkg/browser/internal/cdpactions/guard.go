package cdpactions

// probe/solo's wiring into the sensitive-input guard (pkg/browser/guard). The
// field check is the SAME guard.js the live extension injects, evaluated in a
// CDP isolated world so page scripts cannot redefine what it reads. This file
// is a PROTECTED path (pkg/src manifest).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"github.com/qiangli/yoke/pkg/browser/guard"
	"github.com/qiangli/yoke/pkg/browser/live"
	"github.com/qiangli/yoke/pkg/browser/wire"
)

// GuardBackend adapts a chromedp target to guard.Backend. Ready returns the
// attached context (launching/attaching on first use); Current returns it
// only if already attached. Human reports whether an operator can act in
// this browser (a headless solo Chrome has nobody).
type GuardBackend struct {
	Ready   func(ctx context.Context) (context.Context, error)
	Current func() context.Context
	Human   bool
}

var _ guard.Backend = GuardBackend{}

// evalGuard loads guard.js into a fresh isolated world of the top frame and
// evaluates expr there, decoding the by-value result into out.
func evalGuard(cdpCtx context.Context, expr string, out any) error {
	return chromedp.Run(cdpCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		tree, err := page.GetFrameTree().Do(ctx)
		if err != nil {
			return err
		}
		world, err := page.CreateIsolatedWorld(tree.Frame.ID).WithWorldName("bashy-guard").Do(ctx)
		if err != nil {
			return err
		}
		if _, exc, err := runtime.Evaluate(live.GuardScript()).WithContextID(world).Do(ctx); err != nil || exc != nil {
			if err == nil {
				err = errors.New(exc.Text)
			}
			return fmt.Errorf("guard: load guard.js: %w", err)
		}
		res, exc, err := runtime.Evaluate(expr).WithContextID(world).WithReturnByValue(true).Do(ctx)
		if err != nil {
			return err
		}
		if exc != nil {
			return errors.New("guard: " + exc.Text)
		}
		return json.Unmarshal(res.Value, out)
	}))
}

func (g GuardBackend) InspectField(ctx context.Context, a wire.Action) (guard.Field, error) {
	cdpCtx, err := g.Ready(ctx)
	if err != nil {
		return guard.Field{}, err
	}
	var out struct {
		guard.Field
		Error string `json:"error"`
	}
	if err := evalGuard(cdpCtx, fmt.Sprintf("globalThis.__bashyGuard.inspectSelector(%s)", JSString(a.Selector)), &out); err != nil {
		return guard.Field{}, err
	}
	if out.Error != "" {
		return guard.Field{}, errors.New(out.Error)
	}
	return out.Field, nil
}

func (g GuardBackend) FocusField(ctx context.Context, a wire.Action) error {
	cdpCtx, err := g.Ready(ctx)
	if err != nil {
		return err
	}
	expr := fmt.Sprintf(`(() => {
  const sel = %s;
  const el = sel ? document.querySelector(sel) : document.activeElement;
  if (!el) return { error: "guard: no element matches " + JSON.stringify(sel) };
  return globalThis.__bashyGuard.focusForHuman(el);
})()`, JSString(a.Selector))
	var out struct {
		Error string `json:"error"`
	}
	if err := evalGuard(cdpCtx, expr, &out); err != nil {
		return err
	}
	if out.Error != "" {
		return errors.New(out.Error)
	}
	return nil
}

// FillAdmitted types an operator's admitted answer. probe/solo have no
// page-side refusal in their type path, so this is the ordinary Type; its
// result (which reads the value back) is discarded.
func (g GuardBackend) FillAdmitted(ctx context.Context, a wire.Action, _ string, value []byte) error {
	cdpCtx, err := g.Ready(ctx)
	if err != nil {
		return err
	}
	callCtx, cancel := context.WithTimeout(cdpCtx, 30*time.Second)
	defer cancel()
	a.Text, a.FromAsk = string(value), ""
	res, err := Type(callCtx, a)
	if err != nil || res == nil || !res.Success {
		return errors.New("guard: fill failed")
	}
	return nil
}

func (g GuardBackend) Site(ctx context.Context) string {
	cdpCtx := g.Current()
	if cdpCtx == nil {
		return ""
	}
	var loc string
	if err := chromedp.Run(cdpCtx, chromedp.Location(&loc)); err != nil {
		return ""
	}
	u, err := url.Parse(loc)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func (g GuardBackend) HumanChannel() bool { return g.Human }
