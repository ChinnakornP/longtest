# Pending CI workflow change: the `e2e` job

`ci.yml` here is the intended contents of `.github/workflows/ci.yml`: the
workflow currently on `main`, plus one new job. It is parked rather than
applied because the credential this agent pushes with does not carry the GitHub
`workflow` scope, and GitHub refuses a push that creates or updates a file under
`.github/workflows/` without it.

The same convention was used for the LONG-14 change, which has since been
applied and its directory removed. This one is derived from what is on `main`
today, so applying it is a copy rather than a merge.

## Applying it

From a checkout, with a credential that has the `workflow` scope:

```
cp .github/e2e-workflow-pending/ci.yml .github/workflows/ci.yml
git rm -r .github/e2e-workflow-pending
git commit -m "CI: run the browser-backed e2e job (LONG-16)"
git push
```

Branch protection already requires `ci`, and `ci` now lists `e2e` in its
`needs`, so nothing has to change in repository settings.

## What it changes

One addition. Nothing is removed and no existing job is touched.

**A new `e2e` job** that installs a real Chromium and runs `make test-e2e`:
the executor integration tests, the resilience tests, and the planner
executability benchmark.

Without it, those suites skip themselves — each checks for the Playwright
browser cache and no-ops when it is absent — so CI is green today while the
only tests that drive a browser never run. A skipped browser test and a passing
one are indistinguishable in the exit code, which is the same failure mode the
`security` job's skip check exists to catch.

The benchmark is what measures LONG-16's acceptance criterion. It takes the
golden plan in `server/internal/testcase/testdata/`, runs every case against
the fixture app in a real browser, and fails if fewer than 80% run with every
target resolved. It stands at 10/10 locally.

That number cannot be derived from the contract. A test plan can be a flawless
`test-plan@1` and still point at elements that are not on the page: the
backend's ref check makes such a plan unstorable, but only a browser can
confirm that the application map it checks refs against still describes the
application. When discovery drifts from the app, this job is what says so.

The job adds roughly two to three minutes to a run, most of it the Chromium
download on a cache miss. The cache is keyed on `daemon/executor/package.json`,
which is where the Playwright version is pinned — a browser build belongs to
the library version that drives it.
