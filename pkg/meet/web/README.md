# Bashy Meet web

The browser client for `bashy meet`. It is a single-view Vite application built
for embedding in the Go binary.

## Develop with the built-in demo

```sh
pnpm install
pnpm dev
```

Development mode uses the local fixture transport unless the page is opened
with `?mock=0`. The production build can be demoed without a backend at
`?mock=1`:

```sh
pnpm build
pnpm preview
```

The mock room includes human, agent, and system messages, streaming agent
output, organizer-disabled controls, and a first-action `409` queued response.

## Mount behavior

All REST requests resolve against `document.baseURI`. The observer URL starts
from `new URL("observe", document.baseURI)` and only swaps the URL protocol to
`ws:` or `wss:`. Vite's `base` is `./`, keeping emitted asset references
relative for both `/` and portal path-prefix mounts.
