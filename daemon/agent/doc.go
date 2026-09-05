// Package agent runs an AI coding CLI as a file exchange in a run workspace.
//
// The product holds no LLM API key. It drives whichever CLI the operator
// already installed and authenticated, and talks to it the only way that
// survives a vendor changing its output format overnight (ADR-003): a prompt
// file goes into the phase directory, the CLI is told to write out.json next
// to it, and the daemon reads that file back. Nothing parses stdout — stdout
// is a debugging log here, not a protocol.
//
// The package is three things:
//
//	Detect      what this machine has, reported in every hello frame
//	Provider    one CLI's launch recipe (claude, opencode, antigravity, mock)
//	Runner      the phase loop: render, launch, validate, retry, record
//
// A provider owns only how its CLI is invoked. Everything that must not be
// reimplemented per-CLI lives outside it: prompts are rendered by
// [github.com/ChinnakornP/longtest/daemon/agent/prompts], the only path by
// which page-derived bytes may enter a prompt, and every child is launched
// through a [github.com/ChinnakornP/longtest/daemon/security.Spec], which is
// what confines it to the workspace. A provider that builds its own prompt
// string or its own exec.Cmd has removed both.
package agent
