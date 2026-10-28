# S522.2 — AWS Lightsail provider adapter + budget preflight

Story `ed3ef42edf96`, sprint 522. Plan: the dhnt umbrella's
`docs/sprint-522-master-execution-plan.md` (its **Budget guard** section is
binding and is what §Budget below implements).

Nothing in this story created a real AWS resource. Every test runs against
`pkg/provider/aws/fakelightsail`, an in-process stand-in that speaks the
real wire protocol and **verifies the SigV4 signature of every request**.
The single live `create → inspect → destroy` is the manager's to run with
the vault keys, after review; §Live run below is the exact sequence.

## What shipped

| Where | What |
|---|---|
| `pkg/provider/aws/` | the `aws` built-in adapter: stdlib SigV4 (header + presigned), Lightsail JSON API, budget preflight, governance tags, user-data wrapping and S3 staging, static IP, satellite firewall |
| `pkg/provider/aws/fakelightsail/` | the signature-verifying fake (Lightsail + Cost Explorer + AWS Budgets + STS) |
| `pkg/fleet/infra_builtin.go` | the ONE registry of compiled-in providers: names, per-provider flag defaults, credential resolution, how each is opened |
| `pkg/fleet/infra_provision.go` | now provider-agnostic — no per-provider branching left |
| `pkg/fleet/baseline/infra/aws.yaml` | the embedded `builtin: true` manifest entry |
| `external/aws/provisioning.go` | `ProvisioningAllowed` — the live authorization answer Sprint 457 deferred |

`pkg/provider` is a nested **stdlib-only** module, so there is no AWS SDK
anywhere in this change. The whole signer is ~250 lines, pinned by four of
AWS's published SigV4 test-suite vectors (`get-vanilla`, `post-vanilla`,
`post-x-www-form-urlencoded`, `get-vanilla-query-order-key-case`) at all
three stages: canonical request, string to sign, `Authorization` header.

## Budget

Before any create, and again on `--dry-run` (which runs the same
preflight through `plan`, allocating nothing):

- the bundle's **monthly price** (Lightsail `GetBundles`) plus the
  account's **month-to-date spend BEFORE CREDITS** must stay within the
  monthly cap (**$50** by default, `--budget-cap`, and see the spend
  ladder below — the cap can come from the budget itself);
- the bundle price alone must stay within the per-instance ceiling
  (**$24**, `--budget-per-instance`);
- **no GPU, ever** — a GPU plan is refused by shape, before any price
  arithmetic, so a cheap one is still refused.

### Where month-to-date spend comes from (rework 1, 2026-10-28)

The first live dry-run against the real account refused everything: Cost
Explorer answered **HTTP 400 `DataUnavailableException`** because the
account is new and no cost data had been ingested yet. Cost Explorer only
reports what it has already processed, so on a fresh account it cannot
answer "what have I spent this month" at all — and a guard that fails
closed turns that into a permanent refusal.

The account's **AWS Budget** does not have that problem, so it is now the
PRIMARY source (`pkg/provider/aws/budgets.go`):

1. **Budgets `DescribeBudget`** (`https://budgets.amazonaws.com`, SigV4
   `us-east-1` / `budgets`, `X-Amz-Target:
   AWSBudgetServiceGateway.DescribeBudget`) for the budget named by
   `--budget-name` (default **`dhnt-monthly-cap`**). Its
   `CalculatedSpend.ActualSpend` is month-to-date — that budget is
   `IncludeCredit=false`, i.e. before credits — and its `BudgetLimit` is
   the **cap** when `--budget-cap` is not given, so the number lives where
   the operator set it and the two cannot drift. The `AccountId` the call
   needs comes from **STS `GetCallerIdentity`** (asked once per adapter;
   it requires no IAM permission, so it cannot be the thing that denies a
   preflight). IAM already allows `budgets:ViewBudget`.
2. **Cost Explorer `GetCostAndUsage`** at `ce.us-east-1.amazonaws.com`,
   filtered `Not`/`RECORD_TYPE=Credit`, as the fallback — when the budget
   does not exist, cannot be read, or has no `CalculatedSpend` yet (a
   budget AWS has not evaluated is an unanswered question, **not** zero
   spend).

