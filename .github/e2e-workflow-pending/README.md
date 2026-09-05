# Pending CI workflow change: the `e2e` job

`ci.yml` here is the intended contents of `.github/workflows/ci.yml`: that file
as it stands on `main`, plus one new job. It is parked rather than applied
because the credential this agent pushes with does not carry the GitHub
`workflow` scope, and GitHub refuses a push that creates or updates a file
under `.github/workflows/` without it:

```
! [remote rejected] ... (refusing to allow a Personal Access Token to create or
  update workflow `.github/workflows/ci.yml` without `workflow` scope)
```

The same convention was used for the LONG-14 change, which has since been
applied and its directory removed.

## Base

Derived from `.github/workflows/ci.yml` at **`ce873d21c46286815097c0af013591970d87aa43`**
(`CI: actionlint gate ...`, #15).

That commit matters. The first version of this parked file was derived from
`4612b65`, and `ce873d2` merged one minute later — so a straight `cp` of it
would have deleted the actionlint gate that had just been added, silently and
without any diff a reviewer of the applying commit would read as a removal
(LONG-25). A parked file older than `main` is not a patch waiting to be
applied; it is a revert waiting to be applied.

`make lint-ci` now fails when that is true — see *The staleness gate* below —
so this is checked on every CI run, not remembered.

## Applying it

From a checkout of the commit you want to apply it to, with a credential that
has the `workflow` scope.

**1. Confirm it is still current.** This must pass before the `cp`:

```
make lint-ci
```

**2. Read what the `cp` will do.** Additions only, except the one replaced line
listed in `expected-removals.txt`:

```
diff .github/workflows/ci.yml .github/e2e-workflow-pending/ci.yml
```

Expected: the `e2e:` job block, and `needs:` on the `ci` job gaining `e2e`.
Nothing else. If you see any other `<` line, stop — the parked file is stale
and applying it would delete that line from CI.

**3. Apply:**

```
cp .github/e2e-workflow-pending/ci.yml .github/workflows/ci.yml
git rm -r .github/e2e-workflow-pending
git commit -m "CI: run the browser-backed e2e job (LONG-16)"
git push
```

Branch protection already requires `ci`, and `ci` now lists `e2e` in its
`needs`, so nothing has to change in repository settings.

## The staleness gate

`make check-parked-workflows` (run by `make lint-ci`, which the `lint` job runs
as its first step) diffs every `.github/*-pending/*.yml` against the live file
of the same name under `.github/workflows/`. The parked file must be a superset:
copying it over the live one may only add lines.

The single exception is declared in `expected-removals.txt` in this directory —
the exact lines this patch replaces, one per line, indentation included. An
entry there that the live file no longer contains is also an error, so the
declaration cannot rot into a rubber stamp.

Practical consequence: **when `main`'s `ci.yml` changes, regenerate this file**
(current `ci.yml` + the `e2e` block + the `needs` line) in the same PR. CI goes
red otherwise, which is the point.

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
