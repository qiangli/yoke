# Meet Web E2E

Run from `pkg/meet/web`:

```sh
npm install && npx playwright test
```

The Playwright harness runs `npm run build` to produce `web/dist`, then `go build -tags meetspa -o <tmp>/bashy-meet-e2e ./cmd/coreutils` so the SPA is embedded before it launches `bashy meet serve` with an isolated `BASHY_MEET_DIR`.