It **fails closed**, but only on losing BOTH: one working source is
enough, and a refusal names what went wrong with each. `Budget.
AllowUnknownSpend` is the explicit operator override, and it is never the
default.

"Before credits" is the load-bearing part. Credit-covered usage still
counts against the cap, so a credit balance cannot quietly fund a fleet.
A budget whose `CostTypes` say otherwise is still used — on a fresh
account it is the only figure there is — but the verdict says so in as
many words (`credits_included`, and "may understate the bill" in the
reason), and a non-USD budget is not used at all.

The verdict records its provenance (`spend_source`, `budget_name`,
`cap_source`), so a dry-run and an incident review can both tell which
source answered.

## Tags, and why destroy checks them itself

Every created resource carries the contract's `bashy-lease` tags **and**
`dhnt:managed=true`, plus `dhnt:sprint` / `dhnt:story` / `dhnt:ttl` from
`--sprint` / `--story` / `--ttl`.

The automation IAM policy permits `DeleteInstance` only on resources
tagged `dhnt:managed=true`. The adapter checks that itself, before
calling AWS, so an untagged instance is refused with a message that says
what is wrong — rather than failing as an opaque
`AccessDeniedException` that reads like a credential problem.

## User data: byte-identical, through whichever layer it fits in

The real caller hands the adapter the master's **complete** first-boot
script (`pkg/provider/enroll`). A VM given anything else — a JSON encoding
of the bundle, a truncated script — never installs or enrolls. So the
script reaches the host byte-identical in every case, through the first of
three layers that fits:

| layer | when | what goes in `userData` |
| --- | --- | --- |
| raw | `len <= 16 KiB` | the script itself, byte for byte |
| gzip wrapper | compresses into 16 KiB | `#!/bin/sh` + base64 payload that decodes and execs it |
| **S3 staging** | neither fits | `#!/bin/sh` + a presigned GET that fetches, verifies and execs it |

`Fit` decides between the first two; the adapter takes the third when
`Fit` reports that nothing fits. Nothing is ever truncated.

What the gzip wrapper can carry: base64 costs 4/3, so the gzip output has
to fit in ~11.6 KiB. Shell text compresses 10x or better, so a 25-35 KB
script of mostly shell wraps to 2-3 KB. The binding constraint is
**incompressible** content — roughly **8 KiB** of raw high-entropy bytes
(keys, certificate chains, embedded blobs) is the ceiling, measured and
pinned by `TestWrapUserDataIncompressibleCeiling`.

### Why S3 staging exists (rework 2, 2026-10-28)

The live provision of `zoo-aws` was refused before any AWS call:

```
user-data script is 43392 bytes and still 21753 bytes after gzip+base64,
over the 16384 byte limit
```

The master's script is ~43 KB, carries incompressible material, and only
grows. Compression is not a path to 16 KiB, so the adapter does what AWS
documents for a large launch script: **stage it in S3, launch a fetch
bootstrap.**

- **The object.** `PutObject` of the ORIGINAL script bytes to
  `dhnt-userdata-<accountId>-<region>` (overridable with
  `--userdata-bucket` / `$AWS_USERDATA_BUCKET`), key
  `<instance>/<16 hex>.sh`, with an explicit `x-amz-server-side-encryption:
  AES256` header. The random half of the key means a replayed create never
  overwrites the object a still-booting instance is fetching. The bucket is
  provisioned out of band — private, public access blocked, SSE default, a
  lifecycle rule expiring objects after one day — and the IAM policy grants
  `s3:PutObject` / `GetObject` / `DeleteObject` on `bucket/*` and nothing
  else: no `ListBucket`, no `CreateBucket`. So this adapter never lists and
  never creates a bucket.
- **The bootstrap** (`StagedBootstrap`, ~1.9 KB, pinned under a 4 KB
  budget and syntax-checked with `sh -n`): fetch the presigned URL with
  `curl`, falling back to `wget`, retrying for up to 10 minutes because a
  fresh instance's network is not always up when cloud-init runs; verify
  the script's **sha256** against the digest embedded in the bootstrap;
  exec it under its own shebang. Any failure writes a reason to
  `/var/log/dhnt-userdata.log` and exits non-zero.
