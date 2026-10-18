//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const signalHelperEnvironment = "GOMOD_COOLDOWN_SIGNAL_HELPER"

func TestRunCannotExecuteCommand(t *testing.T) {
	command := filepath.Join(t.TempDir(), "not-executable")

	if err := os.WriteFile(command, []byte("not executable\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	assertRunFailsWithExitCode126(t, command)
}

func TestRunExistingCommandWithMissingInterpreter(t *testing.T) {
	command := filepath.Join(t.TempDir(), "missing-interpreter")
	contents := "#!/gomod-cooldown/interpreter-that-does-not-exist\n"

	if err := os.WriteFile(command, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}

	assertRunFailsWithExitCode126(t, command)
}

func assertRunFailsWithExitCode126(t *testing.T, command string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), Invocation{Args: []string{"--", command}, Stdin: nil, Stdout: &stdout, Stderr: &stderr})
	if code != 126 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	if stdout.Len() != 0 || strings.Count(stderr.String(), "gomod-cooldown:") != 1 || !strings.Contains(stderr.String(), command) {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestRunForwardsSignalsAndShutsDownProxy(t *testing.T) {
	wrapper := buildCLIExecutable(t)
	testExecutable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		name string
		sig  syscall.Signal
		code int
	}{
		{name: "interrupt", sig: syscall.SIGINT, code: 130},
		{name: "terminate", sig: syscall.SIGTERM, code: 143},
	} {
		t.Run(tt.name, func(t *testing.T) {
			testSignalForwarding(t, wrapper, testExecutable, tt.sig, tt.code)
		})
	}
}

func testSignalForwarding(t *testing.T, wrapper, testExecutable string, sig syscall.Signal, wantCode int) {
	t.Helper()
	ready := filepath.Join(t.TempDir(), "ready")
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(
		wrapper,
		"--time-source=commit",
		"--upstream=http://127.0.0.1:1",
		"--",
		testExecutable,
		"-test.run=^TestRunSignalHelper$",
		"-test.count=1",
	)
	cmd.Env = append(os.Environ(), signalHelperEnvironment+"="+ready)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	// The helper runs in its own process group and inherits these pipes, so it can outlive a killed wrapper;
	// do not let Wait block on it forever.
	cmd.WaitDelay = 2 * time.Second

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	finished := false
	stop := func() {
		if !finished {
			finished = true
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}
	t.Cleanup(stop)

	proxyURL, err := waitForSignalHelper(ready, 30*time.Second)
	if err != nil {
		stop()
		t.Fatalf("%v; stderr=%q", err, stderr.String())
	}

	if err := cmd.Process.Signal(sig); err != nil {
		t.Fatalf("signal wrapper: %v", err)
	}
	waitErr := waitUnixTestCommand(t, cmd, 10*time.Second)
	finished = true
	var exit *exec.ExitError
	if !errors.As(waitErr, &exit) || exit.ExitCode() != wantCode {
		t.Fatalf("wait=%v exit=%v, want %d; stdout=%q stderr=%q", waitErr, exit, wantCode, stdout.String(), stderr.String())
	}

	client := &http.Client{Timeout: 500 * time.Millisecond}
	if response, requestErr := client.Get(proxyURL); requestErr == nil {
		_ = response.Body.Close()
		t.Fatalf("proxy still accepts requests after signal exit: %s", proxyURL)
	}
}

func waitUnixTestCommand(t *testing.T, cmd *exec.Cmd, timeout time.Duration) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
		t.Fatalf("command did not exit within %s", timeout)
		return nil
	}
}

func TestRunSignalHelper(t *testing.T) {
	ready := os.Getenv(signalHelperEnvironment)
	if ready == "" {
		return
	}
	if err := os.WriteFile(ready, []byte(os.Getenv("GOPROXY")), 0o600); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "write ready file: %v\n", err)
		os.Exit(90)
	}

	// Exit once the wrapper is gone, so a test that killed it does not leak us.
	parent := os.Getppid()

	for os.Getppid() == parent {
		time.Sleep(100 * time.Millisecond)
	}
}

// waitForSignalHelper does not read the command's output buffers:
// they are written concurrently until the command has been waited for.
func waitForSignalHelper(ready string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		contents, err := os.ReadFile(ready)
		if err == nil {
			proxyURL := string(contents)
			if !strings.HasPrefix(proxyURL, "http://127.0.0.1:") {
				return "", fmt.Errorf("helper GOPROXY=%q", proxyURL)
			}
			return proxyURL, nil
		}
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("read ready file: %w", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return "", fmt.Errorf("signal helper did not start within %s", timeout)
}
