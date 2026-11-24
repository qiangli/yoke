package webconsole

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	core "github.com/qiangli/yoke/pkg/shape"
	"github.com/qiangli/yoke/pkg/shape/appserver"
)

// LocalIncidentReport is the same bounded report accepted by the console.
type LocalIncidentReport = incidentReport

func localShaped(app core.App, opts core.Options) *shapedApp {
	return &shapedApp{name: app.Name, repo: app.SourceDir, shaping: app.Shaping, scopes: app.Scopes, owner: app.Owner, releasePath: app.ReleasePath, releaseRevision: app.ReleaseRevision, protected: app.Protected, seed: app.Seed, opts: ShapeOptions{Home: opts.Home, WorkRoot: opts.WorkRoot, Owner: app.Owner, Scopes: app.Scopes, ProgramRunner: opts.ProgramRunner, ServiceRuntime: opts.ServiceRuntime, Track: opts.Track, Delegate: opts.Delegate, Verifier: opts.Verifier, Activator: opts.Activator}}
}

// LocalShapeSuggest uses the console's version/config suggestion resolver directly.
func LocalShapeSuggest(ctx context.Context, app core.App, caller core.Caller, opts core.Options, page string) ([]string, error) {
	if strings.TrimSpace(caller.Principal) == "" {
		return nil, errors.New("shape: a principal is required")
	}
	a := localShaped(app, opts)
	repo, e := a.open()
	if e != nil {
		return nil, e
	}
	c, e := a.active(repo, caller.Principal)
	if e != nil {
		return nil, e
	}
	tree, e := c.Tree()
	if e != nil {
		return nil, e
	}
	if cfg, _ := genieConfigOf(tree); len(cfg.Examples) > 0 {
		return cfg.Examples, nil
	}
	out := versionSuggestions(c)
	if out == nil {
		out = []string{}
	}
	return out, nil
}

// LocalShapeIncidents reads the same principal-private incident records as the UI.
func LocalShapeIncidents(ctx context.Context, app core.App, caller core.Caller, opts core.Options, id string) (any, error) {
	if (core.Service{App: app}).Kind() != "web" {
		return nil, errors.New("shape: native incident repair is outside the MVP")
	}
	if strings.TrimSpace(caller.Principal) == "" {
		return nil, errors.New("shape: a principal is required")
	}
	a := localShaped(app, opts)
	st, e := a.store(caller.Principal)
	if e != nil {
		return nil, e
	}
	store, e := openIncidents(st)
	if e != nil {
		return nil, e
	}
	if id != "" {
		return store.Get(id)
	}
	return store.List()
}

// LocalShapeReportIncident records through the same store and runs a bounded
// repair synchronously: a one-shot CLI must not abandon an in-flight repair.
func LocalShapeReportIncident(ctx context.Context, app core.App, caller core.Caller, opts core.Options, rep LocalIncidentReport) (Incident, error) {
	if (core.Service{App: app}).Kind() != "web" {
		return Incident{}, errors.New("shape: native incident repair is outside the MVP; use the app's service diagnostics and ordinary shaping workflow")
	}
	if strings.TrimSpace(caller.Principal) == "" {
		return Incident{}, errors.New("shape: a principal is required")
	}
	a := localShaped(app, opts)
	a.opts.Actor = caller.Actor
	a.opts.RepresentedUser = caller.RepresentedUser
	external := rep.Offline
	if u, e := url.Parse(rep.URL); e == nil && u.IsAbs() {
		external = true
	}
	in, start, e := observeIncidentReport(a, caller.Principal, rep, external)
	if e != nil {
		return in, e
	}
	if start {
		ctx, cancel := context.WithTimeout(ctx, 25*time.Minute)
		defer cancel()
		a.repairIncident(ctx, caller.Principal, in.ID)
		st, e := a.store(caller.Principal)
		if e != nil {
			return in, e
		}
		store, e := openIncidents(st)
		if e != nil {
			return in, e
		}
		return store.Get(in.ID)
	}
	return in, nil
}

// LocalShapeWish drives the same console orchestration without HTTP and waits
// for its bounded agent work, including a new app's first draft step.
func LocalShapeWish(ctx context.Context, app core.App, caller core.Caller, opts core.Options, w core.Wish) (core.Candidate, error) {
	if strings.TrimSpace(caller.Principal) == "" {
		return core.Candidate{}, errors.New("shape: a principal is required")
	}
	a := localShaped(app, opts)
	a.opts.TrackingRoot = w.TrackingRoot
	a.opts.Actor = caller.Actor
	a.opts.RepresentedUser = caller.RepresentedUser
	a.opts.Tier = caller.Tier
	a.opts.Circle = caller.Circle
	a.opts.Groups = append([]string(nil), caller.Groups...)
	a.opts.Email = caller.Email
	s := &server{opts: Options{Ctx: ctx, Shape: a.opts}, liveRegistered: true}
	svc := a.candidateService()
	if svc.Kind() == "web" && svc.Owner() != "" {
		p, tier := strings.TrimSpace(caller.Principal), strings.TrimSpace(caller.Tier)
		if p == "anonymous" || tier == "anonymous" || tier == "account" || tier == "signed-in" || strings.HasPrefix(p, "visitor:") {
			return core.Candidate{}, fmt.Errorf("%w: caller %s is not authorized to shape %s", core.ErrUnauthorized, caller.Principal, app.Name)
		}
		ok, err := svc.AdmitsScope(caller, appserver.ScopeShape)
		if err != nil {
			return core.Candidate{}, err
		}
		if !ok {
			c, e := s.fileRequest(a, caller.Principal, wish{Request: w.Request, Page: w.Page, Element: w.Element, Story: w.Story, Todo: w.Todo, operator: false})
			if e != nil {
				return core.Candidate{}, e
			}
			s.shapes.wg.Wait()
			return svc.Show(ctx, caller, c.ID)
		}
	}
	isOperator := svc.Owner() == "" || strings.EqualFold(strings.TrimSpace(caller.Principal), svc.Owner())
	c, e := s.startCandidate(a, caller.Principal, wish{Request: w.Request, Page: w.Page, Element: w.Element, Story: w.Story, Todo: w.Todo, operator: isOperator})
	if e != nil {
		return core.Candidate{}, e
	}
	s.shapes.wg.Wait()
	out, e := svc.Show(ctx, a.caller(c.Principal), c.ID)
	if e != nil {
		return out, e
	}
	if out.Status == core.Failed {
		return out, errors.New(out.Error)
	}
	return out, nil
}
