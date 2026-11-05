//go:build aix || dragonfly || freebsd || illumos || netbsd || openbsd || solaris

package cli

import (
	"os"
	"os/exec"
	"syscall"
)

func (inv Invocation) prepareChildProcess(cmd *exec.Cmd) preparedChild {
	// Foreground terminal hand-off is implemented and tested on the supported
	// Darwin and Linux targets. Preserve interactive behavior on other Unix
	// targets instead of moving a TTY reader into a background process group.
	if file, ok := inv.Stdin.(*os.File); ok {
		info, err := file.Stat()
		if err == nil && info.Mode()&os.ModeCharDevice != 0 {
			return preparedChild{restoreForeground: func() {}}
		}
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return preparedChild{restoreForeground: func() {}, processGroup: true}
}
