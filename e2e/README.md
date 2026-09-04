# e2e

End-to-end tests of the platform itself — the web app, the backend and a
daemon talking to each other — as opposed to the tests the platform *generates*
for a customer application.

Also the home of the deliberately-buggy fixture application used to benchmark
the planner and the failure analyst (T13).

Intentionally not a pnpm workspace member yet: adding Playwright here pulls
browser binaries into every CI job. It joins the workspace when T13 lands, with
its own workflow job.
