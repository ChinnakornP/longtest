// Command daemon is the QA runtime agent. It runs on the operator's own
// machine, dials the backend over a single outbound WebSocket, and drives
// Chromium through the Node executor sidecar.
//
// Stage-1 placeholder: the control loop is implemented in T05.
package main

import (
	"fmt"
	"os"
)

func main() {
	if _, err := fmt.Fprintln(os.Stdout, "qa-daemon: control loop not implemented yet (T05)"); err != nil {
		os.Exit(1)
	}
}
