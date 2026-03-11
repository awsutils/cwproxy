//go:build windows

package inspector

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

var (
	kernel32Proc                 = syscall.NewLazyDLL("kernel32.dll")
	generateConsoleCtrlEventProc = kernel32Proc.NewProc("GenerateConsoleCtrlEvent")
)

func configureCommand(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
	}
}

func forwardSignal(cmd *exec.Cmd, sig os.Signal) error {
	if cmd == nil || cmd.Process == nil {
		return syscall.ESRCH
	}
	if sig == os.Kill {
		return cmd.Process.Kill()
	}

	r1, _, err := generateConsoleCtrlEventProc.Call(
		uintptr(syscall.CTRL_BREAK_EVENT),
		uintptr(uint32(cmd.Process.Pid)),
	)
	if r1 != 0 {
		return nil
	}
	if err != syscall.Errno(0) {
		return fmt.Errorf("generate console control event: %w", err)
	}
	return fmt.Errorf("generate console control event failed")
}

func killProcess(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return syscall.ESRCH
	}
	return cmd.Process.Kill()
}
