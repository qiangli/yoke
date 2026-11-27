package webconsole

import (
	"context"
	"encoding/json"
	"errors"
	core "github.com/qiangli/yoke/pkg/shape"
	"strings"
)

func (a *shapedApp) candidateService() core.Service {
	return core.Service{App: core.App{Name: a.name, SourceDir: a.repo, Shaping: a.shaping, Scopes: a.scopes, Owner: a.owner, ReleasePath: a.releasePath, ReleaseRevision: a.releaseRevision, Protected: a.protected, Seed: a.seed}, Options: core.Options{Home: a.opts.Home, WorkRoot: a.opts.WorkRoot, Delegate: a.opts.Delegate, Verifier: a.opts.Verifier, Activator: a.opts.Activator, Track: shapeTracking(a.opts), ProgramRunner: a.opts.ProgramRunner, ServiceRuntime: a.opts.ServiceRuntime}}
}
func shapeCallerContext(p string) core.Caller {
	return core.Caller{Principal: p, Actor: p, RepresentedUser: p}
}
func coreCandidate(c Candidate) core.Candidate {
	var v core.Candidate
	b, _ := json.Marshal(c)
	_ = json.Unmarshal(b, &v)
	return v
}
func webCandidate(c core.Candidate) Candidate {
	var v Candidate
	b, _ := json.Marshal(c)
	_ = json.Unmarshal(b, &v)
	return v
}
func (a *shapedApp) sharedVerify(ctx context.Context, c *Candidate, dir string) error {
	v := coreCandidate(*c)
	e := a.candidateService().VerifyRecord(ctx, a.caller(c.Principal), &v, dir)
	*c = webCandidate(v)
	if errors.Is(e, core.ErrVerification) {
		return nil
	}
	return e
}

func shapeTracking(o ShapeOptions) func(context.Context, *core.Candidate) error {
	if o.Track != nil {
		return o.Track
	}
	switch o.Verifier.(type) {
	case productionVerifier, *productionVerifier, core.ProductionVerifier, *core.ProductionVerifier:
		return core.NewTracking(o.Home)
	}
	return nil
}

// ConsoleShapeApp exposes the console's existing embedded-source seed and
// protected manifest to the local shaping adapter without starting HTTP.
func ConsoleShapeApp(home string) core.App {
	s := &server{opts: Options{Shape: ShapeOptions{Home: home}}}
	a := s.consoleShaped()
	return core.App{Name: a.name, SourceDir: a.repo, Protected: append([]string(nil), a.protected...), Seed: a.seed}
}

func (a *shapedApp) caller(p string) core.Caller {
	c := shapeCallerContext(p)
	if a.opts.Actor != "" {
		c.Actor = a.opts.Actor
	}
	if a.opts.RepresentedUser != "" {
		c.RepresentedUser = a.opts.RepresentedUser
	}
	c.Tier = a.opts.Tier
	c.Circle = a.opts.Circle
	if len(a.opts.Groups) > 0 {
		c.Groups = append([]string(nil), a.opts.Groups...)
	}
	c.Email = a.opts.Email
	return c
}

func (a *shapedApp) isOwner(principal string) bool {
	owner := strings.TrimSpace(a.owner)
	return owner != "" && strings.EqualFold(strings.TrimSpace(principal), owner)
}

func (a *shapedApp) writeWorkLedger(work, base, principal string) {
	if home, err := a.opts.home(); err == nil {
		_ = core.WriteWorkLedger(work, home, a.name, base, a.isOwner(principal), principal)
	}
}

func (a *shapedApp) workLedger(work, base, principal, excludeCand string) string {
	home, err := a.opts.home()
	if err != nil {
		return ""
	}
	return core.PrepareWorkLedger(work, home, a.name, base, a.isOwner(principal), principal, excludeCand)
}
