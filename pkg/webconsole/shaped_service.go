package webconsole

import (
	"context"
	"encoding/json"
	"errors"
	core "github.com/qiangli/yoke/pkg/shape"
)

func (a *shapedApp) candidateService() core.Service {
	return core.Service{App: core.App{Name: a.name, SourceDir: a.repo, Protected: a.protected, Seed: a.seed}, Options: core.Options{Home: a.opts.Home, WorkRoot: a.opts.WorkRoot, Delegate: a.opts.Delegate, Verifier: a.opts.Verifier, Activator: a.opts.Activator, Track: shapeTracking(a.opts)}}
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
	return c
}
