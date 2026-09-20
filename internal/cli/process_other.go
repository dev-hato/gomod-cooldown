//go:build !aix && !darwin && !dragonfly && !freebsd && !illumos && !linux && !netbsd && !openbsd && !solaris

package cli

import (
	"os/exec"
)

func (Invocation) prepareChildProcess(*exec.Cmd) preparedChild {
	return preparedChild{restoreForeground: func() {}}
}
