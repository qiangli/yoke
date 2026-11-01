---
id: 3fafd5f24bea
kind: bug
title: S557 make executable provider scaffold invocation portable
seq: 3047
status: done
priority: p1
created: 2026-11-01T17:11:57.671492Z
assignee: s557-public-tooling
sprint: 557
sprint_id: f5ccf563-f5d1-5b94-8be4-9388e51ef86d
sprint_title: 'Bashy v1.0.0 preparation: split private/public development and retire qiangli/dhnt'
closed: 2026-11-01T17:17:22.248978Z
closed_by: s557-manager
---

Native Windows conformance directly executes generated plugin.sh although ExecAdapter intentionally never falls back to a shell. Add explicit interpreter argv support to scaffold, use configured executable interpreter for shell scaffold conformance on Windows, retain exact injected failing-stop assertions. Do not change ExecAdapter direct-exec policy or skip Windows tests. Also isolate default journal HOME/USERPROFILE fixture and verify journal locking failure if still present.

Delivery candidate: executable scaffold accepts an explicit interpreter argv
prefix; generated YAML and Go manifests preserve it verbatim. Default direct
execution and ExecAdapter's no-shell-fallback policy are unchanged. Conformance
fixtures select PROVIDER_TEST_SHELL or an available sh executable. The injected
stop-as-destroy failure remains asserted. Journal tests isolate both HOME and
USERPROFILE; Unix0600 mode assertion is Unix-specific because Windows FileMode
cannot express those owner/group bits. Data/replay/tail assertions run everywhere.

Validation: focused native provider/scaffold/conformance checks pass. Dedicated
Windows workflow retains full provider-module logs for manager acceptance.
