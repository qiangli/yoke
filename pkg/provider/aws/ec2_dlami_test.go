package aws_test

// DLAMI host-image resolution and the model-volume layout (S546.1
// slice 1) over fakeec2: an empty spec.image or the
// dlami-ubuntu-24.04 alias resolves the newest AWS Deep Learning
// Base OSS Nvidia Driver GPU AMI (Ubuntu 24.04) via DescribeImages
// (Owner=amazon, x86_64, available, newest CreationDate); a raw ami-
// id passes through untouched; the plan prints the resolved id plus
// name and the model dir on the worker's own gp3 volume. Zero cloud
// spend: every endpoint is an httptest server.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/provider"
	"github.com/qiangli/yoke/pkg/provider/aws"
	"github.com/qiangli/yoke/pkg/provider/aws/fakeec2"
)

// gpuPlanImage plans a worker with an explicit image: "" and the
// DLAMI alias resolve through DescribeImages, a raw ami- id passes
// through.
func gpuPlanImage(t *testing.T, a provider.Adapter, name, size, image string) aws.GPUPlan {
	t.Helper()
	s := gpuSpec(name, size)
	s.Image = image
	res, err := do(t, a, provider.Request{Op: provider.OpPlan, IdempotencyKey: "plan-" + name, Spec: s})
	if err != nil {
		t.Fatalf("plan: %v (%s)", err, res.Error)
	}
	var p aws.GPUPlan
	if err := json.Unmarshal(res.Data, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// An empty spec.image resolves the DLAMI: the default fake seed
// holds two dated candidates plus newer decoys owned by someone
// else, built for the wrong architecture, or not yet available, so
// only one answer is correct.
func TestEC2PlanResolvesEmptyImageToDLAMI(t *testing.T) {
	r := newRig(t)
	seedMoney(r.ls, 10, 400, 5, 50)
	a := openEC2(t, r, nil)
	p := gpuPlanImage(t, a, "dlami-empty", "g6.xlarge", "")
	if p.ImageID != "ami-fakedlami00000002" {
		t.Fatalf("image_id = %q, want ami-fakedlami00000002 (newest amazon-owned x86_64 available)", p.ImageID)
	}
	if p.ImageName != "Deep Learning Base OSS Nvidia Driver GPU AMI (Ubuntu 24.04) 20251029" {
		t.Fatalf("image_name = %q, want the resolved DLAMI name", p.ImageName)
	}
	if n := r.ec2.Calls("DescribeImages"); n == 0 {
		t.Fatal("DescribeImages was never called for an empty image")
	}
	if !p.Preflight.OK {
		t.Fatalf("preflight refused: %s", p.Preflight.Reason)
	}
	mustEndEC2(t, r)
}

// The dlami-ubuntu-24.04 alias resolves exactly like an empty image.
func TestEC2PlanResolvesDLAMIAlias(t *testing.T) {
	r := newRig(t)
	seedMoney(r.ls, 10, 400, 5, 50)
	a := openEC2(t, r, nil)
	p := gpuPlanImage(t, a, "dlami-alias", "g6.xlarge", aws.DLAMIImageAlias)
	if p.ImageID != "ami-fakedlami00000002" {
		t.Fatalf("image_id = %q, want ami-fakedlami00000002", p.ImageID)
	}
	if p.ImageName == "" {
		t.Fatal("plan prints no resolved image name for the alias")
	}
	mustEndEC2(t, r)
}

// Newest CreationDate wins, even when the seed lists the older
// candidate first.
func TestEC2DLAMIResolvesNewest(t *testing.T) {
	r := newRig(t)
	seedMoney(r.ls, 10, 400, 5, 50)
	r.ec2.SetImages([]fakeec2.Image{
		{ID: "ami-fakeold00000000001", Name: "Deep Learning Base OSS Nvidia Driver GPU AMI (Ubuntu 24.04) 20250901",
			OwnerAlias: "amazon", OwnerID: "000000000000",
			Arch: "x86_64", State: "available", CreationDate: "2025-09-01T00:00:00.000Z"},
		{ID: "ami-fakenew00000000001", Name: "Deep Learning Base OSS Nvidia Driver GPU AMI (Ubuntu 24.04) 20251030",
			OwnerAlias: "amazon", OwnerID: "000000000000",
			Arch: "x86_64", State: "available", CreationDate: "2025-10-30T00:00:00.000Z"},
	})
	a := openEC2(t, r, nil)
	p := gpuPlanImage(t, a, "dlami-newest", "", "")
	if p.ImageID != "ami-fakenew00000000001" {
		t.Fatalf("image_id = %q, want the newest candidate ami-fakenew00000000001", p.ImageID)
	}
	mustEndEC2(t, r)
}

// A newer image owned by someone else, one built for the wrong
// architecture, and one not yet available are all skipped, even
// though each is newer than the good candidate.
func TestEC2DLAMISkipsForeignAndWrongArchImages(t *testing.T) {
	r := newRig(t)
	seedMoney(r.ls, 10, 400, 5, 50)
	r.ec2.SetImages([]fakeec2.Image{
		{ID: "ami-fakegood0000000001", Name: "Deep Learning Base OSS Nvidia Driver GPU AMI (Ubuntu 24.04) 20251001",
			OwnerAlias: "amazon", OwnerID: "000000000000",
			Arch: "x86_64", State: "available", CreationDate: "2025-10-01T00:00:00.000Z"},
		{ID: "ami-fakeother000000001", Name: "Deep Learning Base OSS Nvidia Driver GPU AMI (Ubuntu 24.04) 20251030",
			OwnerAlias: "", OwnerID: "123456789012",
			Arch: "x86_64", State: "available", CreationDate: "2025-10-30T00:00:00.000Z"},
		{ID: "ami-fakearm00000000001", Name: "Deep Learning Base OSS Nvidia Driver GPU AMI (Ubuntu 24.04) 20251030",
			OwnerAlias: "amazon", OwnerID: "000000000000",
			Arch: "arm64", State: "available", CreationDate: "2025-10-30T00:00:00.000Z"},
		{ID: "ami-fakepend0000000001", Name: "Deep Learning Base OSS Nvidia Driver GPU AMI (Ubuntu 24.04) 20251030",
			OwnerAlias: "amazon", OwnerID: "000000000000",
			Arch: "x86_64", State: "pending", CreationDate: "2025-10-30T00:00:00.000Z"},
	})
	a := openEC2(t, r, nil)
	p := gpuPlanImage(t, a, "dlami-filter", "", "")
	if p.ImageID != "ami-fakegood0000000001" {
		t.Fatalf("image_id = %q, want the only amazon-owned x86_64 available candidate", p.ImageID)
	}
	mustEndEC2(t, r)
}

// No matching image fails closed before any launch, with a message
// that says what was searched and how to proceed.
func TestEC2DLAMINoneFoundFailsClosed(t *testing.T) {
	r := newRig(t)
	seedMoney(r.ls, 10, 400, 5, 50)
	r.ec2.SetImages(nil)
	a := openEC2(t, r, nil)
	s := gpuSpec("dlami-none", "")
	s.Image = ""
	_, err := do(t, a, provider.Request{Op: provider.OpPlan, IdempotencyKey: "dlami-none", Spec: s})
	if codeOf(err) != provider.ErrInvalidRequest {
		t.Fatalf("plan with no matching image: err = %v, want invalid_request", err)
	}
	if !strings.Contains(err.Error(), "DescribeImages") || !strings.Contains(err.Error(), "explicit AMI id") {
		t.Fatalf("refusal is not actionable (must name DescribeImages and the explicit AMI id escape hatch): %v", err)
	}
	if n := r.ec2.Calls("RunInstances"); n != 0 {
		t.Fatalf("RunInstances called %d times for an unresolvable image", n)
	}
	mustEndEC2(t, r)
}

// A raw ami- id still passes through: no DescribeImages call, no
// resolved name.
func TestEC2RawAMIPassesThrough(t *testing.T) {
	r := newRig(t)
	seedMoney(r.ls, 10, 400, 5, 50)
	a := openEC2(t, r, nil)
	p := gpuPlanImage(t, a, "raw-ami", "", fakeec2.TestAMI)
	if p.ImageID != fakeec2.TestAMI {
		t.Fatalf("image_id = %q, want the raw %q", p.ImageID, fakeec2.TestAMI)
	}
	if p.ImageName != "" {
		t.Fatalf("image_name = %q, want empty for a raw ami- passthrough", p.ImageName)
	}
	if n := r.ec2.Calls("DescribeImages"); n != 0 {
		t.Fatalf("DescribeImages called %d times for a raw ami- id", n)
	}
	mustEndEC2(t, r)
}

// A create under the alias launches the resolved AMI id.
func TestEC2CreateWithDLAMIAliasLaunchesResolvedAMI(t *testing.T) {
	r := newRig(t)
	seedMoney(r.ls, 10, 400, 5, 50)
	a := openEC2(t, r, nil)
	s := gpuSpec("dlami-create", "")
	s.Image = aws.DLAMIImageAlias
	res, err := do(t, a, provider.Request{Op: provider.OpCreate, IdempotencyKey: "dlami-create-key", Spec: s})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if res.Resource == nil || res.Resource.State != provider.StateRunning {
		t.Fatalf("create resource = %+v, want running", res.Resource)
	}
	if got := r.ec2.LastRun().Get("ImageId"); got != "ami-fakedlami00000002" {
		t.Fatalf("launched ImageId = %q, want the resolved ami-fakedlami00000002", got)
	}
	mustEndEC2(t, r)
}

// The plan carries the model-volume layout: models live in
// /var/lib/ollama on the worker's own gp3 root volume (the guard
// grants no CreateVolume/AttachVolume), sized and priced here,
// persisting across Stop/Start and deleted with the worker.
func TestEC2PlanCarriesModelVolumeLayout(t *testing.T) {
	r := newRig(t)
	seedMoney(r.ls, 10, 400, 5, 50)
	a := openEC2(t, r, nil)
	p := gpuPlan(t, a, "modellayout", "")
	if p.ModelDir != "/var/lib/ollama" {
		t.Fatalf("model_dir = %q, want /var/lib/ollama", p.ModelDir)
	}
	if p.ModelDir != aws.ModelDir {
		t.Fatalf("model_dir = %q, want the exported ModelDir", p.ModelDir)
	}
	if p.VolumeType != "gp3" || p.VolumeGB != 100 {
		t.Fatalf("volume = %dGB %s, want 100GB gp3 on the worker itself", p.VolumeGB, p.VolumeType)
	}
	if p.ThroughputMBs != 125 {
		t.Fatalf("throughput = %d MB/s, want the 125 MB/s baseline default", p.ThroughputMBs)
	}
	mustEndEC2(t, r)
}
