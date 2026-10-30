package extension

import _ "embed"

// PageJS is the same selector implementation injected by the live extension.
// CDP actions evaluate it in their page context to keep selector syntax and
// open-shadow-root traversal consistent with live mode.
//
//go:embed page.js
var PageJS string
