package cligw

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/qiangli/yoke/pkg/capability"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/toolcmd"
)

// Capability requests (Sprint 418 S9): a caller — typically a peer door's
// `tool cmd run --capability X --peer NAME` — names a capability instead of a
// command. THIS door picks the provider from ITS OWN host measurements and
// measured quota (toolcmd.SelectMeasuredCommand: host samples only, never
// seeded priors, the caller's vendor excluded) and then runs the chosen
// declared command through the ordinary slash path, so routing, quota policy,
// guards and the reply shape are exactly a slash request's. The selection
// rides back in x_bashy_tool_command.selection so the caller can record it.
const (
	// CapabilityHeader names the capability (e.g. "deep-research").
	CapabilityHeader = toolcmd.CapabilityHeader
	// CallerHeader names the requesting vendor, excluded from selection.
	CallerHeader = toolcmd.CallerHeader
)

// selectMeasured is the measured selector (a seam for tests).
var selectMeasured = func(ctx context.Context, capText, caller string, cat *fleet.Catalog) (toolcmd.Selection, error) {
	_, _, _, sel, err := toolcmd.SelectMeasuredCommand(ctx, capText, caller, cat)
	return sel, err
}

// capabilitySelection resolves a CapabilityHeader into a slash filter and an
// agent selector. No header: (nil, nil) and nothing changes.
func (s *Server) capabilitySelection(r *http.Request, filter *Filter, model *string) (*toolcmd.Selection, *slashFailure) {
	capText := strings.TrimSpace(r.Header.Get(CapabilityHeader))
	if capText == "" {
		return nil, nil
	}
	bad := func(status int, typ, msg string) (*toolcmd.Selection, *slashFailure) {
		return nil, &slashFailure{Status: status, Type: typ, Message: msg, Command: capText}
	}
	if filter.Slash != "" {
		return bad(http.StatusBadRequest, "invalid_request", "cligw: give slash= or "+CapabilityHeader+", not both")
	}
	if !strings.EqualFold(strings.TrimSpace(*model), "auto") {
		return bad(http.StatusBadRequest, "invalid_request", fmt.Sprintf("cligw: %s selects its own measured binding; send model \"auto\", not %q", CapabilityHeader, *model))
	}
	if _, ok := capability.ParseCapability(capText); !ok {
		return bad(http.StatusBadRequest, "tool_command_unknown", fmt.Sprintf("cligw: unknown capability %q", capText))
	}
	sel, err := selectMeasured(r.Context(), capText, strings.TrimSpace(r.Header.Get(CallerHeader)), s.catalog.fleet)
	if err != nil {
		return bad(http.StatusNotFound, "tool_command_no_candidate", err.Error())
	}
	filter.Slash = sel.Command
	*model = sel.Binding
	return &sel, nil
}
