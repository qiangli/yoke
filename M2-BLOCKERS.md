# M2 blocked: missing descriptor and atlas prerequisites

Story: #1572 / 518544db394a, Sprint #336.

This is a partial delivery, not completion of M2. No siblings were edited,
no remotes were followed, and no public exposure defaults were changed.

## Verified blockers

Three attempts to resolve the supplied coreutils dependency locally:

1. Read `../coreutils/tool/mcpdoc.go` and `argschema.go`. The former contains
   only the generic descriptor; the latter does not exist.
2. Searched `../coreutils/tool` for `ArgSchema`, `DocumentAsMCPSchema`, and
   `ApplyEffects`: no definitions or references.
3. Ran `go doc github.com/qiangli/coreutils/tool.ArgSchema`: Go reports
   `no symbol ArgSchema in package github.com/qiangli/coreutils/tool`.
   The sibling checkout's HEAD is `d648411b`.

`pkg/atlas.Entry` also has no `Core`, `Hidden`, or `Status` fields. Searching
all atlas files confirms their absence. OS, Effects, and AliasOf exist.
There is no canonical 35-command core set here from which to implement the
requested default profile. Defining that set locally would invent policy.
The sprint continuity still mentions the pending coreutils descriptor lane.
The linked story ID was confirmed through `bashy sprint show 336 --json`;
`bashy todo show 1572` cannot resolve that number in this clone's todo store.

Finally, `Policy.Check` and `policyDenial` recognize only atlas/coreutils
names. An effects override alone does not authorize synthetic registered
commands or `bashy`. Fixing those functions exceeds the explicitly allowed
`commandEffects override only` scope. The proposed patch below was verified
using a Go overlay without altering those functions in the delivery.

## Working code committed

- `registerRegistryTools` builds generic direct tools from `DocumentAsMCP`,
  uses synopsis plus the first usage line, adds optional stdin/dir/env, skips
  invalid MCP names, validates unknown names before adding any tools, and
  registers sorted, deduplicated names.
- Descriptor conversion was checked against go-sdk v1.8.0: InputSchema is
  `any`, Annotations is `*ToolAnnotations`, and Meta is embedded. Conversion
  through JSON preserves the descriptor's wire fields.
- Handlers reuse `runToolHandler`, preserving RunContext construction,
  dispatch, policy denial, and stdout/stderr/exit behavior. SDK typed helpers
  validate generic inputs. Every call retains the existing middleware.
- `commandEffects` consults a concurrent override map and returns a copy.
- In-memory tests cover echo, stdin, nonzero exits, exact rm policy denial,
  deterministic tools/list bytes, invalid names, unknown-name atomicity,
  schema validation, and defensive copying of effects.

The helper is intentionally not wired into NewServerWithOptions: the public
RegisterDirectTools API, Options hooks, profiles, ApplyEffects, ArgSchema,
RunScript, and registered-tool refresh/list_changed remain to be implemented
after the prerequisite descriptors and atlas policy are available. No
substitute ArgSchema or guessed exposure profile is shipped.

## Verification

- `go build ./... && go test ./mcp/...`: PASS.
- JSON test accounting: 20 top-level tests + 21 subtests = 41 test pass events,
  1 package passed, 0 failures.
- `go vet ./mcp/.`: PASS.
- Proposed policy patch overlay: 1 focused test passed. It verified a
  synthetic destroy command gets the exact denial and status 126, succeeds
  after granting destroy, and accepts the exec effect without a privileged
  grant (matching the unchanged policy).

## Verified out-of-scope policy patch

Apply after reviewing the synthetic-name lifecycle. Registration must prevent
collisions with meta-tools and registry names and manage removal; a global
override map must not let one server replace another server's command effects.
This patch only fixes synthetic-name recognition, not that lifecycle.

```diff
--- a/mcp/policy.go
+++ b/mcp/policy.go
@@ -49,8 +49,10 @@
 // Check rejects unknown commands and ungranted privileged effects in Atlas
 // vocabulary order, independently of the order supplied by the caller.
 func (p *Policy) Check(name string, effects []string) error {
-	if _, ok := atlas.Lookup(name); !ok && tool.Lookup(name) == nil {
-		return fmt.Errorf("unknown command: %s", name)
+	if _, registered := commandEffectOverrides.Load(name); !registered {
+		if _, ok := atlas.Lookup(name); !ok && tool.Lookup(name) == nil {
+			return fmt.Errorf("unknown command: %s", name)
+		}
 	}
 	for _, effect := range atlas.Effects() {
 		if !privileged(effect) || (p != nil && p.Allow[effect]) {
@@ -104,10 +106,12 @@
 
 func policyDenial(name string, err error) (*mcpsdk.CallToolResult, RunToolOutput) {
 	out := RunToolOutput{Stderr: err.Error(), ExitCode: 126}
-	if _, ok := atlas.Lookup(name); !ok && tool.Lookup(name) == nil {
-		// Preserve the generic runner's historical unknown-command status and stderr.
-		out.ExitCode = 2
-		out.Stderr = fmt.Sprintf("%s: not a supported command\n", name)
+	if _, registered := commandEffectOverrides.Load(name); !registered {
+		if _, ok := atlas.Lookup(name); !ok && tool.Lookup(name) == nil {
+			// Preserve the generic runner's historical unknown-command status and stderr.
+			out.ExitCode = 2
+			out.Stderr = fmt.Sprintf("%s: not a supported command\n", name)
+		}
 	}
 	return &mcpsdk.CallToolResult{
 		IsError:           true,
```

Verification used `go test -overlay .m2-overlay.json ./mcp/... -run
TestSyntheticPolicyProposedPatch -count=1`, with the overlay replacing only
policy.go and this temporary test (removed after verification):

```go
func TestSyntheticPolicyProposedPatch(t *testing.T) {
    const name = "mcp-synthetic-policy-fixture"
    commandEffectOverrides.Store(name, []string{"destroy"})
    defer commandEffectOverrides.Delete(name)
    p := &Policy{}
    err := p.Check(name, commandEffects(name))
    if err == nil || err.Error() != "denied: effect destroy requires --allow destroy" {
        t.Fatalf("denial: %v", err)
    }
    _, out := policyDenial(name, err)
    if out.ExitCode != 126 || out.Stderr != err.Error() {
        t.Fatalf("denial result: %#v", out)
    }
    p.Allow = map[string]bool{"destroy": true}
    if err := p.Check(name, commandEffects(name)); err != nil { t.Fatal(err) }
    commandEffectOverrides.Store(name, []string{"exec"})
    if err := p.Check(name, commandEffects(name)); err != nil { t.Fatal(err) }
}
```

The coreutils descriptor and canonical atlas core/visibility definitions need
their owning changes supplied; no verified patch for those missing contracts
can be derived from this checkout without inventing the public schema and
35-command policy. Resume against those dependencies, then wire and test the
remaining public API and notifications.
