# @qa/web

The QA dashboard: create a project from a URL, pick a runtime, start a run,
watch it live over WebSocket and read the report.

Stage-1 placeholder — the screens themselves are delivered by T8.

```bash
make dev-web      # or: pnpm --filter @qa/web dev
```

The API base URL comes from `NEXT_PUBLIC_API_BASE_URL`. Anything prefixed
`NEXT_PUBLIC_` is compiled into the browser bundle, so it must never hold a
secret.
