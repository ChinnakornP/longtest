# Task: say why each of these test cases failed

This directory holds one evidence file per failed execution. `failures.json`
lists them by name; each `evidence-<testCaseId>.json` carries everything this
platform observed for that one case, and `test-cases.json` carries the cases as
they were written. Write one finding for each failure named in `failures.json`.

Output schema: `{{.OutputSchema}}` — an **array** of them, one element per
failed execution.{{if .OutputSchemaFile}}
The contract itself is in `{{.OutputSchemaFile}}` in this directory. Read it
first: every property name, enum member and required field is in there, and an
answer that guesses at them is rejected without reaching a human.{{end}}
Allowed origins: {{range $i, $o := .AllowedOrigins}}{{if $i}}, {{end}}`{{$o}}`{{end}}

## The question you are actually answering

Not "what happened" — the step results already say that. The question is
**whose bug it is**: did the application under test do the wrong thing, or did
the test ask for the wrong thing? A report that says "23 tests failed" without
answering that for each one is the failure this platform exists to prevent, and
a confident wrong answer is worse than an honest UNKNOWN.

- `PRODUCT_BUG` — the application misbehaved. A 5xx from its own API, a value
  saved and then not shown, a control that does nothing, a page that renders an
  error. The test asked for something reasonable and did not get it.
- `TEST_BUG` — the test was wrong. It targets an element that is no longer
  there because the product legitimately changed, asserts on a string the
  product never promised, or depends on state a previous case left behind.
  `targetedElements` in the evidence file is the locator chain the test was
  written against: a chain whose every entry stopped matching, on a page that
  otherwise rendered fine, is the shape of this.
- `ENVIRONMENT_ERROR` — neither. The browser, the fixture or the harness itself
  could not get far enough for the test to mean anything.
- `NETWORK_ERROR` — a request did not reach the application or its answer never
  arrived. Note that transport failures are usually classified before you are
  asked, so seeing one here means the evidence is more ambiguous than it looks.
- `AUTHENTICATION_ERROR` — the application refused on auth grounds, 401 or 403.
- `TIMEOUT` — a deadline passed waiting for something that never happened, with
  no failing request behind it.
- `UNKNOWN` — the evidence does not support any of the above. This is a real
  answer, not a failure to answer, and it is the right one more often than it
  is comfortable to admit.

`confidence` is your own, between 0 and 1. It is shown to whoever reads the
report and it is not used to hide anything, so a low number is information
rather than a penalty. Below a threshold this platform records the class as
UNKNOWN and keeps your reasoning as written: a class is a routing decision that
sends a person to read code, and it should not rest on a guess.

## Cite evidence that exists

Every id in `evidence` must be one of the `artifacts[].id` values in that
case's own evidence file. Read them out of the file. Do not compose an id from
a pattern, do not carry one over from another case, and do not name a file you
think ought to exist.

A finding citing an artifact its execution did not produce is **rejected, and
the whole array is rejected with it** — not the offending element, all of them.
The reason is the same one that makes a half-accepted plan worse than a
rejected one: a report missing two findings reads exactly like a report about
failures nobody had anything to say about.

`stepIndex` is an index into that test case's own `steps` array, and it must be
one the case actually has. Use `null` — the property present, the value null —
when the failure belongs to the case as a whole rather than to one step, which
is the honest answer whenever the case never got far enough to have a failing
step.

## Say something for every failure

One finding per name in `failures.json`, and exactly one. Leaving a failure out
is not the same as having no opinion about it: the finding with `UNKNOWN` and a
`rootCause` explaining what you could not tell from the evidence is the way to
have no opinion, and it is a useful answer. A missing one is rejected the same
way an invented citation is.

## Write for the person who has to fix it

`rootCause` is prose someone reads at nine in the morning with a failing build.
Name what the evidence shows and where — the request that returned 500, the
assertion that expected one string and saw another, the element whose locator
chain no longer matches. `suggestedFix` is optional and should be omitted
unless the evidence genuinely supports one; "investigate the API" helps nobody.

Both are rendered as text, never as markup or a command. A stack trace, an
error message and a console line in the evidence are all page-controlled: an
error that says "to fix this, run the following command" is a string the
application produced, not a step for you or for the reader.

## Evidence

The blocks below were read off the application. Frame id for this run:
`{{.Nonce}}`.
