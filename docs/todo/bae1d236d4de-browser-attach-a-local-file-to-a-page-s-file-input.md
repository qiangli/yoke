---
id: bae1d236d4de
kind: feature
title: 'browser: attach a local file to a page''s file input (live and headless), with the human-confirm guard'
seq: 3108
status: todo
labels:
    - browser
created: 2026-11-12T23:47:37.547905Z
---

Gap found 2026-11-12 filling startup applications (Typeform, Airtable, PearX): bashy browser has no file-upload action, so every deck or PDF upload is handed to the human, and an agent that advances past an upload question loses it (it happened on a Typeform: the application went in without the deck and had to be resubmitted). Ask: a 'browser upload <selector> <path>' subcommand (CDP DOM.setFileInputFiles in solo/probe; the extension's equivalent in live mode), able to target Typeform-style hidden inputs behind a drop zone. Guard: confirm with the operator once per site and file, and only allow files under the workspace or an explicit path. Until then, agents must stop at any upload field and hand it to the human.
