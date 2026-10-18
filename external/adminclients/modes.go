package adminclients

import (
	"fmt"
	"strings"

	"github.com/qiangli/yoke/external/gws"
)

// Mode is an execution mode. Admission attaches to an exact operation and
// mode, not a product name.
type Mode string

const (
	ModeShortLived      Mode = "short-lived"
	ModeInteractiveAuth Mode = "interactive-auth" // device-code/browser login
	ModeBulk            Mode = "bulk"             // csv/loop/batch fan-out: unbounded mass targeting
	ModeRawAPI          Mode = "raw-api"          // arbitrary Graph/Admin request bypassing the reviewed op table
	ModeOverlap         Mode = "gws-overlap"      // a service gws already covers
)

// gwsOverlap maps GAM command words onto the gws service that covers them.
// GAM is retained only for the administrative gaps gws defers (admin:*).
var gwsOverlap = map[string]string{
	"drive": "drive", "drivefile": "drive", "drivefiles": "drive", "filelist": "drive", "teamdrive": "drive", "teamdrives": "drive",
	"gmail": "gmail", "message": "gmail", "messages": "gmail", "draft": "gmail", "drafts": "gmail", "label": "gmail", "labels": "gmail",
	"calendar": "calendar", "calendars": "calendar", "event": "calendar", "events": "calendar",
	"sheet": "sheets", "sheets": "sheets", "doc": "docs", "docs": "docs", "slides": "slides",
	"chat": "chat", "task": "tasks", "tasks": "tasks", "contact": "contacts", "contacts": "contacts",
}

// ClassifyArgs classifies a client invocation by its command words (argv up
// to the first flag).
func ClassifyArgs(client string, argv []string) Mode {
	var words []string
	for _, a := range argv {
		if strings.HasPrefix(a, "-") {
			break
		}
		words = append(words, a)
	}
	switch client {
	case "m365":
		if len(words) == 0 {
			return ModeShortLived
		}
		switch words[0] {
		case "login", "logout":
			return ModeInteractiveAuth
		case "request", "cli":
			return ModeRawAPI
		}
	case "gam":
		for i, w := range words {
			if i == 0 {
				switch w {
				case "csv", "loop", "batch", "tbatch", "multiprocess":
					return ModeBulk
				case "oauth", "create_project":
					return ModeInteractiveAuth
				}
			}
			if _, ok := gwsOverlap[strings.ToLower(w)]; ok {
				return ModeOverlap
			}
		}
	}
	return ModeShortLived
}

// AdmitMode is the policy gate: only short-lived invocations are admitted.
func AdmitMode(client string, m Mode, argv []string) *gws.Error {
	switch m {
	case ModeShortLived:
		return nil
	case ModeOverlap:
		svc := ""
		for _, a := range argv {
			if s, ok := gwsOverlap[strings.ToLower(a)]; ok {
				svc = s
				break
			}
		}
		return gws.NewError(gws.CodeUnsupported, fmt.Sprintf("%s: this service overlaps gws (%s) — use gws; gam is retained only for administrative gaps gws defers", client, svc), nil).WithOp("admit-mode", client+":"+string(m))
	}
	return gws.NewError(gws.CodeUnsupported,
		fmt.Sprintf("%s: mode %q is not admitted — only bounded, op-table short-lived invocations are (bulk fan-out, raw API requests and interactive logins are refused)", client, m), nil).
		WithOp("admit-mode", client+":"+string(m))
}

// AdmitArgs classifies and gates an argv in one step.
func AdmitArgs(client string, argv []string) (Mode, *gws.Error) {
	m := ClassifyArgs(client, argv)
	return m, AdmitMode(client, m, argv)
}
