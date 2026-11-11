//go:build darwin || linux

package privateproxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestBackendCancellationStopsDescendants(t *testing.T) {
	dir := t.TempDir()
	ready := filepath.Join(dir, "child-pid")
	helper := filepath.Join(dir, "fake-go")
	script := "#!/bin/sh\nsleep 30 &\nprintf '%s\\n' \"$!\" > \"$COOLDOWN_PRIVATE_CHILD_PID\"\nwait\n"
	if err := os.WriteFile(helper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	b := New(Config{GoExecutable: helper, Dir: dir, Env: append(os.Environ(), "COOLDOWN_PRIVATE_CHILD_PID="+ready)})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		resp, err := b.RoundTrip(httptest.NewRequestWithContext(ctx, http.MethodGet, "/example.com/mod/@latest", nil))
		if resp != nil {
			resp.Body.Close()
		}
		done <- err
	}()
	pid := privateChildPID(t, ready)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("lookup error=%v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled private module lookup did not return")
	}
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for privateChildRunning(pid) {
		select {
		case <-timer.C:
			t.Fatalf("private module subprocess descendant %d still running after cancellation", pid)
		case <-ticker.C:
		}
	}
}

func privateChildPID(t *testing.T, path string) int {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if data, err := os.ReadFile(path); err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err == nil && pid > 0 {
				return pid
			}
		}
		select {
		case <-timer.C:
			t.Fatal("private module subprocess did not start its descendant")
		case <-ticker.C:
		}
	}
}

func privateChildRunning(pid int) bool {
	if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		return false
	}
	if runtime.GOOS == "linux" {
		// A container's init may leave a killed orphan as a zombie. It has
		// stopped executing and holding pipes, even if it is not yet reaped.
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err == nil {
			_, state, _ := strings.Cut(string(data), ") ")
			return !strings.HasPrefix(state, "Z ")
		}
	}
	return true
}
