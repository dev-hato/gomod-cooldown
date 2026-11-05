//go:build !aix && !darwin && !dragonfly && !freebsd && !illumos && !linux && !netbsd && !openbsd && !solaris && !windows

package cli

import (
	"fmt"
	"os"
	"os/exec"
)

func terminationSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}

func (child childProcess) forwardSignal(sig os.Signal) error {
	if err := child.process.Signal(sig); err != nil {
		return fmt.Errorf("forward %s to child: %w", sig, err)
	}
	return nil
}

func (child childProcess) cancel() error {
	return child.process.Kill()
}

func childExitCode(exit *exec.ExitError) int {
	if code := exit.ExitCode(); code >= 0 {
		return code
	}
	return 1
}
