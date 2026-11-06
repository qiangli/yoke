package shape

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/qiangli/coreutils/pkg/lockfile"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/service"
	"github.com/qiangli/yoke/pkg/shape/selfbin"
	"github.com/qiangli/yoke/pkg/shape/slot"
	state "github.com/qiangli/yoke/pkg/shape/state"
	"github.com/qiangli/yoke/pkg/src"
)

// ServiceRuntime uses the existing service declaration and supervised process
// machinery. Tests inject a driver; production never invents a provider.
type ServiceRuntime interface {
	Load(context.Context, string) (fleet.Service, error)
	Save(context.Context, fleet.Service) error
	Restart(context.Context, string) error
	Healthy(context.Context, fleet.Service) error
	Stop(context.Context, string) error
	Remove(context.Context, string) error
}
type BashyServiceRuntime struct {
	Bashy   string
	Catalog *fleet.Catalog
}

func (r BashyServiceRuntime) catalog() *fleet.Catalog {
	if r.Catalog != nil {
		return r.Catalog
	}
	return fleet.New()
}
func (r BashyServiceRuntime) Load(_ context.Context, name string) (fleet.Service, error) {
	u, ok := r.catalog().Service(name)
	if !ok {
		return u, fmt.Errorf("%w: no service unit %q", os.ErrNotExist, name)
	}
	return u, nil
}
func (r BashyServiceRuntime) Save(_ context.Context, u fleet.Service) error {
	return r.catalog().SaveService(u)
}
func (r BashyServiceRuntime) command(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	b, e := exec.CommandContext(ctx, selfbin.Resolve(r.Bashy), args...).CombinedOutput()
	if e != nil {
		if len(b) > 2048 {
			b = b[len(b)-2048:]
		}
		return b, fmt.Errorf("shape: service operation: %w: %s", e, strings.TrimSpace(string(b)))
	}
	return b, nil
}
func (r BashyServiceRuntime) Restart(ctx context.Context, name string) error {
	_, e := r.command(ctx, "service", "restart", name)
	return e
}
func (r BashyServiceRuntime) Stop(ctx context.Context, name string) error {
	_, e := r.command(ctx, "service", "stop", name)
	return e
}
func (r BashyServiceRuntime) Remove(_ context.Context, name string) error {
	return r.catalog().RemoveService(name)
}
func (r BashyServiceRuntime) Healthy(ctx context.Context, u fleet.Service) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		b, e := r.command(ctx, "service", "status", u.Name, "--json")
		if e == nil {
			var doc struct {
				Units []service.Status `json:"units"`
			}
			if e = json.Unmarshal(b, &doc); e == nil {
				for _, st := range doc.Units {
					if st.Unit == u.Name && st.State == service.Active && st.Health == "ok" {
						return nil
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("shape: service health failed: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}
func (s Service) runtime() ServiceRuntime {
	if s.Options.ServiceRuntime != nil {
		return s.Options.ServiceRuntime
	}
	return BashyServiceRuntime{}
}
func (s Service) serviceUnit(ctx context.Context) (fleet.Service, error) {
	if s.App.Shaping == nil || s.App.Shaping.Service == "" {
		return fleet.Service{}, errors.New("shape: service kind needs a trusted unit")
	}
	u, e := s.runtime().Load(ctx, s.App.Shaping.Service)
	if e != nil {
		return u, e
	}
	if u.ProviderName() != fleet.ServiceProviderProcess {
		return u, errors.New("shape: service shaping requires an existing container-backed process unit")
	}
	if len(u.Exec) < 2 {
		return u, errors.New("shape: container unit has no direct run argv")
	}
	executable := strings.TrimSuffix(strings.ToLower(filepath.Base(u.Exec[0])), ".exe")
	direct := (executable == "podman" && u.Exec[1] == "run") || ((executable == "bashy" || executable == "bashy.real") && len(u.Exec) > 2 && (u.Exec[1] == "oci" || u.Exec[1] == "podman") && u.Exec[2] == "run")
	if !direct {
		return u, errors.New("shape: service shaping supports direct OCI run units only")
	}
	if len(u.Env) > 0 || len(u.Secrets) > 0 || len(u.Requires) > 0 {
		return u, errors.New("shape: disposable shaping unit cannot inherit environment, secrets or other service dependencies")
	}
	start := 2
	if executable != "podman" {
		start = 3
	}
	image, data, portMapping := false, false, false
	for i := start; i < len(u.Exec); i++ {
		arg := u.Exec[i]
		if arg == "{settings.image}" {
			image = true
			if i != len(u.Exec)-1 {
				return u, errors.New("shape: service image must end direct run argv")
			}
			continue
		}
		switch arg {
		case "--rm", "--init":
		case "--name", "--hostname":
			i++
			if i >= len(u.Exec) || strings.HasPrefix(u.Exec[i], "-") {
				return u, errors.New("shape: missing container option value")
			}
		case "-p", "--publish":
			i++
			if i >= len(u.Exec) {
				return u, errors.New("shape: missing managed publish value")
			}
			v := u.Exec[i]
			if !managedProgramPublish(v) {
				return u, errors.New("shape: every publish must use a declared managed port and loopback/managed bind")
			}

			portMapping = true
		case "-v", "--volume":
			i++
			if i >= len(u.Exec) {
				return u, errors.New("shape: missing data mount")
			}
			v := u.Exec[i]
			if !strings.HasPrefix(v, "{settings.data_dir}:/") || strings.Contains(v, ",") || strings.Count(v, ":") > 2 {
				return u, errors.New("shape: only the declared data_dir bind mount is supported")
			}
			data = true
		default:
			return u, fmt.Errorf("shape: unsupported disposable container option %q", arg)
		}
	}
	if !image || !data || !portMapping {
		return u, errors.New("shape: service unit requires managed image, data_dir bind and port publish")
	}
	if !strings.HasPrefix(u.Settings["image"], "sha256:") || len(u.Settings["image"]) != 71 {
		return u, errors.New("shape: service release image must be an immutable sha256 ID")
	}
	if !filepath.IsAbs(u.Settings["data_dir"]) {
		return u, errors.New("shape: service data_dir must be an existing absolute directory")
	}
	if fi, e := os.Stat(u.Settings["data_dir"]); e != nil || !fi.IsDir() {
		return u, errors.New("shape: service data_dir is unavailable")
	}
	if (u.Health.HTTP == "" && !u.Health.TCP) || len(u.Health.Exec) > 0 {
		return u, errors.New("shape: service shaping requires a declared health check")
	}
	return u, nil
}
func managedProgramPublish(v string) bool {
	parts := strings.Split(v, ":")
	if len(parts) != 3 {
		return false
	}
	validPlaceholder := func(value, prefix string) bool {
		if value == "{"+prefix+"}" {
			return true
		}
		return strings.HasPrefix(value, "{"+prefix+".") && strings.HasSuffix(value, "}") && !strings.ContainsAny(strings.TrimSuffix(strings.TrimPrefix(value, "{"+prefix+"."), "}"), "{} /\\")
	}
	if parts[0] != "127.0.0.1" && !validPlaceholder(parts[0], "bind") {
		return false
	}
	if !validPlaceholder(parts[1], "port") {
		return false
	}
	port, e := strconv.Atoi(parts[2])
	return e == nil && port > 0 && port < 65536
}
func servicePolicy(u fleet.Service) any {
	settings := map[string]string{}
	for k, v := range u.Settings {
		if k != "image" {
			settings[k] = v
		}
	}
	return struct {
		Provider string
		Exec     []string
		Workdir  string
		Env      map[string]string
		Secrets  []fleet.ServiceSecret
		Settings map[string]string
		Ports    []fleet.ServicePort
		Health   fleet.ServiceHealth
	}{u.ProviderName(), u.Exec, u.Workdir, u.Env, u.Secrets, settings, u.Ports, u.Health}
}
func cloneService(u fleet.Service) fleet.Service {
	b, _ := json.Marshal(u)
	var copy fleet.Service
	_ = json.Unmarshal(b, &copy)
	return copy
}

// HostStatus names both selected source and immutable artifact for one host.
type HostStatus struct {
	Kind                string      `json:"kind"`
	Scope               string      `json:"scope"`
	ActivationPrincipal string      `json:"activation_principal"`
	SourceRevision      string      `json:"source_revision,omitempty"`
	Artifact            string      `json:"artifact,omitempty"`
	Path                string      `json:"path,omitempty"`
	Previous            string      `json:"previous,omitempty"`
	Pending             bool        `json:"pending,omitempty"`
	Detail              string      `json:"detail,omitempty"`
	Slot                *slot.State `json:"slot,omitempty"`
	Image               string      `json:"image,omitempty"`
	Unit                string      `json:"unit,omitempty"`
	DataDir             string      `json:"data_dir,omitempty"`
}
type ProgramPreview struct {
	Kind     string `json:"kind"`
	Path     string `json:"path,omitempty"`
	Artifact string `json:"artifact"`
	URL      string `json:"url,omitempty"`
	Unit     string `json:"unit,omitempty"`
	DataDir  string `json:"data_dir,omitempty"`
}
type programTransition struct {
	Candidate      Candidate       `json:"candidate"`
	Artifact       ProgramArtifact `json:"artifact"`
	PreviousSource string          `json:"previous_source"`
	PreviousSlot   *slot.Version   `json:"previous_slot,omitempty"`
	TargetSlot     *slot.Version   `json:"target_slot,omitempty"`
	PreviousUnit   *fleet.Service  `json:"previous_unit,omitempty"`
	TargetUnit     *fleet.Service  `json:"target_unit,omitempty"`
	ReleasePath    string          `json:"release_path,omitempty"`
	Phase          string          `json:"phase"`
}

func (s Service) hostDir() (string, error) {
	home, e := s.home()
	return filepath.Join(home, "shape-host", s.App.Name), e
}
func (s Service) transitionPath() (string, error) {
	dir, e := s.hostDir()
	return filepath.Join(dir, "source-transition.json"), e
}
func (s Service) hostLock() (*lockfile.Lock, error) {
	dir, e := s.hostDir()
	if e != nil {
		return nil, e
	}
	return lockfile.AcquireWithin(filepath.Join(dir, ".source.lock"), 30*time.Second, lockfile.Holder{Intent: "host shaping source/artifact transition"})
}
func (s Service) writeTransition(v programTransition) error {
	path, e := s.transitionPath()
	if e != nil {
		return e
	}
	if e = os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".transition-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(append(b, '\n'))
	}
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	if e = os.Rename(f.Name(), path); e != nil {
		return e
	}
	return syncProgramDir(filepath.Dir(path))
}
func (s Service) readTransition() (programTransition, error) {
	path, e := s.transitionPath()
	if e != nil {
		return programTransition{}, e
	}
	b, e := os.ReadFile(path)
	if e != nil {
		return programTransition{}, e
	}
	var v programTransition
	e = json.Unmarshal(b, &v)
	return v, e
}
func (s Service) clearTransition() error {
	path, e := s.transitionPath()
	if e != nil {
		return e
	}
	e = os.Remove(path)
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		return e
	}
	return syncProgramDir(filepath.Dir(path))
}
func (s Service) hostSource() (string, error) {
	home, e := s.home()
	if e != nil {
		return "", e
	}
	st, e := state.OpenAt(home, s.App.Name, HostPrincipal)
	if e != nil {
		return "", e
	}
	v, e := st.Read()
	if e != nil {
		return "", e
	}
	if v.ActiveRevision != "" {
		return v.ActiveRevision, nil
	}
	r, e := srcRevision(s.App.SourceDir)
	return r, e
}
func srcRevision(dir string) (string, error) { r, e := srcRepoHead(dir); return r, e }
func (s Service) slotStore() slot.Store {
	home, _ := s.home()
	return slot.Store{Home: home, App: s.App.Name, ReleaseRevision: s.App.ReleaseRevision}
}
func (s Service) applyProgramTarget(ctx context.Context, t programTransition) error {
	if s.Kind() == "cli" {
		if t.TargetSlot == nil {
			return errors.New("shape: missing staged CLI target")
		}
		_, e := s.slotStore().Activate(*t.TargetSlot, t.ReleasePath)
		return e
	}
	if t.TargetUnit == nil {
		return errors.New("shape: missing service target")
	}
	r := s.runtime()
	if e := r.Save(ctx, *t.TargetUnit); e != nil {
		return e
	}
	if e := r.Restart(ctx, t.TargetUnit.Name); e != nil {
		return e
	}
	return r.Healthy(ctx, *t.TargetUnit)
}
func (s Service) rollbackProgram(ctx context.Context, t programTransition) error {
	if s.Kind() == "cli" {
		ref := "release"
		if t.PreviousSlot != nil {
			ref = t.PreviousSlot.Digest
		}
		_, e := s.slotStore().Restore(ref)
		return e
	}
	if t.PreviousUnit == nil {
		return errors.New("shape: missing retained service unit")
	}
	r := s.runtime()
	if e := r.Save(ctx, *t.PreviousUnit); e != nil {
		return e
	}
	if e := r.Restart(ctx, t.PreviousUnit.Name); e != nil {
		return e
	}
	return r.Healthy(ctx, *t.PreviousUnit)
}
func (s Service) recoverProgramLocked(ctx context.Context) error {
	t, e := s.readTransition()
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		return e
	}
	current, e := s.hostSource()
	if e != nil {
		return e
	}
	switch current {
	case t.Candidate.Rev:
		if _, e = s.requireProgram(ctx, t.Candidate); e != nil {
			return fmt.Errorf("shape: interrupted target requires valid proof: %w", e)
		}
		if e = s.applyProgramTarget(ctx, t); e != nil {
			return e
		}
		return s.clearTransition()
	case t.PreviousSource:
		if e = s.rollbackProgram(ctx, t); e != nil {
			return e
		}
		return s.clearTransition()
	default:
		return errors.New("shape: interrupted transition conflicts with current source; retained release remains available")
	}
}
func (s Service) RecoverProgram(ctx context.Context) error {
	if s.Kind() == "web" {
		return nil
	}
	l, e := s.hostLock()
	if e != nil {
		return e
	}
	defer l.Release()
	return s.recoverProgramLocked(ctx)
}
func (s Service) activateProgram(ctx context.Context, c Candidate) error {
	l, e := s.hostLock()
	if e != nil {
		return e
	}
	defer l.Release()
	if e = s.recoverProgramLocked(ctx); e != nil {
		return e
	}
	artifact, e := s.requireProgram(ctx, c)
	if e != nil {
		return e
	}
	home, e := s.home()
	if e != nil {
		return e
	}
	previous, e := s.hostSource()
	if e != nil {
		return e
	}
	if previous != c.BaseRev {
		return fmt.Errorf("%w: host source changed", ErrConflict)
	}
	t := programTransition{Candidate: c, Artifact: artifact, PreviousSource: previous, ReleasePath: s.App.ReleasePath, Phase: "prepared"}
	if s.Kind() == "cli" {
		store := s.slotStore()
		current, e := store.Status()
		if e != nil {
			return e
		}
		if current.Release == nil && (s.App.ReleaseRevision == "" || s.App.ReleaseRevision != c.BaseRev) {
			return errors.New("shape: installed CLI release source identity is unknown or mismatched; fetch the exact pinned release before activation")
		}
		version, e := store.Stage(artifact.Path, c.Rev, artifact.Evidence)
		if e != nil {
			return e
		}
		if "sha256:"+version.Digest != artifact.Digest {
			return errors.New("shape: staged executable digest changed")
		}
		t.TargetSlot = &version
		t.PreviousSlot = current.Active
	} else {
		u, e := s.serviceUnit(ctx)
		if e != nil {
			return e
		}
		if _, e = s.retainServiceRelease(ctx, u, c.BaseRev); e != nil {
			return e
		}
		target := cloneService(u)
		target.Settings["image"] = artifact.Digest
		t.PreviousUnit = &u
		t.TargetUnit = &target
	}
	if e = s.writeTransition(t); e != nil {
		return e
	}
	req := ActivateRequest{App: s.App.Name, Principal: HostPrincipal, SourceDir: s.App.SourceDir, CandidateDir: filepath.Join(c.Workspace), BaseRev: c.BaseRev, CandidateRev: c.Rev, Request: c.Request, Agent: c.Agent, Evidence: artifact.Evidence}
	activator := &ProductionActivator{Home: home, gate: s.programGate(ctx, artifact), beforeActivate: func() error {
		if e := s.applyProgramTarget(ctx, t); e != nil {
			return e
		}
		t.Phase = "artifact"
		return s.writeTransition(t)
	}}
	if e = activator.Activate(ctx, req); e != nil {
		if errors.Is(e, src.ErrStaleBase) && t.Phase == "prepared" {
			return errors.Join(e, s.clearTransition())
		}
		current, re := s.hostSource()
		if re == nil && current == c.Rev {
			return s.clearTransition()
		}
		if re == nil && current == t.PreviousSource {
			if rollback := s.rollbackProgram(context.WithoutCancel(ctx), t); rollback == nil {
				_ = s.clearTransition()
			} else {
				return fmt.Errorf("shape: activation failed (%v); retained rollback pending: %w", e, rollback)
			}
		}
		return e
	}
	return s.clearTransition()
}

func srcRepoHead(dir string) (string, error) {
	r, e := gogit.PlainOpen(dir)
	if e != nil {
		return "", e
	}
	h, e := r.Head()
	if e != nil {
		return "", e
	}
	return h.Hash().String(), nil
}

func (s Service) HostStatus(ctx context.Context, caller Caller) (HostStatus, error) {
	if s.Kind() == "web" {
		return HostStatus{}, errors.New("shape: web apps have principal-scoped status")
	}
	if e := s.validate(caller); e != nil {
		return HostStatus{}, e
	}
	out := HostStatus{Kind: s.Kind(), Scope: "host", ActivationPrincipal: HostPrincipal}
	var e error
	out.SourceRevision, e = s.hostSource()
	if e != nil {
		return out, e
	}
	_, e = s.readTransition()
	if e == nil {
		out.Pending = true
		out.Detail = "source/artifact transition needs recovery before another mutation"
	} else if !os.IsNotExist(e) {
		return out, e
	}
	if s.Kind() == "cli" {
		st, e := s.slotStore().Status()
		out.Slot = &st
		if e != nil {
			return out, e
		}
		selected := st.Active
		if selected == nil {
			selected = st.Release
		}
		if selected != nil {
			out.Artifact = "sha256:" + selected.Digest
			out.Path = selected.Path
			if selected.Revision != "unknown" && selected.Revision != out.SourceRevision {
				out.Pending = true
				out.Detail = "artifact/source selection differs; use normal host restore to reconcile"
			}
		}
		if st.Previous != nil {
			out.Previous = st.Previous.Revision
		}
	} else {
		u, e := s.serviceUnit(ctx)
		if e != nil {
			return out, e
		}
		out.Unit = u.Name
		out.Image = u.Settings["image"]
		out.Artifact = out.Image
		out.DataDir = u.Settings["data_dir"]
		if a, e := s.loadArtifact(out.SourceRevision); e == nil && a.Digest != out.Image {
			out.Pending = true
			out.Detail = "service image/source differs; use host restore to reconcile"
		} else if e != nil && !os.IsNotExist(e) {
			return out, e
		}

	}
	return out, nil
}
func (s Service) restoreTarget(ctx context.Context, ref string) (string, string, error) {
	home, e := s.home()
	if e != nil {
		return "", "", e
	}
	if s.Kind() == "cli" {
		st, e := s.slotStore().Status()
		if e != nil {
			return "", "", e
		}
		if ref == "release" || ref == "base" {
			if st.Release == nil || !sourceSHA(st.Release.Revision) {
				return "", "", errors.New("shape: retained release source identity is unknown")
			}
			return st.Release.Revision, st.Release.Path, nil
		}
		for _, v := range []*slot.Version{st.Release, st.Active, st.Previous} {
			if v != nil && (ref == v.Revision || ref == v.Digest || ref == "sha256:"+v.Digest) {
				if !sourceSHA(v.Revision) {
					return "", "", errors.New("shape: retained artifact source identity is unknown")
				}
				return v.Revision, v.Path, nil
			}
		}
	}
	if ref == "release" || ref == "base" {
		return s.retainedServiceArtifact()
	}
	if s.Kind() == "service" {
		if release, e := s.loadServiceRelease(); e == nil && ref == release.Revision {
			return s.retainedServiceArtifact()
		}
	}
	h, e := src.History(home, s.App.Name, HostPrincipal)
	if e != nil {
		return "", "", e
	}
	for _, v := range h.Entries {
		if v.Revision == ref {
			return ref, "", nil
		}
	}
	for _, v := range h.Named {
		if v.Name == ref || v.Revision == ref {
			return v.Revision, "", nil
		}
	}
	return "", "", errors.New("shape: restore target is not in this host's verified history")
}

// HostUndo and HostRestore re-run the trusted frozen gate before switching both
// source and artifact. Product emergency recovery is separate and keeps exact
// retained release bytes runnable even when normal proof is unavailable.
func (s Service) HostUndo(ctx context.Context, caller Caller) (HostStatus, error) {
	home, e := s.home()
	if e != nil {
		return HostStatus{}, e
	}
	st, e := state.OpenAt(home, s.App.Name, HostPrincipal)
	if e != nil {
		return HostStatus{}, e
	}
	v, e := st.Read()
	if e != nil {
		return HostStatus{}, e
	}
	target := v.LastKnownGood
	if target == "" && s.Kind() == "cli" {
		slots, e := s.slotStore().Status()
		if e != nil {
			return HostStatus{}, e
		}
		if slots.Previous != nil && sourceSHA(slots.Previous.Revision) && slots.Previous.Revision != v.ActiveRevision {
			target = slots.Previous.Revision
		}
	}
	if target == "" && s.Kind() == "service" {
		release, e := s.loadServiceRelease()
		if e != nil && !os.IsNotExist(e) {
			return HostStatus{}, e
		}
		if e == nil && sourceSHA(release.Revision) && release.Revision != v.ActiveRevision {
			target = release.Revision
		}
	}
	if target == "" {
		return HostStatus{}, errors.New("shape: no previous host version")
	}
	return s.HostRestore(ctx, caller, target)
}
func (s Service) HostRestore(ctx context.Context, caller Caller, ref string) (HostStatus, error) {
	if s.Kind() == "web" {
		return HostStatus{}, errors.New("shape: use normal web history restore")
	}
	if e := s.RecoverProgram(ctx); e != nil {
		return HostStatus{}, e
	}
	target, retained, e := s.restoreTarget(ctx, ref)
	if e != nil {
		return HostStatus{}, e
	}
	c, e := s.begin(ctx, caller, Wish{Request: "Restore host app to " + ref}, true)
	if e != nil {
		return HostStatus{}, e
	}
	r, e := CloneAt(ctx, s.App.SourceDir, c.Workspace, target)
	if e != nil {
		_, _ = s.Complete(ctx, caller, c, e)
		return HostStatus{}, e
	}
	_ = r
	c.Rev = target
	c.Status = Ready
	c, e = s.Complete(ctx, caller, c, nil)
	if e != nil {
		return HostStatus{}, e
	}
	// A retained artifact override is private to this restore path and carries
	// the known release source stamp; fresh candidates always test built bytes.
	verifier := programVerifier{service: s, retained: retained}
	verdict, e := verifier.Verify(ctx, VerifyRequest{App: c.App, Principal: HostPrincipal, SourceDir: s.App.SourceDir, CandidateDir: c.Workspace, BaseRev: c.BaseRev, CandidateRev: c.Rev, Agent: c.Agent})
	if e != nil {
		return HostStatus{}, e
	}
	c.Verdict = &verdict
	c.Artifact = verdict.Artifact
	c.ArtifactPath = verdict.ArtifactPath
	c.ProgramPolicy = verdict.Policy
	if !verdict.Pass {
		return HostStatus{}, ErrVerification
	}
	c.Status = Verified
	dir, _ := s.CandidateDir(c.Principal, c.ID)
	if e = SaveCandidate(dir, c); e != nil {
		return HostStatus{}, e
	}
	// Approval's normal reverify would replace a retained release with rebuilt
	// bytes. The same exact sealed kind gate is used directly after fresh proof.
	l, e := s.lock(caller)
	if e != nil {
		return HostStatus{}, e
	}
	defer l.Release()
	c.Status = "activating"
	if e = SaveCandidate(dir, c); e != nil {
		return HostStatus{}, e
	}
	if e = s.activateProgram(ctx, c); e != nil {
		return HostStatus{}, e
	}
	c.Status = Approved
	if _, e = s.finishApproval(ctx, c, dir); e != nil {
		return HostStatus{}, e
	}
	return s.HostStatus(ctx, caller)
}

func (s Service) PreviewProgram(ctx context.Context, caller Caller, id string) (ProgramPreview, error) {
	if s.Kind() == "web" {
		return ProgramPreview{}, errors.New("shape: web candidates use console preview")
	}
	c, e := s.Show(ctx, caller, id)
	if e != nil {
		return ProgramPreview{}, e
	}
	if c.Status != Verified {
		return ProgramPreview{}, errors.New("shape: verify the program candidate before preview")
	}
	a, e := s.requireProgram(ctx, c)
	if e != nil {
		return ProgramPreview{}, e
	}
	preview := ProgramPreview{Kind: s.Kind(), Artifact: a.Digest, Path: a.Path}
	if s.Kind() == "cli" {
		return preview, nil
	}
	if c.ProgramPreview != nil && c.ProgramPreview.Unit != "" {
		u, e := s.runtime().Load(ctx, c.ProgramPreview.Unit)
		if e == nil {
			if u.Settings["image"] != a.Digest || u.Settings["data_dir"] != c.ProgramPreview.DataDir {
				return preview, errors.New("shape: preview unit ownership changed")
			}
			if e = s.runtime().Healthy(ctx, u); e != nil {
				return preview, e
			}
			return *c.ProgramPreview, nil
		} else if !errors.Is(e, os.ErrNotExist) {
			return preview, e
		}
	}
	preview, e = s.startServicePreview(ctx, c.ID, a)
	if e != nil {
		return preview, e
	}
	_, e = s.Update(ctx, caller, id, func(c *Candidate, _ string) error { c.ProgramPreview = &preview; return nil })
	if e != nil {
		e = errors.Join(e, s.cleanupProgramPreview(context.WithoutCancel(ctx), Candidate{ID: c.ID, ProgramPreview: &preview}))
	}
	return preview, e
}
func (s Service) startServicePreview(ctx context.Context, id string, a ProgramArtifact) (ProgramPreview, error) {
	preview := ProgramPreview{Kind: "service", Artifact: a.Digest, Path: a.Path}
	u, e := s.serviceUnit(ctx)
	if e != nil {
		return preview, e
	}
	home, e := s.home()
	if e != nil {
		return preview, e
	}
	data := filepath.Join(home, "shape-host", s.App.Name, "previews", id, "data")
	name := "shape-preview-" + s.App.Name + "-" + id
	if existing, le := s.runtime().Load(ctx, name); le == nil {
		if existing.Settings["data_dir"] != data || existing.Settings["image"] != a.Digest {
			return preview, errors.New("shape: existing preview ownership differs")
		}
		pending := ProgramPreview{Unit: name, DataDir: data, Artifact: a.Digest}
		if e = s.cleanupProgramPreview(ctx, Candidate{ID: id, ProgramPreview: &pending}); e != nil {
			return preview, fmt.Errorf("shape: preview cleanup pending for %s: %w", name, e)
		}
	} else if !errors.Is(le, os.ErrNotExist) {
		return preview, le
	}

	if e = os.MkdirAll(data, 0700); e != nil {
		return preview, e
	}
	if e = copyPreviewData(u.Settings["data_dir"], data); e != nil {
		return preview, e
	}
	target := cloneService(u)
	target.Name = "shape-preview-" + s.App.Name + "-" + id
	for i, arg := range target.Exec {
		if arg == "--name" && i+1 < len(target.Exec) {
			target.Exec[i+1] = target.Name
		}
		if strings.HasPrefix(arg, "--name=") {
			target.Exec[i] = "--name=" + target.Name
		}
	}
	target.Env = nil
	target.Secrets = nil
	target.Settings["image"] = a.Digest
	target.Settings["data_dir"] = data
	target.Enabled = true
	target.Expose = fleet.ServiceExpose{}
	for i := range target.Ports {
		listener, e := net.Listen("tcp", "127.0.0.1:0")
		if e != nil {
			return preview, e
		}
		target.Ports[i].Port = listener.Addr().(*net.TCPAddr).Port
		target.Ports[i].Bind = "127.0.0.1"
		target.Ports[i].Env = ""
		_ = listener.Close()
	}
	if len(target.Ports) == 0 {
		return preview, errors.New("shape: service preview requires a declared side-port listener")
	}
	r := s.runtime()
	if existing, le := r.Load(ctx, target.Name); le == nil {
		return preview, fmt.Errorf("shape: preview unit name already exists: %s", existing.Name)
	} else if !errors.Is(le, os.ErrNotExist) {
		return preview, le
	}
	if e = r.Save(ctx, target); e != nil {
		return preview, e
	}
	cleanup := func() error {
		if e := r.Stop(context.WithoutCancel(ctx), target.Name); e != nil {
			return e
		}
		if e := r.Remove(context.WithoutCancel(ctx), target.Name); e != nil {
			return e
		}
		return os.RemoveAll(filepath.Dir(data))
	}
	if e = r.Restart(ctx, target.Name); e != nil {
		return preview, errors.Join(e, cleanup())
	}
	if e = r.Healthy(ctx, target); e != nil {
		return preview, errors.Join(e, cleanup())
	}
	preview.Unit = target.Name
	preview.DataDir = data
	preview.URL = fmt.Sprintf("http://127.0.0.1:%d/", target.Ports[0].Port)
	return preview, nil
}

func copyPreviewData(source, dest string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		rel, e := filepath.Rel(source, path)
		if e != nil {
			return e
		}
		if rel == "." {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("shape: preview cannot follow data symlink %s", rel)
		}
		target := filepath.Join(dest, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("shape: preview data is not a regular file: %s", rel)
		}
		b, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		return os.WriteFile(target, b, 0600)
	})
}
func (s Service) cleanupProgramPreview(ctx context.Context, c Candidate) error {
	if c.ProgramPreview == nil || c.ProgramPreview.Unit == "" {
		return nil
	}
	expected := "shape-preview-" + s.App.Name + "-" + c.ID
	if c.ProgramPreview.Unit != expected {
		return errors.New("shape: preview identity mismatch")
	}
	r := s.runtime()
	unit, loadErr := r.Load(ctx, expected)
	if loadErr != nil && !errors.Is(loadErr, os.ErrNotExist) {
		return loadErr
	}
	if loadErr == nil {
		if unit.Settings["data_dir"] != c.ProgramPreview.DataDir || unit.Settings["image"] != c.ProgramPreview.Artifact {
			return errors.New("shape: preview unit ownership changed")
		}
		if e := r.Stop(ctx, expected); e != nil {
			return e
		}
		if e := r.Remove(ctx, expected); e != nil {
			return e
		}
	}
	home, e := s.home()
	if e != nil {
		return e
	}
	return os.RemoveAll(filepath.Join(home, "shape-host", s.App.Name, "previews", c.ID))
}

