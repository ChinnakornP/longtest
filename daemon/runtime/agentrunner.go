package runtime

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/ChinnakornP/longtest/daemon/agent"
	"github.com/ChinnakornP/longtest/daemon/agent/prompts"
	"github.com/ChinnakornP/longtest/daemon/pkg/qaschema"
	"github.com/ChinnakornP/longtest/daemon/workspace"
)

// AgentRunner adapter
//
// The daemon and the agent package do not share a task type on purpose: the
// daemon's AgentTask says what a *run* needs done, and agent.Task says how a
// *CLI* is driven. This file is the one place the two meet, which is also the
// one place a status from the agent package becomes a run error code the
// backend and the UI switch on.

// agentRunner drives phases through an [agent.Runner].
type agentRunner struct {
	runner *agent.Runner

	mu   sync.RWMutex
	emit func(runID string, ev agent.Event)
}

// NewAgentRunner adapts an agent runner for [Deps.Agent].
func NewAgentRunner(r *agent.Runner) AgentRunner { return &agentRunner{runner: r} }

// AttachEvents wires the provider's progress into the run's event stream. The
// daemon calls it during New; a runner used outside one simply narrates
// nothing.
func (a *agentRunner) AttachEvents(emit func(runID string, ev agent.Event)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.emit = emit
}

func (a *agentRunner) emitter() func(string, agent.Event) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.emit
}

// agentPhases maps a workspace phase onto the prompt template that phase uses.
// Execution is absent because no model runs there: the executor drives the
// browser from a plan that was already written and gated.
var agentPhases = map[workspace.Phase]prompts.Phase{
	workspace.PhaseDiscovery: prompts.PhaseDiscovery,
	workspace.PhasePlanning:  prompts.PhasePlanning,
	workspace.PhaseAnalysis:  prompts.PhaseAnalysis,
}

func (a *agentRunner) Run(ctx context.Context, task AgentTask) ([]byte, error) {
	phase, ok := agentPhases[task.Phase]
	if !ok {
		return nil, failure(qaschema.RunErrorCodeInternal, nil, "no AI CLI runs in the %s phase", task.Phase)
	}

	events := make(chan agent.Event, 32)
	var forwarding sync.WaitGroup
	forwarding.Add(1)
	go func() {
		defer forwarding.Done()
		emit := a.emitter()
		for ev := range events {
			if emit != nil {
				emit(task.RunID, ev)
			}
		}
	}()

	result, err := a.runner.Run(ctx, agent.Task{
		Agent:        task.Agent,
		Phase:        phase,
		WorkspaceDir: task.Dir,
		OutputSchema: task.SchemaID,
		// The analysis phase answers with an array of findings: the contract
		// describes one finding, and a list of them is not one.
		OutputAsList:   task.Phase == workspace.PhaseAnalysis,
		Inputs:         task.Inputs,
		AllowedOrigins: allowedOrigins(task.BaseURL),
		FixtureNames:   task.FixtureNames,
		Review:         task.Review,
		RunID:          task.RunID,
		BaseURL:        task.BaseURL,
		Events:         events,
	})
	close(events)
	forwarding.Wait()

	if err != nil {
		var typed *agent.Error
		if errors.As(err, &typed) {
			// The cause rather than err itself: the agent error's own message
			// is already the one being reported, and wrapping it in itself
			// prints the same sentence twice in the run log.
			return nil, failure(typed.Status.RunErrorCode(), typed.Unwrap(), "%s", typed.Message)
		}
		return nil, failure(qaschema.RunErrorCodeAgentOutputInvalid, err, "the %s agent failed", phase)
	}
	return result.Output, nil
}

// allowedOrigins is the egress allowlist restated to the model. It is derived
// from the run's own base URL rather than configured, so a run can never be
// told that more of the internet is in scope than the run is about.
func allowedOrigins(baseURL string) []string {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Host == "" {
		return nil
	}
	return []string{fmt.Sprintf("%s://%s", parsed.Scheme, parsed.Host)}
}

// emitAgentEvent narrates one agent event as a run event.
//
// The message is this daemon's own text and the data is counters and a schema
// path: nothing a page or a model wrote passes through here, which is why it
// can go straight to the backend.
func (d *Daemon) emitAgentEvent(runID string, ev agent.Event) {
	level := qaschema.RunEventPayloadLevelInfo
	if ev.Kind == agent.EventOutputInvalid {
		level = qaschema.RunEventPayloadLevelWarn
	}

	data := map[string]any{"attempt": ev.Attempt, "provider": string(ev.Provider)}
	if ev.Status != "" {
		data["status"] = string(ev.Status)
	}
	if ev.Message != "" {
		data["detail"] = ev.Message
	}

	d.emitEvent(runID, qaschema.RunEventPayload{
		Phase:   agentEventPhase(ev.Phase),
		Level:   level,
		Code:    string(ev.Kind),
		Message: agentEventMessage(ev),
		Data:    data,
	})
}

func agentEventPhase(p prompts.Phase) qaschema.RunEventPayloadPhase {
	switch p {
	case prompts.PhaseDiscovery:
		return qaschema.RunEventPayloadPhaseDiscover
	case prompts.PhasePlanning:
		return qaschema.RunEventPayloadPhasePlan
	default:
		return qaschema.RunEventPayloadPhaseAnalyze
	}
}

func agentEventMessage(ev agent.Event) string {
	switch ev.Kind {
	case agent.EventAttemptStarted:
		return fmt.Sprintf("%s: %s attempt %d", ev.Phase, ev.Provider, ev.Attempt)
	case agent.EventOutputInvalid:
		return fmt.Sprintf("%s: %s attempt %d did not match the contract", ev.Phase, ev.Provider, ev.Attempt)
	case agent.EventAttemptFinished:
		return fmt.Sprintf("%s: %s attempt %d finished (%s)", ev.Phase, ev.Provider, ev.Attempt, ev.Status)
	default:
		return fmt.Sprintf("%s: %s finished after %d attempt(s) (%s)", ev.Phase, ev.Provider, ev.Attempt, ev.Status)
	}
}
