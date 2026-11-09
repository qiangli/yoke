---
id: 503c076842ce
kind: bug
title: 'webconsole: TestDOMGenieReviewBar fails (Shape page card link empty)'
seq: 3067
status: todo
priority: p2
labels:
    - webconsole
    - shaping
    - test
created: 2026-11-09T16:14:20.56097Z
---

Found 2026-11-09 while changing the shape button icon. `go test ./pkg/webconsole -tags verifydom -run Genie` fails only TestDOMGenieReviewBar: genie_review_dom_test.go:209 'Shape page card link = "", want Review in place'. It fails identically with the icon change stashed (main at ff2b3c4b+), so it predates that change. Not listed in test/known-failures.txt. Expected: the Shape page candidate card links to Review in place, or the test is updated to the current UI; then the verifydom Genie set is green.
