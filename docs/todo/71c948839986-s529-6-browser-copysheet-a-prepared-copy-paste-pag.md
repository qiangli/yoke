---
id: 71c948839986
kind: feature
title: 'S529.6 browser copysheet: a prepared copy/paste page for forms bashy can read but not fill'
seq: 3118
status: done
priority: p1
labels:
    - browser
created: 2026-11-16T15:28:46.758078Z
assignee: claude-opus5.5-s529
sprint: 529
sprint_id: 61593fcd-15ea-578f-b8ca-527620119de2
sprint_title: 'bashy browser extension: store distribution + cross-browser (Chrome Web Store, Edge/Windows, Safari)'
closed: 2026-11-16T15:37:31.074067Z
closed_by: claude-opus5.5-s529
---

Operator request 2026-11-16, from the Chrome Web Store submission: Chrome blocks extensions on Web Store pages, so bashy could read and prepare every field but not fill them. MVP: 'bashy browser copysheet FILE|-' reads JSON fields (title, intro, sections[{name, fields[{label, value, note}]}] or a flat fields list), writes one self-contained theme-aware HTML page with a Copy button per field (a copied field greys out to track progress), opens it in Chrome to sit beside the live page; --out PATH, --no-open, --json {path, fields, withheld}. Guard parity: any value the sensitive-input guard would refuse (pkg/sensitive.Scan: SSN/ITIN, EIN, ABA routing, Luhn card, IBAN) is never written; its field shows 'enter this yourself' and is reported as withheld. Acceptance: unit tests for both input shapes, HTML escaping, withheld values absent from the file, --no-open; help page entry.
