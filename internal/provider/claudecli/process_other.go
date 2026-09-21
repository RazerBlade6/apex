//go:build !unix

package claudecli

import "os/exec"

// configureProcessGroup is a no-op where process groups are not available.
// Apex targets macOS (DESIGN.md §13 depends on the macOS Keychain), so this
// file exists to keep `go build` honest on other platforms rather than to
// support them.
func configureProcessGroup(cmd *exec.Cmd) {}

// killProcessGroup kills only the child, which is the best this platform
// offers.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