- **The presigned URL** (`PresignURL`, SigV4 query-string authentication,
  `X-Amz-Expires` 1800). This is the whole security argument for the path:
  the bootstrap is readable by anything that can read the instance's user
  data, and the ONLY credential-derived material in it authorizes exactly
  one GET of exactly one object and expires on its own. No access key, no
  secret, and no enrollment token is ever in the bootstrap — the
  enrollment token is in the staged script, which is what the URL fetches,
  and the digest is what makes that fetch trustworthy.
- **Remembering the key.** Without `s3:ListBucket`, destroy cannot
  discover the object, so create records it as the Lightsail tag
  `dhnt:userdata-key`. Destroy deletes that key after the instance is
  gone (while the instance exists it may still be fetching). A failed
  delete is a NOTE, not a failed destroy: the lifecycle rule collects it,
  and an object under a one-day expiry is not an ongoing cost.
- **Order.** Staging happens after the budget preflight and before
  `CreateInstances`, so a refused create writes nothing. A staged object
  whose create then fails is deliberately left alone — the instance may
  exist anyway (a lost answer reconciles by idempotency tag) and may still
  be fetching; the lifecycle rule is the collector.

The presigner is pinned by AWS's **published presigned-URL vector**
(canonical request, its hash, and the signature, all three), and the fake
S3 in `fakelightsail` verifies every presigned GET by RECOMPUTING its
signature — so a URL that merely looks plausible fails the test suite
rather than the live boot.

**The limit is 16 KiB, and it is not in the Lightsail docs.**
`CreateInstances` documents no length constraint on `userData`
(<https://docs.aws.amazon.com/lightsail/2016-11-28/api-reference/API_CreateInstances.html>),
but a Lightsail instance is an EC2 instance underneath and EC2 enforces
16384 bytes of raw user data at launch
(<https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/user-data.html>).
Over it, the instance launches and then never initializes — which is why
this is handled rather than trusted to a missing doc constraint. The real
enrollment script is ~43 KB and does not compress into the limit, i.e.
always S3-staged in practice.

## Static IP and firewall

- **Static IP.** Create allocates and attaches `<instance>-ip` so the
  satellite's sslip.io endpoint survives a stop/start (Lightsail's
  ordinary public IP does not). Inspect reports it first. Destroy releases
  it — an **unattached** Lightsail static IP is billed, so a destroy that
  left one behind would keep costing money. Lightsail static IPs cannot
  carry tags, so destroy releases only the address named after the
  instance it was attached to, never a sweep. A release that fails is
  reported as ongoing cost, not swallowed.
- **Firewall.** `PutInstancePublicPorts` REPLACES the whole public-port
  set, so the adapter sends the complete intended state: tcp 80, 443,
  6443, 10250; udp 8475-8476; and tcp 22 restricted to the
  `lightsail-connect` CIDR alias — the console's browser SSH client, so a
  satellite has **no internet-facing sshd**. Mirror of the DigitalOcean
  `dhnt-satellite` firewall.

## Other decisions worth knowing

- **A Lightsail resource is addressed by NAME**, so `Resource.ID` is the
  instance name verbatim. Nothing in the contract requires an opaque id.
- **`resize` is not in the manifest.** Lightsail has no in-place bundle
  change, so an undeclared operation is refused by `provider.Guard` before
  any API call rather than failing as a 400.
- **x86_64 only.** Lightsail bundles carry no arch field and there is no
  ARM Lightsail plan; `capacity` reports `x86_64` explicitly, which is
  what the amd64-only satellite installer needs.
- **A stopped Lightsail instance keeps billing.** The plan is charged
  monthly whether it runs or not, so `stop` reports ongoing cost naming
  the instance and its static IP. This differs from a per-hour provider
  and is the most expensive thing to get wrong.
- **One adapter is pinned to one region.** A `spec.region` that is not the
  adapter's region is an `invalid_request` naming the mismatch, because
  Lightsail's catalog is per-endpoint — silently querying the wrong
  endpoint would price the wrong bundle.
