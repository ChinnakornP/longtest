# Pending CI workflow change

`ci.yml` here is the intended contents of `.github/workflows/ci.yml`. It is
parked because the credential the agent pushes with does not carry the GitHub
`workflow` scope, and GitHub refuses a push that creates or updates a file
under `.github/workflows/` without it.

## Applying it

From a checkout, with a credential that has the `workflow` scope:

```
cp .github/ci-workflow-pending/ci.yml .github/workflows/ci.yml
git rm -r .github/ci-workflow-pending
git commit -m "CI: add the security gate and the aggregate ci job (LONG-14)"
git push
```

## What it changes (LONG-14 / T12)

Two additions to the existing pipeline. Nothing is removed.

1. **The `security` job gains a real gate.** It already ran gitleaks, validated
   `docker-compose.yml` and rejected a committed `.env`. It now also runs the
   prompt-injection corpus and the security boundary tests
   (`daemon/security/...`, `daemon/agent/prompts/...`) with `-v`, and then
   asserts on the log that the named gates actually **passed** rather than
   skipped.

   The skip check is the point. The filesystem-confinement tests need a kernel
   with Landlock (5.13+); on a runner without one they skip, and a skip is
   indistinguishable from a pass in the exit code. "The AI CLI cannot read
   outside its workspace" is the load-bearing claim in `docs/SECURITY.md`, so a
   runner that cannot check it must fail the build, not quietly agree.

   The step also sets `pipefail`, which an Actions `run` block does not have by
   default — without it, `go test | tee` reports *tee's* exit status and a
   failing boundary test lands as a green check.

2. **A new aggregate `ci` job** depending on `[lint, test, security, test-db]`.

   **Set branch protection on `main` to require the `ci` check**, not the four
   jobs by name. Requiring them individually means a job added later is not
   covered until someone remembers to update the protection rule, and a gate
   that is not required is not a gate. `ci` depends on all of them, so a new
   job is covered the moment it joins `needs`.

   Branch protection itself is a repository setting and cannot be configured
   from a workflow file — this is the one step that has to be done by hand in
   **Settings → Branches → main → Require status checks to pass**.

Until this is applied, secret scanning still runs on every push and pull
request (it is in the `security` job already on `main`); what is missing is the
injection-corpus gate and the single required check.
