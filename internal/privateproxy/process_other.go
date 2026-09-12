//go:build !aix && !darwin && !dragonfly && !freebsd && !illumos && !linux && !netbsd && !openbsd && !solaris

package privateproxy

import "os/exec"

func prepareCommand(_ *exec.Cmd) {}
