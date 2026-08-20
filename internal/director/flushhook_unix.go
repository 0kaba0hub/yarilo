//go:build unix

package director

import (
	"os/exec"
	"syscall"
)

// isolateProcessGroup puts the hook in a process group of its own so the whole
// tree can be signalled, not just the program we spawned. A hook is usually a
// shell script, and the work it waits on is a grandchild.
func isolateProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessTree signals the hook's whole process group. A hook that called
// setsid has left that group and is out of reach here — that is the case
// cmd.WaitDelay covers, and the only case in which a descendant outlives us.
func killProcessTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
