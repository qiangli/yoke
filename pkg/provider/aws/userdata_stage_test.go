package aws_test

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/provider"
	"github.com/qiangli/yoke/pkg/provider/aws"
	"github.com/qiangli/yoke/pkg/provider/aws/fakelightsail"
)

// S3 user-data staging (S522.2 rework 2).
//
// The live provision of zoo-aws was refused before any AWS call: the
// master's first-boot script is ~43 KB and still ~21.7 KB after
// gzip+base64, over EC2's hard 16 KiB user-data limit. Compression cannot
// fix that and the script only grows. These tests pin the standard AWS
// answer — stage the script in S3, launch a fetch bootstrap — and, just
// as importantly, pin that the in-limit paths did NOT change.

// smallScript is a first-boot script that fits raw.
func smallScript(n int) []byte {
	var b strings.Builder
	b.WriteString("#!/bin/bash\nset -euo pipefail\n")
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "printf '%%s\\n' 'step %d'\n", i)
	}
	return []byte(b.String()[:n])
}

// hugeScript is the live shape: ~43 KB with enough incompressible content
// (an embedded key, a certificate chain, a blob) that the gzip wrapper is
// still over the limit.
func hugeScript(t *testing.T) []byte {
	t.Helper()
	raw := make([]byte, 16*1024)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("#!/bin/bash\nset -euo pipefail\n# dhnt satellite first boot\n")
	enc := base64.StdEncoding.EncodeToString(raw)
	for i := 0; i < len(enc); i += 76 {
		b.WriteString(enc[i:min(i+76, len(enc))] + "\n")
	}
	for i := 0; b.Len() < 43*1024; i++ {
		fmt.Fprintf(&b, "printf '%%s\\n' 'first-boot step %d: install, verify, enroll'\n", i)
	}
	return []byte(b.String())
}

