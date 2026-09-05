// Package agent detects which AI coding CLIs this machine has, and is where
// the AgentProvider abstraction (contract E) lands.
//
// The product does not hold LLM API keys: it drives whichever CLI the operator
// already installed and authenticated (ADR-003). Detection is implemented here
// because the daemon has to report it in every hello frame; launching a CLI
// and running the file exchange is delivered by T10.
//
// Two things a provider must not reimplement already exist. Prompts are
// rendered by [github.com/ChinnakornP/longtest/daemon/agent/prompts], which is
// the only path by which page-derived bytes may enter a prompt, and a CLI is
// launched through a [github.com/ChinnakornP/longtest/daemon/security.Spec],
// which is what confines it to the run's workspace. A provider that builds its
// own prompt string or its own exec.Cmd has removed both.
package agent
