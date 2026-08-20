//go:build !unix

package director

import "os/exec"

// isolateProcessGroup is a no-op where process groups are not available; the
// kill below then reaches the hook itself and WaitDelay bounds the rest.
func isolateProcessGroup(*exec.Cmd) {}

func killProcessTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
