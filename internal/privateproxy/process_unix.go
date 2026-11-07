//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package privateproxy

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

func prepareCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		if err != nil {
			return fmt.Errorf("kill private module subprocess group: %w", err)
		}
		return nil
	}
}
