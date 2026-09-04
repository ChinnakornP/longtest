// Package report assembles the read model of a finished run: every execution
// with its steps, assertions and evidence, plus what the failure analyst
// concluded about them.
//
// It is six queries whatever the run's size. Executions join their test case,
// steps and assertions are fetched for the whole run at once, and evidence
// comes back joined to its finding — the per-execution and per-finding lookups
// those replace are the N+1 this package exists to avoid, and on a 74-case run
// they would be hundreds of round trips for one page.
package report
