// Destroy: the instance, the address and the staged script (S522.2,
// per-step reporting added in rework 3).
//
// A Lightsail satellite is three billable things, not one: the instance,
// the "<name>-ip" static IP (free while attached to a running instance,
// BILLED the moment it is not), and the first-boot script staged in S3
// when it was too large to go inline. A destroy that deletes only the
// instance leaves the address charging forever, so every step is
// attempted and — this is what rework 3 adds — every step is REPORTED,
// including the ones that were skipped. An operator cleaning up has to be
// able to see what is left rather than infer it.
package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/qiangli/yoke/pkg/provider"
)

// Destroy step names, as they appear in DestroyReport.
const (
	StepDeleteInstance       = "delete-instance"
	StepReleaseStaticIP      = "release-static-ip"
	StepDeleteStagedUserData = "delete-staged-userdata"
)

// Destroy step outcomes.
const (
	// StepDone means the step changed something and succeeded.
	StepDone = "done"
	// StepSkipped means there was nothing for the step to do.
	StepSkipped = "skipped"
	// StepFailed means the step was attempted and did not succeed. A
	// failed step does not fail the destroy (the instance is already
	// gone by then); it is reported, and anything still billing is in
	// the cost report too.
	StepFailed = "failed"
)

// DestroyStep is one step of one destroy.
type DestroyStep struct {
	Step   string `json:"step"`
	Target string `json:"target,omitempty"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// DestroyReport is the per-step account of one destroy, carried in
// Result.Data so a caller can print (and a test can assert) what actually
// happened to each billable thing.
//
// It is reported by the adapter and not reconstructed by the CLI, because
// only the adapter knows which steps applied: whether the instance held
// an address at all, and whether its first-boot script went inline or
// into a bucket. A journal REPLAY of a destroy answers from the journal
// and carries no report — the work was done by the first call.
type DestroyReport struct {
	Instance string        `json:"instance"`
	Steps    []DestroyStep `json:"steps"`
}

func (r *DestroyReport) add(step, target, status, detail string) {
	r.Steps = append(r.Steps, DestroyStep{Step: step, Target: target, Status: status, Detail: detail})
}

// data encodes the report for Result.Data. A report that cannot be
// encoded is dropped rather than failing a destroy that has happened.
func (r DestroyReport) data() json.RawMessage {
	b, err := json.Marshal(r)
	if err != nil {
		return nil
	}
	return b
}

// DecodeDestroyReport reads the per-step report out of a destroy result's
// Data. ok is false when there is none (a non-AWS provider, or a journal
// replay of a destroy this process did not perform).
func DecodeDestroyReport(data json.RawMessage) (DestroyReport, bool) {
	if len(data) == 0 {
		return DestroyReport{}, false
	}
	var r DestroyReport
	if err := json.Unmarshal(data, &r); err != nil || len(r.Steps) == 0 {
		return DestroyReport{}, false
	}
	return r, true
}

// destroy deletes an instance this owner created, after the A6 check and
// the dhnt:managed gate, then releases the instance's static IP and
// deletes its staged first-boot script.
//
// The managed gate is checked HERE rather than left to AWS: the automation
// IAM policy permits DeleteInstance only on resources tagged
// dhnt:managed=true, so without this check an untagged instance would fail
// as an opaque AccessDeniedException instead of saying what is wrong.
//
// Order matters. The static IP is read BEFORE the delete (afterwards
// Lightsail has cleared its attachment, so there is nothing left to
// identify it by) and released AFTER it, so a delete that fails has not
// already given away the address. A release that fails is reported as
// ongoing cost rather than swallowed: an unattached Lightsail static IP is
// billed.
//
// A staged first-boot script (the dhnt:userdata-key tag) is deleted after
// the instance, for the same reason in reverse: while the instance exists
// it may still be fetching the object, and once it does not, nothing
// needs it. That delete is best-effort — the bucket expires objects after
// a day — so its failure is a note, not a failed destroy.
//
// An instance that is ALREADY GONE is not an error: destroy is a cleanup
// verb and a cleanup that cannot be repeated is not much use. See
// destroyGone.
func (a *adapter) destroy(ctx context.Context, req provider.Request) (provider.Result, error) {
	in, res, err := a.owned(ctx, req)
	if err != nil {
		if normalize(err).Code == provider.ErrNotFound {
			return a.destroyGone(ctx, req)
		}
		return res, err
	}
	r := a.resource(ctx, in)
	if e := provider.AuthorizeDestroy(req, *r); e != nil {
		return provider.Result{ProtocolVersion: provider.ProtocolVersion, Op: req.Op, Error: e, Cost: provider.RefusedDestroyCost()}, e
	}
	if !Managed(r.Tags) {
		e := &provider.Error{Code: provider.ErrNotOwned, Message: fmt.Sprintf(
			"aws: instance %s is not tagged %s; the automation IAM policy permits deleting only tagged resources, so this destroy would be denied by AWS",
			in.Name, ManagedTag)}
		return provider.Result{ProtocolVersion: provider.ProtocolVersion, Op: req.Op, Error: e, Cost: provider.RefusedDestroyCost()}, e
	}

	ipName := in.Name + staticIPSuffix
	attached := false
	if in.IsStaticIP {
		if sip, serr := a.getStaticIP(ctx, ipName); serr == nil && sip.AttachedTo == in.Name {
			attached = true
		}
	}

	var out struct {
		Operations []operation `json:"operations"`
	}
	if err := a.ls.call(ctx, "DeleteInstance", map[string]any{"instanceName": in.Name}, &out); err != nil {
		return fail(req.Op, err)
	}
	if err := opsError(out.Operations); err != nil {
		return fail(req.Op, err)
	}
	r.State = provider.StateDeleted
	rep := DestroyReport{Instance: in.Name}
	rep.add(StepDeleteInstance, in.Name, StepDone, "")
	cost := &provider.CostReport{}
	var notes []string

	switch {
	case !attached:
		rep.add(StepReleaseStaticIP, ipName, StepSkipped, "the instance held no static ip")
	default:
		if rerr := a.releaseStaticIP(ctx, ipName); rerr != nil {
			note := fmt.Sprintf("the instance is deleted but its static IP %s could not be released (%v), and an unattached Lightsail static IP is billed; release it with `bashy aws lightsail release-static-ip --static-ip-name %s`",
				ipName, normalize(rerr), ipName)
			cost.Ongoing = true
			cost.Retained = retainedStaticIP(in.Name)
			notes = append(notes, note)
			rep.add(StepReleaseStaticIP, ipName, StepFailed, note)
		} else {
			rep.add(StepReleaseStaticIP, ipName, StepDone, "detached and released; the address is no longer billing")
		}
	}

	switch key := UserDataKey(r.Tags); {
	case key == "":
		rep.add(StepDeleteStagedUserData, "", StepSkipped, "the first-boot script went inline; nothing was staged")
	default:
		if note := a.deleteStagedUserData(ctx, r.Tags); note != "" {
			notes = append(notes, note)
			rep.add(StepDeleteStagedUserData, key, StepFailed, note)
		} else {
			rep.add(StepDeleteStagedUserData, key, StepDone, "")
		}
	}

	cost.Note = strings.Join(notes, "; ")
	return ok(req.Op, &provider.Result{Resource: r, Cost: cost, Data: rep.data()})
}

// destroyGone is the destroy of an instance that is not there: a repeat
// of a destroy that already ran, a create that failed after reserving an
// address, or an instance an operator deleted in the console.
//
// It reports success, because the instance is in the state the caller
// asked for — but it does NOT stop there. The address is the whole reason
// this path exists: "<name>-ip" outlives the instance it was attached to,
// an unattached Lightsail static IP is billed, and nothing else in the
// system will come back for it. So the reservation is released here when
// it is free to release, and left alone when something else holds it.
//
// The A6 confirmation is still required: releasing an address is a
// mutation, and an unauthorized destroy must not perform one. Only the
// STATIC half of the check can run (the resource whose tags the temporary
// half would read is gone), which is also why there is no dhnt:managed
// gate here — a Lightsail static IP cannot carry tags at all, so the IAM
// policy does not key the release on one.
func (a *adapter) destroyGone(ctx context.Context, req provider.Request) (provider.Result, error) {
	if e := provider.CheckDestroyAuthorization(req); e != nil {
		return provider.Result{ProtocolVersion: provider.ProtocolVersion, Op: req.Op, Error: e, Cost: provider.RefusedDestroyCost()}, e
	}
	name := req.ResourceID
	ipName := name + staticIPSuffix
	rep := DestroyReport{Instance: name}
	rep.add(StepDeleteInstance, name, StepSkipped, "the instance is already gone")
	cost := &provider.CostReport{}

	sip, serr := a.getStaticIP(ctx, ipName)
	switch {
	case serr != nil && normalize(serr).Code == provider.ErrNotFound:
		rep.add(StepReleaseStaticIP, ipName, StepSkipped, "there is no such static ip")
	case serr != nil:
		note := fmt.Sprintf("static ip %s could not be read (%v); if it exists and is unattached it is billing", ipName, normalize(serr))
		cost.Ongoing, cost.Retained, cost.Note = true, retainedStaticIP(name), note
		rep.add(StepReleaseStaticIP, ipName, StepFailed, note)
	case sip.AttachedTo != "" && sip.AttachedTo != name:
		rep.add(StepReleaseStaticIP, ipName, StepSkipped, fmt.Sprintf(
			"the address is attached to %s, not to %s; leaving it alone", sip.AttachedTo, name))
	default:
		if rerr := a.releaseStaticIP(ctx, ipName); rerr != nil {
			note := fmt.Sprintf("the orphaned static IP %s could not be released (%v), and an unattached Lightsail static IP is billed; release it with `bashy aws lightsail release-static-ip --static-ip-name %s`",
				ipName, normalize(rerr), ipName)
			cost.Ongoing, cost.Retained, cost.Note = true, retainedStaticIP(name), note
			rep.add(StepReleaseStaticIP, ipName, StepFailed, note)
		} else {
			rep.add(StepReleaseStaticIP, ipName, StepDone,
				"released the address the gone instance left behind; it is no longer billing")
		}
	}
	// The tag that recorded the staged object key went with the
	// instance, and the IAM policy grants no s3:ListBucket, so there is
	// nothing to look the key up by here. The bucket's one-day
	// lifecycle rule is the backstop.
	rep.add(StepDeleteStagedUserData, "", StepSkipped,
		"no staged-script key is recoverable without the instance's tags; the bucket's one-day lifecycle rule expires it")

	return ok(req.Op, &provider.Result{
		Resource: &provider.Resource{
			ID: name, Name: name, State: provider.StateDeleted, Region: a.cfg.Region,
		},
		Cost: cost,
		Data: rep.data(),
	})
}
