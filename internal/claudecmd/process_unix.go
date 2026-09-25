//go:build unix

package claudecmd

import (
	"os/exec"
	"syscall"
)

// SetProcessGroup puts the child in its own process group.
//
// This is what makes cancellation leave nothing behind. exec.CommandContext
// signals the process it started and nothing else, so a claude that has
// spawned children of its own would leave them running after Apex's context
// was cancelled — orphans holding quota and file descriptors that no later
// `apex` invocation knows about. With its own group, one signal reaches the
// whole tree.
func SetProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// KillProcessGroup kills the child and everything it started.
//
// The negative pid is the whole point: kill(-pgid) signals every member of
// the group. It falls back to the single process if the group is gone, which
// happens when the child exited between the check and the signal.
func KillProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err == nil {
		return nil
	}
	return cmd.Process.Kill()
}
