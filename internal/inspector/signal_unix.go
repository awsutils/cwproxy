//go:build !windows

package inspector

import (
	"os"
	"os/exec"
	"syscall"
)

func configureCommand(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
}

func forwardSignal(cmd *exec.Cmd, sig os.Signal) error {
	if cmd == nil || cmd.Process == nil {
		return syscall.ESRCH
	}

	sysSig, ok := sig.(syscall.Signal)
	if !ok {
		return cmd.Process.Signal(sig)
	}

	return syscall.Kill(-cmd.Process.Pid, sysSig)
}

func killProcess(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return syscall.ESRCH
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
