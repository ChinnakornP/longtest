// Package agent detects which AI coding CLIs this machine has, and is where
// the AgentProvider abstraction (contract E) lands.
//
// The product does not hold LLM API keys: it drives whichever CLI the operator
// already installed and authenticated (ADR-003). Detection is implemented here
// because the daemon has to report it in every hello frame; launching a CLI
// and running the file exchange is delivered by T10.
package agent
