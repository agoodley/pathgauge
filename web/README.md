# PathGauge dashboard

The single-page web dashboard served by `pathgauge server`. It is built with
React 19, TypeScript and Vite, draws charts with Apache ECharts, and uses
plain CSS (custom properties, light and dark themes). The production build
goes to `../internal/server/ui/dist`, which the Go server embeds at compile
time and serves for every non-`/api/` path (so client-side routes work).

Runtime dependencies: `react`, `react-dom`, `echarts` (and its `zrender` and
`tslib`). Routing is a ~80-line history router in `src/lib/router.tsx`.

## Build

```sh
cd web
npm ci
npm run build          # type-checks (tsc --noEmit, strict) then vite build
cd ..
CGO_ENABLED=0 go build -o bin/pathgauge ./cmd/pathgauge   # embeds the new UI
```

`make ui` runs the first part from the repository root.

## Develop

Start a server with demo data (see `scripts/dev-stack.sh`), then run the
Vite dev server, which proxies `/api` (including the `/api/live` WebSocket)
to it:

```sh
scripts/dev-stack.sh start          # https://127.0.0.1:17443, admin / admin-password-123
cd web && npm run dev               # http://localhost:5173
PATHGAUGE_URL=https://other-host:8443 npm run dev   # another server
```

## Layout

| Path | What |
|---|---|
| `src/lib/api.ts` | fetch wrapper (adds `X-PathGauge: 1` to every non-GET request) |
| `src/lib/types.ts` | JSON types mirroring the Go structs (`internal/store`, `internal/results`, `internal/engine`) |
| `src/lib/live.ts` | the `/api/live` WebSocket: reconnect with backoff, per-run event buffer |
| `src/components/Chart.tsx` | ECharts wrapper and option builders that apply the chart rules |
| `src/components/StepResult.tsx` | per-test result renderers (mirror `internal/report/html.go`) |
| `src/pages/*` | one file per page |

Chart rules: one y-axis per chart (two measures → two charts); categorical
colours in fixed order (blue, orange, aqua, yellow; at most four series);
status colours only for status and always with an icon and a label; 2 px
lines, recessive grid, a legend only for two or more series, axis tooltips,
units in axis names, rates and latencies from zero; heatmaps use a single
blue ramp.

## Smoke test (Playwright)

`tests/smoke.spec.ts` signs in, visits every page, opens a run result,
compares two runs, starts a short custom run (info, latency, tcp-down at
scale 0.3) and waits for the live page to switch to the result, checks dark
mode and the mobile menu, and fails on any browser console error. It needs a
running server with an online, idle agent (the dev stack has one).

```sh
cd web
npx playwright install chromium      # once; browsers live in ~/.cache, not in the repo
npm run smoke                        # against https://127.0.0.1:17443
PATHGAUGE_URL=http://localhost:5173 npm run smoke   # against the Vite dev server
SHOTS=/tmp/ui-shots npm run smoke    # where screenshots go (default test-results/shots)
```

Other variables: `PATHGAUGE_USER`, `PATHGAUGE_PASSWORD`, `RUN_TIMEOUT_MS`.
If the host lacks Chromium's libraries, run it in the Playwright image:

```sh
docker run --rm --network host -v "$PWD/..:/src" -w /src/web \
  mcr.microsoft.com/playwright:v1.64.1-noble npx playwright test
```
