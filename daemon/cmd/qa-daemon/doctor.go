package main

import (
	"context"
	"errors"
	"io"

	"github.com/ChinnakornP/longtest/daemon/runtime"
)

func runDoctor(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("doctor", stderr)
	output := fs.String("output", "text", "output format: text or json")
	configPath := fs.String("config", "", "path to the daemon config (default: platform config directory)")
	statePath := fs.String("state", "", "path to the daemon state file (default: platform state directory)")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}

	report := runtime.Doctor(ctx, runtime.DoctorOptions{ConfigPath: *configPath, StatePath: *statePath})

	if *output == "json" {
		if err := writeJSON(stdout, report); err != nil {
			return err
		}
	} else {
		out := printer{stdout}
		for _, check := range report.Checks {
			out.printf("%s %-22s %s\n", marker(check.Status), check.Name, check.Detail)
			if check.Hint != "" {
				out.printf("  %-24s fix: %s\n", "", check.Hint)
			}
		}
	}

	// A non-zero exit is what makes doctor usable from a provisioning script.
	if !report.OK() {
		return errors.New("some checks failed")
	}
	return nil
}

func marker(status runtime.CheckStatus) string {
	switch status {
	case runtime.CheckOK:
		return "✓"
	case runtime.CheckWarn:
		return "!"
	default:
		return "✗"
	}
}
