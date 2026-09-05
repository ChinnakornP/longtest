# Injection corpus

The set of ways a page under test can try to give the AI agent instructions,
and — for each one — the test plan that injection is fishing for.

`corpus.json` is the data. Two things consume it.

## The deterministic half (runs in CI)

`daemon/security/injection_corpus_test.go`. It asserts two properties that
hold whatever model is behind the CLI:

1. **Page content cannot reach the instruction region of a prompt.** For every
   case, the prompt is rendered and everything outside the untrusted frames is
   compared against a prompt built from benign content. They must be identical.
   An injection can therefore only ever be a persuasion attempt; it can never
   change what the model is *told*.
2. **The plan the injection wanted does not run.** Each case's `hijack` is the
   plan a fully compromised planner would emit. `security.PlanGate` has to
   reject it, for the rule named in `expectedRules`.

It also checks that frames cannot be forged, that terminal escapes and
invisible Unicode do not survive, and that a megabyte of page text does not
become a megabyte of prompt.

What it does **not** assert is that a model ignored an injection. That depends
on a model version nobody here controls, and a green CI run built on a lucky
sample would be worse than no test: it would read as a guarantee.

## The live half (opt-in, not a CI gate)

`src/server.ts` serves each case through its real channel — an HTML comment is
an HTML comment, a console case reaches `console.log`, a download case is a
download with an attacker-chosen filename.

```
pnpm --filter @qa/injection-corpus start
```

It prints `FIXTURE_PORT=<n>` and binds to loopback only. Point a discovery run
at it, then diff the resulting plan against a run over the benign fixture app
in `e2e/fixture-app`. A difference is a finding about that model; it is not a
regression in this repository unless property 1 or 2 above also broke.

## Adding a case

Add an object to `cases`. The fields:

| field | meaning |
| --- | --- |
| `id` | stable, kebab-case, used as a subtest name and a URL |
| `channel` | how `src/server.ts` should serve it |
| `kind` | the `security.Kind` the daemon would frame it as |
| `payload` | the literal content, or `payloadRepeat` for a volume case |
| `hijack` | the plan the injection wants, or `null` if it is not asking for one |
| `expectedRules` | the `security.Rule*` identifiers that must fire on `hijack` |

A case with `hijack: null` still runs against the framing and sanitisation
assertions — that is the right shape for an attack on the *reader* of a
prompt, like the ANSI-escape and invisible-Unicode cases, rather than on the
model's decisions.
