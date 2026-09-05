// Package security holds the enforcement primitives that make an AI QA run
// safe to point at a website nobody vetted.
//
// The product's data flow is
//
//	untrusted website -> browser -> daemon -> AI CLI (shell access, model credentials)
//
// which means every layer here exists to answer one question: what can a
// hostile page make the run do? The package is deliberately free of policy
// decisions about *what* to test — it only owns boundaries:
//
//   - [Wrap] frames page-derived bytes so they arrive at a model as data,
//     never as instructions, and so a page cannot forge the frame.
//   - [Scrubber] removes target-app credentials from anything that leaves the
//     daemon: prompts, workspace files, logs, events, artifacts.
//   - [Workspace] confines file access to one run's directory, symlink- and
//     TOCTOU-safe, and [Sandbox] extends that confinement to child processes.
//   - [EgressPolicy] is deny-by-default: a destination has to be on a list
//     before the browser or the AI CLI may reach it.
//   - [PlanGate] validates what the model produced *before* it is executed,
//     on the assumption that the model can be hijacked despite the above.
//
// Nothing in here trusts the model. A prompt boundary reduces the odds of a
// successful injection; the plan gate is what bounds the damage when one
// succeeds anyway.
package security
