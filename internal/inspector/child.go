package inspector

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"

	psnet "github.com/shirou/gopsutil/v4/net"
	psprocess "github.com/shirou/gopsutil/v4/process"
)

type Child struct {
	command string
	args    []string
	cmd     *exec.Cmd

	done chan struct{}

	mu     sync.RWMutex
	result ExitStatus
	exited bool
}

type ExitStatus struct {
	Code int
	Err  error
}

func Start(command string, args []string, env []string, stdin io.Reader, stdout, stderr io.Writer) (*Child, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return nil, errors.New("target command is required")
	}

	cmd := exec.Command(command, args...)
	configureCommand(cmd)
	cmd.Env = append([]string(nil), env...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	child := &Child{
		command: command,
		args:    append([]string(nil), args...),
		cmd:     cmd,
		done:    make(chan struct{}),
	}

	go child.wait()
	return child, nil
}

func (c *Child) PID() int {
	if c == nil || c.cmd == nil || c.cmd.Process == nil {
		return 0
	}
	return c.cmd.Process.Pid
}

func (c *Child) Command() string {
	if c == nil {
		return ""
	}
	return c.command
}

func (c *Child) Args() []string {
	if c == nil {
		return nil
	}
	return append([]string(nil), c.args...)
}

func (c *Child) Done() <-chan struct{} {
	if c == nil {
		return nil
	}
	return c.done
}

func (c *Child) Result() (ExitStatus, bool) {
	if c == nil {
		return ExitStatus{}, false
	}

	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.result, c.exited
}

func (c *Child) ForwardSignal(sig os.Signal) error {
	if c == nil || c.cmd == nil || c.cmd.Process == nil {
		return errors.New("child process is not running")
	}
	if sig == nil {
		return errors.New("signal is required")
	}
	return forwardSignal(c.cmd, sig)
}

func (c *Child) Kill() error {
	if c == nil || c.cmd == nil || c.cmd.Process == nil {
		return errors.New("child process is not running")
	}
	return killProcess(c.cmd)
}

func (c *Child) ListeningPorts(ctx context.Context) ([]int, error) {
	if c == nil || c.cmd == nil || c.cmd.Process == nil {
		return nil, errors.New("child process is not running")
	}

	pids, err := processTreePIDs(ctx, int32(c.cmd.Process.Pid))
	if err != nil && len(pids) == 0 {
		return nil, err
	}

	seen := make(map[int]struct{})
	var combined error
	for _, pid := range pids {
		connections, connErr := psnet.ConnectionsPidWithContext(ctx, "tcp", pid)
		if connErr != nil {
			combined = errors.Join(combined, connErr)
			continue
		}
		for _, connection := range connections {
			if !strings.EqualFold(connection.Status, "LISTEN") {
				continue
			}
			port := int(connection.Laddr.Port)
			if port <= 0 {
				continue
			}
			seen[port] = struct{}{}
		}
	}

	ports := make([]int, 0, len(seen))
	for port := range seen {
		ports = append(ports, port)
	}
	sort.Ints(ports)

	if len(ports) == 0 {
		return nil, combined
	}
	return ports, nil
}

func (c *Child) wait() {
	err := c.cmd.Wait()
	result := ExitStatus{
		Code: 0,
		Err:  err,
	}
	if err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			result.Code = exitError.ExitCode()
		} else {
			result.Code = -1
		}
	}

	c.mu.Lock()
	c.result = result
	c.exited = true
	c.mu.Unlock()
	close(c.done)
}

const maxProcessTreeSize = 256

func processTreePIDs(ctx context.Context, rootPID int32) ([]int32, error) {
	if rootPID <= 0 {
		return nil, errors.New("root pid must be positive")
	}

	queue := []int32{rootPID}
	seen := map[int32]struct{}{}
	pids := make([]int32, 0, 4)
	var combined error

	for len(queue) > 0 {
		if len(pids) >= maxProcessTreeSize {
			break
		}
		pid := queue[0]
		queue = queue[1:]
		if _, found := seen[pid]; found {
			continue
		}
		seen[pid] = struct{}{}
		pids = append(pids, pid)

		process, err := psprocess.NewProcessWithContext(ctx, pid)
		if err != nil {
			combined = errors.Join(combined, err)
			continue
		}

		children, err := process.ChildrenWithContext(ctx)
		if err != nil {
			combined = errors.Join(combined, err)
			continue
		}
		for _, child := range children {
			if child == nil {
				continue
			}
			queue = append(queue, child.Pid)
		}
	}

	return pids, combined
}
