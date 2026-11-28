// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/ask"
	"github.com/qiangli/yoke/pkg/coopauth"
	"github.com/qiangli/yoke/pkg/hostauth"
)

// needView is one "needs you" request as the sprint board shows it: what is
// asked, the options with the recommendation marked, how old it is and when
// it is due. It never carries the requester's argv or cwd.
type needView struct {
	ID         string               `json:"id"`
	Short      string               `json:"short"`
	Kind       string               `json:"kind"`
	Title      string               `json:"title"`
	State      string               `json:"state"`
	Urgency    string               `json:"urgency"`
	Options    []ask.ApprovalOption `json:"options,omitempty"`
	Recommend  string               `json:"recommend,omitempty"`
	Refs       []string             `json:"refs,omitempty"`
	Risk       []string             `json:"risk,omitempty"`
	Sprint     string               `json:"sprint,omitempty"`
	From       string               `json:"from,omitempty"`
	Human      string               `json:"human"`
	Created    time.Time            `json:"created"`
	Deadline   time.Time            `json:"deadline,omitzero"`
	AgeSeconds int                  `json:"age_seconds"`
	Answer     string               `json:"answer"` // the CLI line that answers it
}

// needsForBoard lists the needs requests waiting on the human: open, overdue,
// or escalated from a delegate. A request the delegate is still handling is
// not an alert. Read on every overview request rather than cached with the
// board: it is a directory read, and an answered request must leave the
// alert at the next poll, not half a minute later.
func needsForBoard(now time.Time) []needView {
	pending, err := ask.PendingNeeds()
	if err != nil {
		return nil
	}
	out := []needView{}
	for _, r := range pending {
		if !ask.NeedsHuman(r, now) {
			continue
		}
		n := r.Approval.Need
		short := r.ID
		if len(short) > 8 {
			short = short[:8]
		}
		out = append(out, needView{
			ID: r.ID, Short: short, Kind: n.Kind, Title: n.Title,
			State: ask.NeedState(r, now), Urgency: n.Urgency,
			Options: r.Approval.Options, Recommend: n.Recommend, Refs: n.Refs, Risk: n.Risk,
			Sprint: r.Approval.Sprint, From: n.From, Human: n.Human,
			Created: r.Created, Deadline: n.Deadline,
			AgeSeconds: int(now.Sub(r.Created).Seconds()),
			Answer:     "bashy approve allow " + short + answerOptionHint(n.Recommend),
		})
	}
	return out
}

func answerOptionHint(rec string) string {
	if rec == "" {
		return ""
	}
	return " --option " + rec
}

// boardAnswerer says who may answer on the board, and why not when nobody may.
//
// The board is reachable from anywhere through the cloud relay, and from the
// host on loopback. Loopback admits the machine owner WITHOUT a sign-in, which
// is fine for reading and not for answering: every agent on the host reaches
// loopback too. So an answer counts only from a signed-in session (minted for
// the host owner) or a cloud caller whose owner relationship outpost signed.
func (s *server) boardAnswerer(r *http.Request) (string, string) {
	user, via := s.userOf(r)
	switch via {
	case "session":
		// A paired device holds a session too (user#dev:ID), and its scope
		// may include the board; it reads the alert but never answers as
		// the owner. Only the owner's own sign-in does.
		if _, _, device := splitDeviceSubject(user); device {
			return "", "a paired device cannot answer; sign in as the host owner"
		}
		owner := currentOSUser()
		if !hostauth.SameUser(user, owner) {
			return "", "only the host owner answers needs requests"
		}
		return owner, ""
	case "cloud":
		var secrets [][]byte
		if s.guard != nil && len(s.guard.Secret) > 0 {
			secrets = append(secrets, s.guard.Secret)
		}
		if t, ok := loadOutpostTrust(s.cloudApp()); ok && len(t.ssoSecret) > 0 {
			secrets = append(secrets, t.ssoSecret)
		}
		for _, secret := range secrets {
			if rel, ok := coopauth.VerifyRelationship(r, secret); ok && rel.Kind == coopauth.RelOwner {
				return currentOSUser(), ""
			}
		}
		return "", "only the host owner answers needs requests"
	}
	return "", "sign in to answer here, or answer at a shell with bashy approve"
}

type needAnswerBody struct {
	Verdict string `json:"verdict"` // allow | deny
	Option  string `json:"option,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// handleNeedAnswer is the board's ONE write: the human answers one needs
// request. It takes no identity from the request body; the answerer is the
// authenticated caller (boardAnswerer), and pkg/ask refuses anyone who is not
// the request's human. It touches no lease, no sprint and no run.
func (s *server) handleNeedAnswer(w http.ResponseWriter, r *http.Request) {
	human, why := s.boardAnswerer(r)
	if human == "" {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": why})
		return
	}
	var in needAnswerBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "malformed request"})
		return
	}
	if in.Verdict != ask.VerdictAllow && in.Verdict != ask.VerdictDeny {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "verdict must be allow or deny"})
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	req, err := ask.Find(id)
	if err != nil || !req.IsNeed() {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no needs request " + id})
		return
	}
	d := ask.Decision{Verdict: in.Verdict, OptionID: strings.TrimSpace(in.Option), Reason: strings.TrimSpace(in.Reason)}
	if _, err := ask.Decide(req.ID, d, ask.DecideOpts{Via: ask.ViaBoard, Caller: ask.Caller{Human: human}}); err != nil {
		switch {
		case errors.Is(err, ask.ErrDecided):
			writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
		case errors.Is(err, ask.ErrNotYours):
			writeJSON(w, http.StatusForbidden, map[string]any{"error": err.Error()})
		default:
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		}
		return
	}
	dec, _ := ask.LoadDecision(req.ID)
	writeJSON(w, http.StatusOK, map[string]any{"id": req.ID, "decision": dec})
}
