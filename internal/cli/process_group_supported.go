//go:build darwin || linux

package cli

import (
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"unsafe"
)

// terminalFD is a file descriptor that may refer to a controlling terminal.
type terminalFD uintptr

// prepareChildProcess isolates the child and its descendants in a process
// group. For an interactive invocation, the child group owns the terminal
// while it runs so reads from stdin and terminal-generated signals keep their
// normal shell semantics.
func (inv Invocation) prepareChildProcess(cmd *exec.Cmd) preparedChild {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	file, ok := inv.Stdin.(*os.File)
	if !ok {
		return preparedChild{restoreForeground: func() {}, processGroup: true}
	}
	fd := terminalFD(file.Fd())
	wrapperGroup := syscall.Getpgrp()
	foregroundGroup, err := fd.foregroundGroup()
	if err != nil || foregroundGroup != wrapperGroup {
		return preparedChild{restoreForeground: func() {}, processGroup: true}
	}

	cmd.SysProcAttr.Foreground = true
	cmd.SysProcAttr.Ctty = int(fd)
	return preparedChild{
		restoreForeground: func() {
			_ = fd.setForegroundGroup(wrapperGroup)
		},
		processGroup: true,
	}
}

// foregroundGroup intentionally uses the standard library syscall package to
// avoid adding a runtime dependency solely for two terminal ioctls.
//
//nolint:gosec // The kernel writes an int32 to the pointer for TIOCGPGRP.
func (fd terminalFD) foregroundGroup() (int, error) {
	var group int32
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		uintptr(fd),
		uintptr(syscall.TIOCGPGRP),
		uintptr(unsafe.Pointer(&group)),
	)
	if errno != 0 {
		return 0, errno
	}
	return int(group), nil
}

//nolint:gosec // The kernel reads an int32 from the pointer for TIOCSPGRP.
func (fd terminalFD) setForegroundGroup(group int) error {
	group32 := int32(group)
	wasIgnored := signal.Ignored(syscall.SIGTTOU)
	signal.Ignore(syscall.SIGTTOU)
	if !wasIgnored {
		defer signal.Reset(syscall.SIGTTOU)
	}
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		uintptr(fd),
		uintptr(syscall.TIOCSPGRP),
		uintptr(unsafe.Pointer(&group32)),
	)
	if errno != 0 {
		return errno
	}
	return nil
}
