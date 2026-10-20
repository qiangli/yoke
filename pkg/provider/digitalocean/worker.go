// Worker provisioning (S451.4): role=worker resources created independently
// from satellite provisioning. A worker never carries a satellite
// enrollment bundle — it is not routed through pkg/provider/enroll at all,
// so Spec.UserData, when the caller sets it, is the worker's own opaque
// first-boot payload, never the master-issued satellite bundle. Scope is
// narrowed to create here; bashy installation, DKS virtual-node and sphere
// enrollment belong to S451.9, which needs only the handoff CreateWorker
// returns to resume without reallocating the VM.
package digitalocean

import (
	"context"
	"slices"

	"github.com/qiangli/yoke/pkg/provider"
)

// TagRoleWorker marks a resource created for role=worker.
const TagRoleWorker = "role-worker"

// WorkerHandoff is what S451.9 needs to resume bootstrap (bashy install,
// DKS virtual-node and sphere enrollment) for the VM CreateWorker
// allocated, keyed so a retried bootstrap never reallocates it.
type WorkerHandoff struct {
	ResourceID     string
	IdempotencyKey string
}

// CreateWorker creates, or on retry adopts, a role=worker resource and
// returns its bootstrap handoff. It is idempotent on idempotencyKey like
// any other create: a response lost after the side effect, or a plain
// replay, finds the existing droplet by its idempotency tag instead of
// allocating a second one.
func CreateWorker(ctx context.Context, a provider.Adapter, idempotencyKey string, spec provider.Spec, auth *provider.Authorization) (provider.Result, WorkerHandoff, error) {
	if spec.Region == "" || spec.Size == "" || spec.Image == "" {
		r, err := provider.Failure(provider.OpCreate, provider.ErrInvalidRequest, "digitalocean: worker create needs spec.region, spec.size and spec.image")
		return r, WorkerHandoff{}, err
	}
	if !slices.Contains(spec.Tags, TagRoleWorker) {
		spec.Tags = append(slices.Clone(spec.Tags), TagRoleWorker)
	}
	res, err := a.Do(ctx, provider.Request{
		Op:             provider.OpCreate,
		IdempotencyKey: idempotencyKey,
		Spec:           &spec,
		Authorization:  auth,
	})
	if err != nil {
		return res, WorkerHandoff{}, err
	}
	return res, WorkerHandoff{ResourceID: res.Resource.ID, IdempotencyKey: idempotencyKey}, nil
}
