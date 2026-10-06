# M1 blockers

The requested gate remains blocked by the server's logging advertisement.
`server.go` and `ServeStdio` are unchanged, as required by this assignment.

## Evidence and attempts

1. Ran `go build ./... && go test ./mcp/... ./pkg/atlas/...` against the
   unmodified server. Build passed; the conformance test observed
   `{"logging":{},"tools":{"listChanged":true}}`. Atlas coverage also exposed
   existing missing records for gencat, ladder, toolcmd and localedef.
2. Read go-sdk v1.8.0's `mcp/server.go`: nil ServerOptions.Capabilities defaults
   to logging enabled; an explicit empty ServerCapabilities disables it while
   retaining inferred tools. Verified the patch below using Go's `-overlay`
   facility, without editing the server. Repaired the atlas records and the
   corresponding Unix-origin count within the permitted atlas scope.
3. Re-ran the exact requested gate on the final sources. Build passed, atlas
   passed, MCP still failed solely on the logging assertion. Collected JSON
   test events for exact counts. No allowed test-only change can truthfully
   prove the unchanged server advertises no logging.

Negotiated protocol version: **2026-07-28** via
`ClientSession.InitializeResult().ProtocolVersion`. Exact tool names, repeated
list JSON, echo stdout `hi\n` and exit code 0 all pass.

Final unmodified-server results: **44 top-level tests passed, 1 failed**;
**35 subtests passed, 1 failed**; **1 package passed, 1 failed**; **0 skipped**.
Atlas: 40 top-level tests and 32 subtests passed. MCP: 4 top-level tests passed,
1 failed; 3 subtests passed, 1 failed.

With the proposed server overlay: **45 top-level tests passed, 0 failed**;
**36 subtests passed, 0 failed**; **2 packages passed, 0 failed**; **0 skipped**.
Verification command: `go test -overlay .git/m1-overlay.json -json ./mcp/... ./pkg/atlas/...`.
The overlay maps `mcp/server.go` to a copy containing only the patch below.

## Verified patch requiring server-scope authorization

```diff
--- a/mcp/server.go
+++ b/mcp/server.go
@@ -81,6 +81,7 @@
 	srv := mcpsdk.NewServer(
 		&mcpsdk.Implementation{Name: name, Version: version},
 		&mcpsdk.ServerOptions{
+			Capabilities: &mcpsdk.ServerCapabilities{},
 			Instructions: "Pure-Go AgentOS userland. Use list_tools to discover commands, run_tool to execute one. Tools follow GNU semantics for the flags they implement and fail loudly (exit 2) on unsupported flags rather than guessing.",
 		},
 	)
```

## Atlas scope mismatch

The new mcp verb uses StageCross, GroupOrch, CapJSON and EffExec. Existing
classification supplies OriginBashy and darwin/linux/windows. Inspection of
all steward occurrences found read/write-effect lists specific to steward,
and a package census entry for the real pkg/steward directory. Copying those
would give mcp incorrect effects and a nonexistent pkg/mcp census entry;
the MCP package is at mcp/, outside that census. Neither was fabricated.

There is no experimental/status/hidden field or classification mechanism in
pkg/atlas.Entry. Experimental intent is documented beside the new verb;
actual hidden/experimental command behavior must be supplied by the separate
bashy front-door implementation. Adding a new atlas status API is not implied
by an instruction to use an existing mechanism.
