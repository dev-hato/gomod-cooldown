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
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	signalHelperEnvironment       = "GOMOD_COOLDOWN_SIGNAL_HELPER"
	signalHelperResultEnvironment = "GOMOD_COOLDOWN_SIGNAL_HELPER_RESULT"
)

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
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	result := filepath.Join(dir, "result")
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
	cmd.Env = append(os.Environ(), signalHelperEnvironment+"="+ready, signalHelperResultEnvironment+"="+result)
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

	assertHelperReceivedSignal(t, result, sig)

	client := &http.Client{Timeout: 500 * time.Millisecond}

	response, err := client.Get(proxyURL)
	if err != nil {
		return
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Fatal(err)
		}
	}()

	t.Fatalf("proxy still accepts requests after signal exit: %s", proxyURL)
}

// assertHelperReceivedSignal checks the helper's record of the signal it received.
// The wrapper exits with 128+signal on its own, so only this record proves that the signal reached the child.
func assertHelperReceivedSignal(t *testing.T, result string, sig syscall.Signal) {
	t.Helper()

	received, err := os.ReadFile(result)
	if err != nil {
		t.Fatal(err)
	}
	if string(received) != strconv.Itoa(int(sig)) {
		t.Fatalf("helper received signal %q, want %d", received, sig)
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
	// Exit once the wrapper is gone, so a test that killed it does not leak us.
	// The wrapper may already be gone before the first Getppid, in which case
	// parent is the adopting process; the lifetime limit covers that case. It
	// must exceed the readiness and signal-wait timeouts.
	parent := os.Getppid()
	deadline := time.Now().Add(2 * time.Minute)
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)

	if err := os.WriteFile(ready, []byte(os.Getenv("GOPROXY")), 0o600); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "write ready file: %v\n", err)
		os.Exit(90)
	}

	for os.Getppid() == parent && time.Now().Before(deadline) {
		select {
		case received := <-signals:
			sig, ok := received.(syscall.Signal)
			if !ok {
				t.Fatalf("received signal %v of type %T, want syscall.Signal", received, received)
			}

			result := os.Getenv(signalHelperResultEnvironment)

			if err := os.WriteFile(result, []byte(strconv.Itoa(int(sig))), 0o600); err != nil {
				t.Fatal(err)
			}

			// Die from the signal as an unhandled child would.
			signal.Reset(sig)

			if err := syscall.Kill(os.Getpid(), sig); err != nil {
				t.Fatal(err)
			}

			time.Sleep(time.Second)
			os.Exit(92)
		case <-time.After(100 * time.Millisecond):
		}
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
