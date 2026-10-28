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
| `pkg/provider/aws/` | the `aws` built-in adapter: stdlib SigV4, Lightsail JSON API, budget preflight, governance tags, user-data wrapping, static IP, satellite firewall |
| `pkg/provider/aws/fakelightsail/` | the signature-verifying fake (Lightsail + Cost Explorer) |
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
  account's **month-to-date unblended cost BEFORE CREDITS** (Cost Explorer
  `GetCostAndUsage` at `ce.us-east-1.amazonaws.com`, filtered
  `Not`/`RECORD_TYPE=Credit`) must stay within the monthly cap
  (**$50**, `--budget-cap`);
- the bundle price alone must stay within the per-instance ceiling
  (**$24**, `--budget-per-instance`);
- **no GPU, ever** — a GPU plan is refused by shape, before any price
  arithmetic, so a cheap one is still refused.

It **fails closed.** A Cost Explorer read that is denied or unreachable
leaves spend unknown, and unknown spend refuses the create: a budget guard
that passes when it cannot see the bill is not a guard. `Budget.
AllowUnknownSpend` is the explicit operator override, and it is never the
default.

"Before credits" is the load-bearing part. Credit-covered usage still
counts against the cap, so a credit balance cannot quietly fund a fleet.

## Tags, and why destroy checks them itself

Every created resource carries the contract's `bashy-lease` tags **and**
`dhnt:managed=true`, plus `dhnt:sprint` / `dhnt:story` / `dhnt:ttl` from
`--sprint` / `--story` / `--ttl`.

The automation IAM policy permits `DeleteInstance` only on resources
tagged `dhnt:managed=true`. The adapter checks that itself, before
calling AWS, so an untagged instance is refused with a message that says
what is wrong — rather than failing as an opaque
`AccessDeniedException` that reads like a credential problem.

## User data: byte-identical, or losslessly wrapped

The real caller hands the adapter the master's **complete** first-boot
script (`pkg/provider/enroll`). A VM given anything else — a JSON encoding
of the bundle, a truncated script — never installs or enrolls, so:

- a script within the limit goes through **byte-identical**; a wrapper that
  is not needed is a wrapper that can go wrong;
- a larger one is gzip+base64'd into a tiny `#!/bin/sh` bootstrap that
  decodes and execs it, and the round trip is unit-tested at 16 KiB+1,
  25 KB, 30 KB, 35 KB and 200 KB;
- a script that does not fit even compressed is an **error**, never a
  silent truncation.

What the wrapper can carry: base64 costs 4/3, so the gzip output has to
fit in ~11.6 KiB. Shell text compresses 10x or better, so a 25-35 KB
script of mostly shell wraps to 2-3 KB. The binding constraint is
**incompressible** content — roughly **8 KiB** of raw high-entropy bytes
(keys, certificate chains, embedded blobs) is the ceiling, measured and
pinned by `TestWrapUserDataIncompressibleCeiling`. The enrollment bundle's
Ed25519 key and single-use token are far below it. A caller that ever hits
it has to stage the payload out of band; that is the caller's decision,
not something this adapter truncates its way around.

**The limit is 16 KiB, and it is not in the Lightsail docs.**
`CreateInstances` documents no length constraint on `userData`
(<https://docs.aws.amazon.com/lightsail/2016-11-28/api-reference/API_CreateInstances.html>),
but a Lightsail instance is an EC2 instance underneath and EC2 enforces
16384 bytes of raw user data at launch
(<https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/user-data.html>).
Over it, the instance launches and then never initializes — which is why
this is wrapped rather than trusted to a missing doc constraint. The real
enrollment script is ~25-35 KB, i.e. always wrapped in practice.

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
- **Cost Explorer is global**: its endpoint and signing region are
  `us-east-1` no matter where the instance lands.

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
