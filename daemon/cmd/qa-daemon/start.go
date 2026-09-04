package main

import (
	"context"
	"io"
	"log/slog"

	"github.com/ChinnakornP/longtest/daemon/runtime"
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

	daemon, err := runtime.New(cfg, runtime.Deps{
		Logger:     logger,
		Workspaces: workspaces,
		State:      state,
		// Agent is left nil until a provider is wired in (T10): a run that
		// needs an AI CLI then fails with agent_not_available, which is the
		// honest answer rather than a phase that silently does nothing.
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
