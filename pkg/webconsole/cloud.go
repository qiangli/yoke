// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/yoke/external/meshagent"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/principal"
)

// The Cloud section of the launcher (sprint 220, story 72c86b58): the one
// place a host's relationship to the hosted control plane is shown and made.
//
// Paired: two cards, Periscope and Cloudbox, at the addresses the host is
// paired to. Unpaired: a Pair card — the operator pastes an invite code from
// the portal and the console does the rest: provisions the outpost agent
// (binmgr: download → sha256 → cache), runs the pairing exchange, registers
// the supervisor to start at boot. No terminal on any OS. The section shows
// and hides with a switch like Favorites/Recent (a browser preference, in
// app.js); this file is the state and the one action behind it.

// cloudView is what GET /api/cloud answers.
type cloudView struct {
	Paired    bool   `json:"paired"`
	Host      string `json:"host,omitempty"` // the name the control plane knows this host by
	Cloudbox  string `json:"cloudbox"`       // the portal base URL
	Periscope string `json:"periscope"`      // the Periscope address on that portal
	Portal    string `json:"portal"`         // where an invite code is minted
	Outpost   struct {
		Installed bool   `json:"installed"`
		Path      string `json:"path,omitempty"`
	} `json:"outpost"`
	Pair cloudPairState `json:"pair"`
}

// cloudPairState is the Pair action's progress, polled by the page.
type cloudPairState struct {
	State   string    `json:"state"` // idle · provisioning · pairing · installing · paired · error
	Message string    `json:"message,omitempty"`
	At      time.Time `json:"at,omitempty"`
}

type cloudPairer struct {
	mu    sync.Mutex
	state cloudPairState
	// run is the seam a test replaces: it performs the three steps and reports.
	run func(ctx context.Context, code string, report func(state, msg string)) error
}

func (p *cloudPairer) get() cloudPairState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state
}

func (p *cloudPairer) set(state, msg string) {
	p.mu.Lock()
	p.state = cloudPairState{State: state, Message: msg, At: time.Now().UTC()}
	p.mu.Unlock()
}

// start begins a pairing in the background; a second call while one runs is
// refused so two exchanges cannot race for one host name.
func (p *cloudPairer) start(code string) error {
	p.mu.Lock()
	if st := p.state.State; st == "provisioning" || st == "pairing" || st == "installing" {
		p.mu.Unlock()
		return fmt.Errorf("cloud: a pairing is already in progress (%s)", st)
	}
	p.state = cloudPairState{State: "provisioning", Message: "downloading the outpost agent", At: time.Now().UTC()}
	p.mu.Unlock()
	run := p.run
	if run == nil {
		run = pairHost
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if err := run(ctx, code, p.set); err != nil {
			p.set("error", err.Error())
			return
		}
		p.set("paired", "paired; the agent is registered to start at boot")
	}()
	return nil
}

// pairHost is the real sequence: provision → register → service install.
func pairHost(ctx context.Context, code string, report func(state, msg string)) error {
	bin, ok := meshagent.Resolve()
	if !ok {
		report("provisioning", "downloading the outpost agent (checksum-verified)")
		var err error
		if bin, err = meshagent.Ensure(ctx, ""); err != nil {
			return err
		}
	}
	report("pairing", "exchanging the invite code with the portal")
	if out, err := runOutpost(ctx, bin, "register", "--code", code, "--yes"); err != nil {
		return fmt.Errorf("pairing: %s", firstLine(out, err))
	}
	report("installing", "registering the agent to start at boot")
	if out, err := runOutpost(ctx, bin, "service", "install"); err != nil {
		// The host IS paired at this point; say what is left rather than
		// undoing a good pairing over a supervisor registration.
		return fmt.Errorf("paired, but the supervisor could not be registered: %s — run `outpost service install`", firstLine(out, err))
	}
	return nil
}

func runOutpost(ctx context.Context, bin string, args ...string) (string, error) {
	c := exec.CommandContext(ctx, bin, args...)
	var out bytes.Buffer
	c.Stdout, c.Stderr = &out, &out
	err := c.Run()
	return out.String(), err
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

func firstLine(out string, err error) string {
	for _, ln := range strings.Split(ansiRe.ReplaceAllString(out, ""), "\n") {
		if ln = strings.TrimSpace(ln); ln != "" && !strings.HasPrefix(ln, "bashy:") {
			return ln
		}
	}
	return err.Error()
}

// cloudState composes the view from the host: pairing from the principal
// environment, the portal from the fleet's cloud resolution, the agent from
// meshagent.
func (s *server) cloudState() cloudView {
	env := principal.DefaultEnv()
	base, _ := fleet.ResolveCloud("", "")
	base = strings.TrimRight(base, "/")
	v := cloudView{
		Paired:    env.Paired,
		Host:      env.PairedName,
		Cloudbox:  base + "/cloudbox/",
		Periscope: base + "/periscope/",
		Portal:    base + "/cloudbox/",
	}
	if p, ok := meshagent.Resolve(); ok {
		v.Outpost.Installed, v.Outpost.Path = true, p
	}
	if s.cloudPair != nil {
		v.Pair = s.cloudPair.get()
	}
	if v.Pair.State == "" {
		v.Pair.State = "idle"
	}
	return v
}

func (s *server) handleCloudGet(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.cloudState())
}

// handleCloudPair takes {"code": "<invite>"} and starts the pairing; the page
// polls GET /api/cloud for progress. Gated by the console ladder like every
// other write: pairing a host is the largest write this page can make.
func (s *server) handleCloudPair(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&body); err != nil {
		http.Error(w, "cloud: invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	code := strings.TrimSpace(body.Code)
	if code == "" || len(code) > 256 || strings.ContainsAny(code, " \t\r\n") {
		http.Error(w, "cloud: an invite code is one token from the portal's \"Add a machine\" dialog", http.StatusBadRequest)
		return
	}
	if principal.DefaultEnv().Paired {
		http.Error(w, "cloud: this host is already paired", http.StatusConflict)
		return
	}
	if s.cloudPair == nil {
		s.cloudPair = &cloudPairer{}
	}
	if err := s.cloudPair.start(code); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusAccepted, s.cloudState())
}
