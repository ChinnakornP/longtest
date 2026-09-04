package main

import (
	"context"
	"fmt"
	"io"

	"github.com/ChinnakornP/longtest/daemon/runtime"
)

func runPair(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("pair", stderr)
	code := fs.String("code", "", "one-time pairing code from the web app (required)")
	server := fs.String("server", "", "backend base URL, e.g. https://qa.example.com (required)")
	name := fs.String("name", "", "runtime name shown in the UI (default: this machine's hostname)")
	configPath := fs.String("config", "", "where to write the daemon config (default: platform config directory)")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	if *code == "" || *server == "" {
		fs.Usage()
		return fmt.Errorf("both --code and --server are required")
	}

	path := *configPath
	if path == "" {
		resolved, err := runtime.ConfigPath()
		if err != nil {
			return err
		}
		path = resolved
	}

	cfg, err := runtime.Pair(ctx, runtime.PairInput{
		ServerURL:   *server,
		Code:        *code,
		RuntimeName: *name,
	})
	if err != nil {
		return err
	}
	if err := runtime.SaveConfig(path, cfg); err != nil {
		return err
	}

	// The token is deliberately not printed. It is written 0600 and shown
	// exactly once by the API; echoing it here would put an organization-wide
	// credential into a terminal scrollback and a CI log.
	printer{stdout}.printf("Paired as runtime %q (%s).\nConfig written to %s (mode 0600).\n\nNext: qa-daemon start\n",
		cfg.RuntimeName, cfg.RuntimeID, path)
	return nil
}
