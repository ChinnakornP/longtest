package main

import (
	"context"
	"io"
	"log/slog"

	"github.com/ChinnakornP/longtest/daemon/agent"
	"github.com/ChinnakornP/longtest/daemon/agent/antigravity"
	"github.com/ChinnakornP/longtest/daemon/agent/claude"
	"github.com/ChinnakornP/longtest/daemon/agent/opencode"
	"github.com/ChinnakornP/longtest/daemon/pkg/qaschema"
	"github.com/ChinnakornP/longtest/daemon/runtime"
	"github.com/ChinnakornP/longtest/daemon/security"
	"github.com/ChinnakornP/longtest/daemon/workspace"
)

func runStart(ctx context.Context, args []string, stderr io.Writer) error {
	fs := newFlagSet("start", stderr)
	configPath := fs.String("config", "", "path to the daemon config (default: platform config directory)")
	verbose := fs.Bool("verbose", false, "log at debug level")
	quiet := fs.Bool("quiet", false, "log to the file only, not to stderr")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}

	path := *configPath
	if path == "" {
		resolved, err := runtime.ConfigPath()
		if err != nil {
			return err
		}
		path = resolved
	}

	cfg, err := runtime.LoadConfig(path)
	if err != nil {
		return err
	}

	logPath, err := cfg.ResolveLogPath()
	if err != nil {
		return err
	}
	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	logger, closer, err := runtime.NewLogger(runtime.LogOptions{Path: logPath, Level: level, Console: !*quiet})
	if err != nil {
		return err
	}
	if closer != nil {
		defer func() { _ = closer.Close() }()
	}

	workspaceRoot, err := cfg.ResolveWorkspaceRoot()
	if err != nil {
		return err
	}
	workspaces, err := workspace.NewManager(workspaceRoot, cfg.Retention.Retention())
	if err != nil {
		return err
	}

	statePath, err := runtime.StatePath()
	if err != nil {
		return err
	}
	state, err := runtime.NewStateFile(statePath, runtime.State{
		RuntimeID:   cfg.RuntimeID,
		RuntimeName: cfg.RuntimeName,
		ServerURL:   cfg.ServerURL,
		Connection:  runtime.ConnectionConnecting,
	}, nil)
	if err != nil {
		return err
	}

	agentRunner, err := newAgentRunner(cfg, logger)
	if err != nil {
		return err
	}

	daemon, err := runtime.New(cfg, runtime.Deps{
		Logger:     logger,
		Workspaces: workspaces,
		State:      state,
		Agent:      agentRunner,
	})
	if err != nil {
		return err
	}

	logger.Info("starting qa-daemon",
		"version", runtime.Version, "config", cfg, "workspaceRoot", workspaceRoot, "log", logPath)
	printer{stderr}.printf("qa-daemon %s starting; logs at %s, status at %s\n", runtime.Version, logPath, statePath)

	// Run blocks until the context is cancelled and every in-flight run has
	// been torn down, so returning from here means nothing is left running.
	return daemon.Run(ctx)
}

// newAgentRunner builds the AI CLI layer this daemon offers.
//
// Every provider is registered whether or not its CLI is installed: detection
// is what decides usability, and a runtime that omitted the ones it lacks
// would report a shorter list of options each time an operator uninstalled
// something, with no way for the UI to say why.
func newAgentRunner(cfg runtime.Config, logger *slog.Logger) (runtime.AgentRunner, error) {
	registry := agent.NewRegistry(
		claude.New(claude.Options{Model: cfg.Agent.Model}),
		opencode.New(opencode.Options{}),
		antigravity.New(antigravity.Options{}),
	)

	runner, err := agent.NewRunner(agent.RunnerOptions{
		Registry:    registry,
		Default:     qaschema.AgentCapabilityName(cfg.Agent.Default),
		MaxAttempts: cfg.Agent.MaxAttempts,
		Timeout:     cfg.Agent.Timeout(),
		Logger:      logger,
		// The pairing token is this runtime's bearer credential for the
		// control plane. It is registered so that an application under test
		// which somehow echoes it back cannot get it copied into an AI CLI's
		// context window on top of wherever it already leaked.
		Secrets: []string{cfg.Token},
		Sandbox: security.Spec{
			Limits: security.DefaultAgentLimits(),
			// The CLI has to reach its own vendor's API, and there is no list
			// of addresses to allow that would survive a CDN. The confinement
			// that matters for an AI CLI is the filesystem one: it can read
			// and write the run workspace and its own credentials, and
			// nothing else on the machine.
			Network:          security.NetworkHost,
			EnvAllow:         security.BaseEnvAllow(),
			AllowUnsandboxed: cfg.Agent.AllowUnsandboxed,
		},
	})
	if err != nil {
		return nil, err
	}
	return runtime.NewAgentRunner(runner), nil
}