func sum256(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// bootstrapURL pulls the presigned URL out of a rendered bootstrap.
func bootstrapURL(t *testing.T, bootstrap string) string {
	t.Helper()
	for _, line := range strings.Split(bootstrap, "\n") {
		if rest, ok := strings.CutPrefix(line, "url='"); ok {
			return strings.TrimSuffix(rest, "'")
		}
	}
	t.Fatalf("no presigned URL in the bootstrap:\n%s", bootstrap)
	return ""
}

func createdUserData(t *testing.T, fake *fakelightsail.Server) string {
	t.Helper()
	last := fake.LastCreate()
	if last == nil {
		t.Fatal("no CreateInstances request reached the fake")
	}
	ud, _ := last["userData"].(string)
	return ud
}

// The whole point, end to end: an oversize script is PUT in S3 as the
// ORIGINAL bytes, the instance launches with the small fetch bootstrap
// instead, the object key is recorded as a tag, and the presigned URL in
// the bootstrap really fetches the script it promises.
func TestCreateStagesAnOversizeScriptInS3(t *testing.T) {
	fake := fakelightsail.New(t)
	// The ORDER is load-bearing: an instance created before its script
	// is in the bucket races its own first boot, and the retry window
	// would be hiding the bug rather than covering a slow network.
	var order []string
	fake.Observe(func(target string, _ map[string]any) {
		if target == "PutObject" || target == "CreateInstances" {
			order = append(order, target)
		}
	})
	a := open(t, fake, nil)
	s := hugeScript(t)
	res, err := do(t, a, provider.Request{
		Op: provider.OpCreate, IdempotencyKey: "stage-1",
		Spec: &provider.Spec{
			Name: "zoo-aws", Region: fakelightsail.TestRegion,
			Size: fakelightsail.TestBundleID, Image: fakelightsail.TestBlueprintID,
			UserData: s,
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// 1. The ORIGINAL script is in S3, byte-identical, encrypted.
	if n := fake.S3ObjectCount(); n != 1 {
		t.Fatalf("%d objects staged, want 1 (%v)", n, fake.S3Keys())
	}
	keys := fake.S3Keys()
	wantBucket := aws.UserDataBucketName(fakelightsail.TestAccountID, fakelightsail.TestRegion)
	bucket, key, _ := strings.Cut(keys[0], "/")
	if bucket != wantBucket {
		t.Fatalf("staged in bucket %q, want %q", bucket, wantBucket)
	}
	obj, ok := fake.S3Object(bucket, key)
	if !ok {
		t.Fatalf("object %q missing", keys[0])
	}
	if string(obj.Body) != string(s) {
		t.Fatalf("the staged object must be the script byte-identical (%d bytes staged, %d rendered)", len(obj.Body), len(s))
	}
	if obj.SSE != "AES256" {
		t.Fatalf("the staged object must be written with SSE AES256, got %q", obj.SSE)
	}
	if obj.ContentType != "text/x-shellscript" {
		t.Fatalf("content type = %q", obj.ContentType)
	}
	if got := fake.Scope("PutObject"); got != "s3/"+fakelightsail.TestRegion {
		t.Fatalf("PutObject signed for %q, want s3/%s", got, fakelightsail.TestRegion)
	}
	if len(order) != 2 || order[0] != "PutObject" || order[1] != "CreateInstances" {
		t.Fatalf("call order = %v, want PutObject then CreateInstances", order)
	}

	// 2. The instance launched with the bootstrap, not the script.
	ud := createdUserData(t, fake)
	if !aws.StagedUserData([]byte(ud)) {
		t.Fatalf("user data is not a fetch bootstrap:\n%s", ud)
	}
	if len(ud) > aws.UserDataLimit {
		t.Fatalf("user data is %d bytes, over the %d byte limit", len(ud), aws.UserDataLimit)
	}
	if !strings.Contains(ud, sum256(s)) {
		t.Fatal("the bootstrap must carry the script's sha256")
	}
	if strings.Contains(ud, "first-boot step 0") {
		t.Fatal("the bootstrap must not inline the script it fetches")
	}

	// 3. The key is recorded as a tag, because the IAM policy grants no
	// s3:ListBucket and a later destroy has no other way to find it.
	if res.Resource == nil {
		t.Fatal("create returned no resource")
	}
	if got := aws.UserDataKey(res.Resource.Tags); got != key {
		t.Fatalf("dhnt:userdata-key = %q, want %q (tags %v)", got, key, res.Resource.Tags)
	}

	// 4. The presigned URL in the bootstrap actually works: fetching it
	// with no credentials returns the script, and its digest is the one
	// the bootstrap would check.
	url := bootstrapURL(t, ud)
	resp, err := http.Get(url) //nolint:gosec // the URL is the fake's own
	if err != nil {
		t.Fatalf("fetching the presigned URL: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("presigned GET answered %d: %s", resp.StatusCode, body)
	}
	if string(body) != string(s) {
		t.Fatalf("the presigned URL fetched %d bytes, want the %d byte script", len(body), len(s))
	}
	if sum256(body) != sum256(s) {
		t.Fatal("the fetched script does not match the digest the bootstrap verifies")
	}
	mustEnd(t, fake)
}

// Destroy deletes the object it knows about, and reports nothing retained:
// an S3 object under a one-day lifecycle is not an ongoing cost.
func TestDestroyDeletesTheStagedScript(t *testing.T) {
	fake := fakelightsail.New(t)
	a := open(t, fake, nil)
	if _, err := do(t, a, provider.Request{
		Op: provider.OpCreate, IdempotencyKey: "stage-del",
		Spec: &provider.Spec{
			Name: "zoo-aws", Region: fakelightsail.TestRegion,
			Size: fakelightsail.TestBundleID, Image: fakelightsail.TestBlueprintID,
			UserData: hugeScript(t),
		},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if fake.S3ObjectCount() != 1 {
		t.Fatal("nothing was staged")
	}
	res, err := do(t, a, provider.Request{
		Op: provider.OpDestroy, IdempotencyKey: "stage-del", ResourceID: "zoo-aws",
		Authorization: confirm("zoo-aws", "stage-del"),
	})
	if err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if n := fake.S3ObjectCount(); n != 0 {
		t.Fatalf("%d staged object(s) left after destroy: %v", n, fake.S3Keys())
	}
	if n := fake.Calls("DeleteObject"); n != 1 {
		t.Fatalf("DeleteObject called %d times, want 1", n)
	}
	if res.Cost != nil && res.Cost.Note != "" {
		t.Fatalf("a clean destroy should report no note, got %q", res.Cost.Note)
	}
	mustEnd(t, fake)
}

// An instance whose user data went INLINE has no staged object, so destroy
// must not go looking for one: a stray DeleteObject on a key it invented
// would be a denied call at best.
func TestDestroyDoesNotTouchS3ForAnInlineInstance(t *testing.T) {
	fake := fakelightsail.New(t)
	a := open(t, fake, nil)
	seed(fake, "inline-host", "running", "inline-key", nil)
	if _, err := do(t, a, provider.Request{
		Op: provider.OpDestroy, IdempotencyKey: "inline-key", ResourceID: "inline-host",
		Authorization: confirm("inline-host", "inline-key"),
	}); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if n := fake.Calls("DeleteObject"); n != 0 {
		t.Fatalf("DeleteObject called %d times for an instance with no staged script", n)
	}
	mustEnd(t, fake)
}

// The in-limit paths are UNCHANGED: a script that fits goes through
// byte-identical, a compressible oversize one takes the gzip wrapper, and
// neither touches S3 at all.
func TestInLimitUserDataPathsAreUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name    string
		script  []byte
		wrapped bool
	}{
		{"raw", smallScript(10 * 1024), false},
		{"gzip", smallScript(43 * 1024), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := fakelightsail.New(t)
			a := open(t, fake, nil)
			if _, err := do(t, a, provider.Request{
				Op: provider.OpCreate, IdempotencyKey: "inline-" + tc.name,
				Spec: &provider.Spec{
					Name: "host-" + tc.name, Region: fakelightsail.TestRegion,
					Size: fakelightsail.TestBundleID, Image: fakelightsail.TestBlueprintID,
					UserData: tc.script,
				},
			}); err != nil {
				t.Fatalf("create: %v", err)
			}
			if n := fake.S3ObjectCount(); n != 0 {
				t.Fatalf("a script that fits must not be staged (%d objects)", n)
			}
			if n := fake.Calls("PutObject"); n != 0 {
				t.Fatalf("PutObject called %d times for a script that fits", n)
			}
			ud := []byte(createdUserData(t, fake))
			if aws.StagedUserData(ud) {
				t.Fatal("a script that fits must not be sent as a fetch bootstrap")
			}
			if aws.Wrapped(ud) != tc.wrapped {
				t.Fatalf("wrapped = %v, want %v", aws.Wrapped(ud), tc.wrapped)
			}
			back, err := aws.Unwrap(ud)
			if err != nil {
				t.Fatal(err)
			}
			if string(back) != string(tc.script) {
				t.Fatal("the script that reaches the host must be what the master rendered")
			}
			mustEnd(t, fake)
		})
	}
}

// A refused PutObject fails the create with a message that names the
// bucket and the size, and never launches an instance that would fetch
// nothing.
func TestCreateFailsClearlyWhenStagingIsDenied(t *testing.T) {
	fake := fakelightsail.New(t)
	fake.Inject("PutObject", fakelightsail.FaultAccessDenied, 1)
	a := open(t, fake, nil)
	_, err := do(t, a, provider.Request{
		Op: provider.OpCreate, IdempotencyKey: "stage-denied",
		Spec: &provider.Spec{
			Name: "zoo-aws", Region: fakelightsail.TestRegion,
			Size: fakelightsail.TestBundleID, Image: fakelightsail.TestBlueprintID,
			UserData: hugeScript(t),
		},
	})
	if err == nil {
		t.Fatal("a denied PutObject must fail the create")
	}
	if code := codeOf(err); code != provider.ErrInvalidCredentials {
		t.Fatalf("error code = %q, want %q (%v)", code, provider.ErrInvalidCredentials, err)
	}
	for _, want := range []string{"staging", "AccessDenied", "first-boot script"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("message should mention %q: %v", want, err)
		}
	}
	if fake.CreateCount() != 0 {
		t.Fatal("no instance may be created when its script could not be staged")
	}
	mustEnd(t, fake)
}

// The budget preflight runs BEFORE anything is staged: a refused create
// writes nothing to S3 either.
func TestARefusedBudgetStagesNothing(t *testing.T) {
	fake := fakelightsail.New(t)
	fake.SetMonthToDate(49.50)
	a := open(t, fake, nil)
	_, err := do(t, a, provider.Request{
		Op: provider.OpCreate, IdempotencyKey: "stage-budget",
		Spec: &provider.Spec{
			Name: "zoo-aws", Region: fakelightsail.TestRegion,
			Size: fakelightsail.TestBundleID, Image: fakelightsail.TestBlueprintID,
			UserData: hugeScript(t),
		},
	})
	if err == nil {
		t.Fatal("the budget must refuse this create")
	}
	if n := fake.Calls("PutObject"); n != 0 {
		t.Fatalf("PutObject called %d times for a create the budget refused", n)
	}
	mustEnd(t, fake)
}

// A plan STAGES NOTHING. It still reports the truth a --dry-run needs:
// that the script would be staged, in which bucket, and how big the
// inline bootstrap would be.
func TestPlanReportsStagingWithoutWritingAnything(t *testing.T) {
	fake := fakelightsail.New(t)
	a := open(t, fake, nil)
	s := hugeScript(t)
	res, err := do(t, a, provider.Request{
		Op: provider.OpPlan, IdempotencyKey: "stage-plan",
		Spec: &provider.Spec{
			Name: "zoo-aws", Region: fakelightsail.TestRegion,
			Size: fakelightsail.TestBundleID, Image: fakelightsail.TestBlueprintID,
			UserData: s,
		},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if n := fake.Calls("PutObject"); n != 0 {
		t.Fatalf("a plan must write nothing, PutObject called %d times", n)
	}
	var p struct {
		Staged     bool   `json:"user_data_staged"`
		Bucket     string `json:"user_data_bucket"`
		Bytes      int    `json:"user_data_bytes"`
		ScriptLen  int    `json:"user_data_script_bytes"`
		SHA256     string `json:"user_data_sha256"`
		Wrapped    bool   `json:"user_data_wrapped"`
		Tags       []string
		ProviderID string `json:"provider"`
	}
	if err := json.Unmarshal(res.Data, &p); err != nil {
		t.Fatal(err)
	}
	if !p.Staged {
		t.Fatal("the plan must say the script would be staged")
	}
	if want := aws.UserDataBucketName(fakelightsail.TestAccountID, fakelightsail.TestRegion); p.Bucket != want {
		t.Fatalf("plan bucket = %q, want %q", p.Bucket, want)
	}
	if p.ScriptLen != len(s) {
		t.Fatalf("plan script bytes = %d, want %d", p.ScriptLen, len(s))
	}
	if p.SHA256 != sum256(s) {
		t.Fatalf("plan sha256 = %q, want %q", p.SHA256, sum256(s))
	}
	if p.Bytes <= 0 || p.Bytes > aws.UserDataLimit {
		t.Fatalf("plan inline bytes = %d, want a bootstrap-sized figure within the limit", p.Bytes)
	}
	if p.Wrapped {
		t.Fatal("a staged script is not gzip-wrapped")
	}
	// A plan records no key: there is no object to record.
	for _, tag := range p.Tags {
		if strings.HasPrefix(tag, aws.TagUserDataKey+"=") {
			t.Fatalf("a plan must not claim a staged object: %q", tag)
		}
	}
	mustEnd(t, fake)
}

// The bucket is configurable, and an explicitly configured one needs no
// STS lookup at all: the account id is only there to NAME the default.
func TestStagingBucketOverrideSkipsTheAccountLookup(t *testing.T) {
	fake := fakelightsail.New(t)
	a := open(t, fake, func(c *aws.Config) { c.UserDataBucket = "my-own-bucket" })
	if _, err := do(t, a, provider.Request{
		Op: provider.OpCreate, IdempotencyKey: "stage-bucket",
		Spec: &provider.Spec{
			Name: "zoo-aws", Region: fakelightsail.TestRegion,
			Size: fakelightsail.TestBundleID, Image: fakelightsail.TestBlueprintID,
			UserData: hugeScript(t),
		},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	keys := fake.S3Keys()
	if len(keys) != 1 || !strings.HasPrefix(keys[0], "my-own-bucket/") {
		t.Fatalf("staged at %v, want the configured bucket", keys)
	}
	mustEnd(t, fake)
}

// One create, one object: a second create against the same idempotency
// key reconciles to the existing instance and does NOT stage a second
// script (nor overwrite the one the booting host may still be fetching).
func TestAReplayedCreateStagesNothingTwice(t *testing.T) {
	fake := fakelightsail.New(t)
	a := open(t, fake, nil)
	req := provider.Request{
		Op: provider.OpCreate, IdempotencyKey: "stage-replay",
		Spec: &provider.Spec{
			Name: "zoo-aws", Region: fakelightsail.TestRegion,
			Size: fakelightsail.TestBundleID, Image: fakelightsail.TestBlueprintID,
			UserData: hugeScript(t),
		},
	}
	if _, err := do(t, a, req); err != nil {
		t.Fatalf("create: %v", err)
	}
	first := fake.S3Keys()
	if _, err := do(t, a, req); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if fake.CreateCount() != 1 {
		t.Fatalf("%d creates, want 1", fake.CreateCount())
	}
	if got := fake.S3Keys(); len(got) != 1 || got[0] != first[0] {
		t.Fatalf("staged objects after the replay = %v, want just %v", got, first)
	}
	mustEnd(t, fake)
}
