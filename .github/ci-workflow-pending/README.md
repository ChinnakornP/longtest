# Pending CI workflow

`ci.yml` here is the finished CI pipeline. It is parked outside
`.github/workflows/` for one reason only: the token available to the agent
that bootstrapped this repository carries the `repo` scope but not `workflow`,
and GitHub refuses any push — and any Contents API write — that creates or
changes a file under `.github/workflows/`.

Nothing about the workflow itself is unfinished. Activate it with:

```bash
git mv .github/ci-workflow-pending/ci.yml .github/workflows/ci.yml
git rm .github/ci-workflow-pending/README.md
git commit -m "Activate CI workflow"
git push
```

That push has to come from a credential with the `workflow` scope
(`gh auth refresh -s workflow`, or a fine-grained token with **Workflows:
write**). Once it lands, delete this directory.

## What the workflow runs

| Job        | Steps                                                             |
| ---------- | ----------------------------------------------------------------- |
| `lint`     | gofmt, `go vet`, golangci-lint v2.13.2, eslint, `tsc --noEmit`     |
| `test`     | `go test ./...` on both modules, vitest across the pnpm workspace  |
| `security` | gitleaks secret scan, compose validation, tracked-`.env` check     |

`lint` and `test` are exactly `make lint` and `make test`, so a green local run
predicts a green CI run.
