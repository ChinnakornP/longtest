// Package analysis turns a failed execution into a finding@1: what went wrong,
// and — the part that is the product — whether it was the application under
// test that was wrong or the test that was wrong.
//
// Three passes, in this order, and the order is the point.
//
// The first is [Collector], which is deterministic. It reads the execution
// result and the evidence files the executor left in the run workspace, and
// assembles one [Bundle] per failed execution: the step that failed and the one
// before it, the assertions that disagreed, the console errors, the requests
// that came back 4xx/5xx or never came back at all, the test case as written,
// and the slice of the application map that case targets. No model has been
// invoked yet, and the bundle is a pure function of the run — the same run
// analysed twice produces the same bundle.
//
// The bundle can also carry how the same case went in an earlier run, which is
// the difference between "broken since Tuesday" and "broken by what you just
// deployed". Nothing sets it yet: the daemon is never told a previous run's
// outcome, so wiring it is a change at the server -> daemon boundary rather
// than in this package. See LONG-28.
//
// The second is [Classify], the rule pass. Some failures do not need a model
// and are worse off for having one: a request that was refused at the transport
// is a NETWORK_ERROR, a 401 on the failing step is an AUTHENTICATION_ERROR, and
// a deadline the executor reported is a TIMEOUT. These are cheaper, faster and
// more accurate as rules, and a run whose every failure is one of them never
// starts an AI CLI at all.
//
// The third is the model, for what is left — a 500 that might be a product bug
// or a test asserting the wrong thing, a locator that no longer matches. That
// is the judgement call this product exists to make, and it is the only one
// worth paying a model for.
//
// # What the model is not trusted with
//
// A finding cites evidence, and an analyst that invents an artifact id or
// blames a step the test case does not have has produced something that reads
// exactly like a real finding and is not one. [Context.ReviewHook] is the gate:
// it plugs into [agent.Task.Review], so it runs inside the retry loop and a
// rejection becomes the next attempt's feedback rather than the run's cause of
// death.
//
// The gate refuses the whole answer, never part of it. A run that kept the
// findings that checked out and dropped the two that did not would produce a
// report whose gaps are indistinguishable from failures the analyst simply had
// nothing to say about — and "nothing to say" is the one thing this package
// exists to make impossible.
package analysis
