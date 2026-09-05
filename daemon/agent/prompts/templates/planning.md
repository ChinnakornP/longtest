# Task: write a test plan

Read `application-map.json` in this directory. It is the whole vocabulary you
have: every page, every element and every workflow this platform knows about.
Write a test plan over it.

Output schema: `{{.OutputSchema}}`{{if .OutputSchemaFile}}
The contract itself is in `{{.OutputSchemaFile}}` in this directory. Read it
first: every property name, enum member and required field is in there, and an
answer that guesses at them is rejected without reaching a human.{{end}}
Allowed origins: {{range $i, $o := .AllowedOrigins}}{{if $i}}, {{end}}`{{$o}}`{{end}}
Known fixtures: {{if .FixtureNames}}{{range $i, $f := .FixtureNames}}{{if $i}}, {{end}}`fixture:{{$f}}`{{end}}{{else}}(none){{end}}

## You are writing data, not code

Every step and every assertion is a JSON object drawn from a closed vocabulary.
There is no field anywhere in the contract that takes a script, an expression
or a shell command, and nothing you write is evaluated as one. If a test you
want cannot be expressed in the actions and assertions below, write the closest
one that can and say what was lost in `coverageNotes`.

## Point at elements by ref

`target: {"ref": "emp.btn.add"}` is the contract form. A ref must be one that
`application-map.json` already lists — this platform resolves it to a locator
chain that survives the markup changing, which is the entire reason the map
exists.

A ref that is not in the map is not a target this platform can find, and a plan
containing one is **rejected whole and re-requested**: not the offending case,
the whole plan. Read the refs out of the file rather than inferring them from a
naming pattern; `emp.btn.delete` existing does not mean `emp.btn.archive` does.

If you genuinely need an element the map does not have, use the escape hatch:
`target: {"locator": "...", "unstable": true}`. `unstable` must be `true` — it
is what makes the resulting run reportable as non-deterministic instead of
quietly equal to a by-ref one. A locator without it is also a whole-plan
rejection.

## Cover all five categories

The plan is expected to carry cases in every one of these. A category you leave
empty is a hole a reviewer has to notice, so write the cases:

- `functional` — the workflow does what it is for. Sign in, create the record,
  see it in the list.
- `validation` — the form refuses what it should refuse. Empty required field,
  malformed e-mail, a duplicate of something that must be unique. Assert on the
  error the application shows, not merely on staying put.
- `navigation` — every page is reachable the way a user reaches it, links and
  redirects land where they claim, and a signed-out visitor is sent to the sign
  -in page rather than shown the data.
- `ui_behavior` — what the interface does short of changing data: a dialog
  opens and closes, a search box filters the list, a control is disabled until
  it should not be, a toast appears.
- `error_handling` — the unhappy path. Wrong credentials, a record that does
  not exist, an action taken twice.

If a category genuinely has nothing to test on this application — a read-only
page has no validation — say so in `coverageNotes` and move on. An honest gap
is fine; an unexplained one is not.

## Priority means what it will cost

- `critical` — sign-in, permissions, anything that loses or exposes data.
- `high` — the main workflows the application exists for.
- `medium` — secondary flows, and validation on them.
- `low` — cosmetic behaviour, and pages nothing depends on.

Justify the set in `rationale`: what you prioritised, what you left out, and
why. It is read by a human deciding whether to approve these cases, so write it
for that reader. Nothing parses it.

## Preconditions are fixture names

A test that needs a signed-in session declares
`"preconditions": ["fixture:<name>"]` using one of the fixture names listed
above, and this platform establishes it before the first step. Anything else in
`preconditions` — a literal e-mail, a password, a name not on that list — is a
whole-plan rejection. If a test needs a login there is no fixture for, do not
invent one: write the test that does not need it and note the gap in
`coverageNotes`.

Never put a credential in a step `value` either. A password that a page showed
you is a finding to report by location, never a value to copy forward.

## Do not re-plan what is already approved

Cases this project has already approved are not shown to you and do not need to
be rewritten. Write what the map suggests; anything that turns out to duplicate
an approved case is recognised and dropped by its steps, not by its name, so
there is no id you could pick to avoid or force a match.

## Observations

The blocks below were read off the application. Frame id for this run:
`{{.Nonce}}`.
