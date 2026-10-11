package toolcmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/capability"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/llmbudget"
	"github.com/qiangli/yoke/pkg/room"
)

// Selection is the auditable result of capability routing.  Vendor and Binding
// identify what will actually run; neither is inferred from the caller's tool.
type Selection struct {
	Capability      string  `json:"capability"`
	Vendor          string  `json:"vendor"`
	Binding         string  `json:"binding"`
	Command         string  `json:"command"`
	RemainingQuota  float64 `json:"remaining_quota"`
	MeasuredSamples int     `json:"measured_samples"`
}

var publishSelection = room.Emit

// ErrPeerDispatchUnavailable refuses a capability call addressed to another
// sphere host. A peer door today authenticates only its owner bearer and
// advertises no command catalog or measured quota, so a peer candidate could
// not be ranked from authentic evidence nor admitted as a peer: dispatch
// waits on Sprint 308 A3 (7a5f32dbaafa, peer door catalog/capacity
// advertisement and authenticated dispatch) and A5 (53fde6fa2d0d, per-peer
// bearer and share: allow/deny). Forwarding the owner token is not a share.
var ErrPeerDispatchUnavailable = errors.New("toolcmd: peer capability dispatch is unavailable until Sprint 308 A3 (7a5f32dbaafa: peer door advertisement and authenticated dispatch) and A5 (53fde6fa2d0d: per-peer bearer and share policy) land; refusing before selection, credentials or network")

// checkPeerDispatch fails closed for any non-empty peer. The peer value is
// deliberately not echoed: an operator may pass a URL carrying credentials.
func checkPeerDispatch(peer string) error {
	if strings.TrimSpace(peer) == "" {
		return nil
	}
	return ErrPeerDispatchUnavailable
}

// SelectMeasuredCommand chooses a declared vendor command only from host
// measurements with a currently measured remaining subscription quota.  It
// intentionally does not call capability.Load until an existing matrix is
// present: Load seeds priors, and a command must never make those priors look
// like evidence merely because it was invoked.
func SelectMeasuredCommand(ctx context.Context, capText, caller string, cat *fleet.Catalog) (fleet.Tool, fleet.ToolCommand, string, Selection, error) {
	c, ok := capability.ParseCapability(capText)
	if !ok {
		return fleet.Tool{}, fleet.ToolCommand{}, "", Selection{}, fmt.Errorf("toolcmd: unknown capability %q", capText)
	}
	if _, err := os.Stat(filepath.Join(capability.Dir(), "matrix.json")); err != nil {
		if os.IsNotExist(err) {
			return fleet.Tool{}, fleet.ToolCommand{}, "", Selection{}, fmt.Errorf("toolcmd: capability %q has no measured provider matrix; record a host measurement first", c)
		}
		return fleet.Tool{}, fleet.ToolCommand{}, "", Selection{}, fmt.Errorf("toolcmd: read capability matrix: %w", err)
	}
	data, err := os.ReadFile(filepath.Join(capability.Dir(), "matrix.json"))
	if err != nil {
		return fleet.Tool{}, fleet.ToolCommand{}, "", Selection{}, fmt.Errorf("toolcmd: read capability matrix: %w", err)
	}
	// Deliberately decode the persisted evidence directly rather than calling
	// capability.Load: its normal interactive path reconciles and seeds missing
	// priors. A command selector is read-only evidence consumer.
	m := new(capability.Matrix)
	if err := json.Unmarshal(data, m); err != nil || m.Agents == nil {
		if err == nil {
			err = fmt.Errorf("empty agents")
		}
		return fleet.Tool{}, fleet.ToolCommand{}, "", Selection{}, fmt.Errorf("toolcmd: invalid capability matrix: %w", err)
	}
	if cat == nil {
		cat = fleet.New()
	}
	callerVendor := callerTool(caller)
	for _, ranked := range m.Best(c, true, capability.ByValue) {
		if ranked.Cell.Source != capability.SourceHost || ranked.Cell.Samples <= 0 {
			continue
		}
		vendor, model := capability.ToolOf(ranked.Agent), capability.ModelOf(ranked.Agent)
		if vendor == "" || vendor == callerVendor {
			continue
		}
		tool, found := cat.Tool(vendor)
		if !found {
			continue
		}
		command, found := capability.CommandFor(tool, c)
		if !found {
			continue
		}
		remaining, _, known, err := llmbudget.MeasuredHeadroom(ctx, model, ranked.Agent)
		if err != nil {
			return fleet.Tool{}, fleet.ToolCommand{}, "", Selection{}, fmt.Errorf("toolcmd: measured quota for %s: %w", ranked.Agent, err)
		}
		if !known || remaining <= 0 {
			continue
		}
		sel := Selection{Capability: string(c), Vendor: vendor, Binding: ranked.Agent, Command: command.Name, RemainingQuota: remaining, MeasuredSamples: ranked.Cell.Samples}
		return tool, command, model, sel, nil
	}
	return fleet.Tool{}, fleet.ToolCommand{}, "", Selection{}, fmt.Errorf("toolcmd: no measured non-caller provider with remaining quota for capability %q", c)
}

func callerTool(caller string) string {
	caller = strings.TrimSpace(caller)
	if caller == "" {
		caller = os.Getenv("BASHY_AGENT")
	}
	if caller == "" {
		return ""
	}
	if tool := capability.ToolOf(caller); tool != caller {
		return tool
	}
	return strings.Split(caller, "-")[0]
}

func recordSelection(sel Selection) error {
	return publishSelection(room.Event{
		Type: room.EventUsage, Actor: sel.Vendor, Target: "toolcmd:" + sel.Vendor + ":" + sel.Command,
		Topic: "capability-selection", Body: room.Brief(fmt.Sprintf("capability=%s binding=%s remaining_quota=%.4f samples=%d", sel.Capability, sel.Binding, sel.RemainingQuota, sel.MeasuredSamples), 280),
		TS: time.Now().UTC().Format(time.RFC3339),
	})
}