type serviceRelease struct {
	Revision string        `json:"revision"`
	Unit     fleet.Service `json:"unit"`
}
type serviceRevisionRuntime interface {
	ReleaseRevision(context.Context, fleet.Service) (string, error)
}

func (r BashyServiceRuntime) ReleaseRevision(ctx context.Context, u fleet.Service) (string, error) {
	b, e := r.command(ctx, "oci", "image", "inspect", u.Settings["image"])
	if e != nil {
		return "", e
	}
	var docs []struct {
		Labels map[string]string `json:"Labels"`
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
	}
	if e = json.Unmarshal(b, &docs); e != nil || len(docs) != 1 {
		return "", errors.New("shape: cannot read installed image source identity")
	}
	revision := docs[0].Labels["org.opencontainers.image.revision"]
	if revision == "" {
		revision = docs[0].Config.Labels["org.opencontainers.image.revision"]
	}
	if !sourceSHA(revision) {
		return "", errors.New("shape: installed image has no exact source revision label")
	}
	return revision, nil
}
func sourceSHA(v string) bool {
	if len(v) != 40 {
		return false
	}
	_, e := hex.DecodeString(v)
	return e == nil
}
func (s Service) serviceReleaseFile() (string, error) {
	dir, e := s.hostDir()
	return filepath.Join(dir, "release-service.json"), e
}
func (s Service) loadServiceRelease() (serviceRelease, error) {
	p, e := s.serviceReleaseFile()
	if e != nil {
		return serviceRelease{}, e
	}
	b, e := os.ReadFile(p)
	if e != nil {
		return serviceRelease{}, e
	}
	var v serviceRelease
	e = json.Unmarshal(b, &v)
	if e == nil && !sourceSHA(v.Revision) {
		e = errors.New("shape: retained service source identity is unknown")
	}
	return v, e
}
func (s Service) retainServiceRelease(ctx context.Context, u fleet.Service, base string) (serviceRelease, error) {
	if v, e := s.loadServiceRelease(); e == nil {
		return v, nil
	} else if !os.IsNotExist(e) {
		return v, e
	}
	revision := s.App.ReleaseRevision
	if resolver, ok := s.runtime().(serviceRevisionRuntime); ok {
		actual, e := resolver.ReleaseRevision(ctx, u)
		if e != nil {
			return serviceRelease{}, e
		}
		if revision != "" && revision != actual {
			return serviceRelease{}, errors.New("shape: service release source stamp mismatch")
		}
		revision = actual
	}
	if !sourceSHA(revision) || revision != base {
		return serviceRelease{}, errors.New("shape: installed service image source identity is unknown or mismatched")
	}
	v := serviceRelease{Revision: revision, Unit: cloneService(u)}
	p, e := s.serviceReleaseFile()
	if e != nil {
		return v, e
	}
	if e = os.MkdirAll(filepath.Dir(p), 0700); e != nil {
		return v, e
	}
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return v, e
	}
	f, e := os.CreateTemp(filepath.Dir(p), ".release-*")
	if e != nil {
		return v, e
	}
	defer os.Remove(f.Name())
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(append(b, '\n'))
	}
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil {
		return v, e
	}
	if ce != nil {
		return v, ce
	}
	if e = os.Rename(f.Name(), p); e != nil {
		return v, e
	}
	return v, syncProgramDir(filepath.Dir(p))
}
func (s Service) retainedServiceArtifact() (string, string, error) {
	v, e := s.loadServiceRelease()
	if e != nil {
		return "", "", e
	}
	dir, e := s.hostDir()
	if e != nil {
		return "", "", e
	}
	path := filepath.Join(dir, "release-image")
	if e = os.WriteFile(path, []byte(v.Unit.Settings["image"]+"\n"), 0600); e != nil {
		return "", "", e
	}
	return v.Revision, path, nil
}

// RunProgramPreview executes a verified candidate with private state and bounded capture.
func (s Service) RunProgramPreview(ctx context.Context, caller Caller, id string, args ...string) (ProgramPreviewResult, error) {
	preview, e := s.PreviewProgram(ctx, caller, id)
	if e != nil {
		return ProgramPreviewResult{}, e
	}
	if preview.Kind != "cli" {
		return ProgramPreviewResult{}, errors.New("shape: service previews use their managed side-port URL")
	}
	return runShapeCLIPreview(ctx, preview, args)
}