- **Cost Explorer, Budgets and STS are global**: their endpoints and
  signing regions are `us-east-1` no matter where the instance lands.
  Signing one of them for the instance's region would only surface as a
  live `SignatureDoesNotMatch`, so the fake verifies the scope per
  operation.
- **STS is the one service here that is not JSON 1.1**: it speaks the
  older AWS *query* protocol (form-encoded request, XML answer), which is
  why `sts.go` has its own tiny client. The signer is shared, unchanged —
  a canonical request does not care what the body means.

## Dry run (safe, creates nothing)

```sh
bashy secret exec AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY -- \
  bashy infra provision aws --satellite zoo-aws --dry-run
```

The provider may be named positionally (`provision aws`) or with
`--provider aws`; both are accepted. Defaults come from the provider, not
DigitalOcean: `us-west-2` / `small_3_0` / `ubuntu_24_04`. It prints the request, the size verdict, the
budget verdict and the exact provider request the create would send;
`--json` carries the numbers the verdict was computed from.

The budget verdict needs a readable spend source. On this account that is
the `dhnt-monthly-cap` budget (`--budget-name` names another); if neither
it nor Cost Explorer can be read, the dry-run FAILS rather than printing a
pass it cannot justify.

## Live run (manager, after review)

Every instance creation needs the operator's go-ahead (plan §Stories).

```sh
# 1. Authorize and price it, creating nothing.
bashy secret exec AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY -- \
  bashy infra provision aws --satellite zoo-aws \
    --region us-west-2 --size small_3_0 --image ubuntu_24_04 \
    --sprint 522 --story 2816 --dry-run --json

# 2. Create. The master issues the enrollment bundle; the adapter passes
#    its first-boot script through, waits for running, firewalls the
#    instance and attaches zoo-aws-ip.
bashy secret exec AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY -- \
  bashy infra provision aws --satellite zoo-aws \
    --region us-west-2 --size small_3_0 --image ubuntu_24_04 \
    --sprint 522 --story 2816

# 3. Inspect. Re-running provision with the same --satellite is
#    idempotent: it finds the instance by its idempotency tag and reports
#    it (with its static IP) instead of creating a second one.
bashy secret exec AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY -- \
  bashy infra provision aws --satellite zoo-aws --json
```

**`bashy infra` has no inspect/list/destroy verb for provider resources
yet** — not for aws and not for digitalocean either. The adapter
implements all three contract operations and they are exercised by the
conformance suite, but nothing mounts them on the CLI; adding those verbs
was not part of this story. Until they exist, read and tear down through
the AWS front door:

```sh
bashy secret exec AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY -- \
  bashy aws lightsail get-instance --instance-name zoo-aws --region us-west-2
bashy secret exec AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY -- \
  bashy aws lightsail delete-instance --instance-name zoo-aws --region us-west-2
bashy secret exec AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY -- \
  bashy aws lightsail release-static-ip --static-ip-name zoo-aws-ip --region us-west-2
```

Through the contract, destroy needs the A6 per-operation confirmation
bound to that resource id and that idempotency key; without it the destroy
is refused before any AWS call and the instance keeps its ongoing cost.
The adapter's destroy also releases `zoo-aws-ip` itself, which the manual
path above does not — so when tearing down by hand, **release the static
IP explicitly**.

Either way, prove zero leftovers afterwards: no instance, and no
`zoo-aws-ip` still allocated (an unattached Lightsail static IP is
billed).

## Running the tests

```sh
cd pkg/provider && go test ./aws/...      # adapter, SigV4 vectors, conformance
go test ./pkg/fleet/ -run AWS              # provider selection + dry-run
go test ./external/aws/                    # ProvisioningAllowed
```

`pkg/provider` is a separate module, so the repo-root
`go test $(go list ./... | grep -v /external/)` does **not** reach it —
run it on its own, and cross-vet it on its own too
(`cd pkg/provider && GOOS=windows go vet ./...`), since `scripts/crossvet.sh`
lists only the root module's packages.
