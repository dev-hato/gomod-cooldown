// Package cli owns command-line parsing and the lifecycle of the temporary proxy.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"github.com/dev-hato/gomod-cooldown/internal/availability"
	"github.com/dev-hato/gomod-cooldown/internal/goindex"
	"github.com/dev-hato/gomod-cooldown/internal/proxy"
)

const timeSourceCommit = "commit"

type action uint8

const (
	actionRun action = iota
	actionHelp
	actionVersion
)

// Invocation is one CLI invocation: its arguments and its standard streams.
type Invocation struct {
	Args   []string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// upstreamDeps are the shared HTTP client and clock used for upstream access.
type upstreamDeps struct {
	client *http.Client
	clock  func() time.Time
}

// childCommand is the argv to run and the proxy URL to run it against.
type childCommand struct {
	command  []string
	proxyURL string
}

// childProcess is a started child, plus whether it owns its own process group.
type childProcess struct {
	process *os.Process
	group   bool
}

// signalForwarding are the channels driving signal forwarding to a child.
type signalForwarding struct {
	signals <-chan os.Signal
	done    <-chan struct{}
}

// preparedChild is the result of preparing a child command for execution.
type preparedChild struct {
	restoreForeground func()
	processGroup      bool
}

// environ is a process environment in os.Environ form.
type environ []string

// Options contains the parsed CLI configuration and child command.
type Options struct {
	Cooldown        time.Duration
	Upstream        string
	TimeSource      string
	UpstreamTimeout time.Duration
	Verbose         bool
	Command         []string
	action          action
}

// Parse parses command-line arguments without modifying the process environment.
func Parse(args []string) (Options, error) {
	sep := slices.Index(args, "--")
	flagArgs := args
	if sep >= 0 {
		flagArgs = args[:sep]
	}
	values := newFlags()
	err := values.fs.Parse(flagArgs)
	if err != nil {
		return Options{}, fmt.Errorf("parse flags: %w", err)
	}
	if values.fs.NArg() != 0 {
		return Options{}, fmt.Errorf("unexpected argument %q before --", values.fs.Arg(0))
	}
	if values.help {
		return Options{action: actionHelp}, nil
	}
	if values.version {
		return Options{action: actionVersion}, nil
	}

	// This check must stay after the help/version early returns above:
	// --help and --version are valid without a -- separator, so checking this first would reject them incorrectly.
	if sep < 0 || sep+1 >= len(args) {
		return Options{}, errors.New("a command after -- is required")
	}

	command := args[sep+1:]

	if command[0] == "" {
		return Options{}, errors.New("command must not be empty")
	}

	return Options{
		Cooldown: values.cooldown, Upstream: values.upstream, TimeSource: values.timeSource,
		UpstreamTimeout: values.timeout, Verbose: values.verbose, Command: command,
	}, nil
}

// flagValues holds the flag.FlagSet and the values its flags write into.
type flagValues struct {
	fs         *flag.FlagSet
	cooldown   time.Duration
	upstream   string
	timeSource string
	timeout    time.Duration
	verbose    bool
	help       bool
	version    bool
}

// cooldownValue adapts ParseCooldown to the flag.Value interface,
// so invalid --cooldown values are rejected during fs.Parse itself.
type cooldownValue time.Duration

func (c *cooldownValue) String() string {
	if c == nil {
		return ""
	}

	return time.Duration(*c).String()
}

func (c *cooldownValue) Set(s string) error {
	d, err := ParseCooldown(s)
	if err != nil {
		return err
	}

	*c = cooldownValue(d)
	return nil
}

// upstreamTimeoutValue adapts a positive-duration check to the flag.Value
// interface, so an invalid --upstream-timeout is rejected during fs.Parse itself.
type upstreamTimeoutValue time.Duration

func (t *upstreamTimeoutValue) String() string {
	if t == nil {
		return ""
	}

	return time.Duration(*t).String()
}

func (t *upstreamTimeoutValue) Set(s string) error {
	d, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid upstream-timeout %q: %w", s, err)
	}

	if d <= 0 {
		return errors.New("upstream-timeout must be positive")
	}

	*t = upstreamTimeoutValue(d)
	return nil
}

// timeSourceValue adapts the commit/combined enum check to the flag.Value
// interface, so an unsupported --time-source is rejected during fs.Parse itself.
type timeSourceValue string

func (t *timeSourceValue) String() string {
	if t == nil {
		return ""
	}

	return string(*t)
}

func (t *timeSourceValue) Set(s string) error {
	if s != "combined" && s != timeSourceCommit {
		return fmt.Errorf("unsupported time-source %q", s)
	}

	*t = timeSourceValue(s)
	return nil
}

func newFlags() *flagValues {
	values := &flagValues{cooldown: 14 * 24 * time.Hour, timeSource: timeSourceCommit, timeout: 30 * time.Second}
	fs := flag.NewFlagSet("gomod-cooldown", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Var((*cooldownValue)(&values.cooldown), "cooldown", "minimum availability age; accepts Go duration strings plus a 'd' day suffix (e.g. 14d, 168h, 1.5d)")
	fs.StringVar(&values.upstream, "upstream", "https://proxy.golang.org", "upstream GOPROXY URL")
	fs.Var((*timeSourceValue)(&values.timeSource), "time-source", "availability source: commit or combined")
	fs.Var((*upstreamTimeoutValue)(&values.timeout), "upstream-timeout", "upstream HTTP timeout")
	fs.BoolVar(&values.verbose, "verbose", false, "log upstream requests and decisions")
	fs.BoolVar(&values.help, "help", false, "show this help and exit")
	fs.BoolVar(&values.help, "h", false, "show this help and exit")
	fs.BoolVar(&values.version, "version", false, "show version and exit")
	values.fs = fs

	return values
}

func writeUsage(w io.Writer) {
	_, _ = io.WriteString(w, `Usage: gomod-cooldown [options] -- command [args...]

Run a command with a temporary GOPROXY that hides module versions still in cooldown.

Options:
`)
	fs := newFlags().fs
	fs.SetOutput(w)
	fs.PrintDefaults()
}

// ParseCooldown accepts time.ParseDuration plus a day suffix, where one day is
// exactly 24 hours. Months and years deliberately have no meaning here.
func ParseCooldown(s string) (time.Duration, error) {
	if s == "" {
		return 0, errors.New("cooldown is required")
	}
	normalized, err := normalizeDayUnits(s)
	if err != nil {
		return 0, fmt.Errorf("invalid cooldown %q: %w", s, err)
	}
	d, err := time.ParseDuration(normalized)
	if err != nil {
		return 0, fmt.Errorf("invalid cooldown %q: %w", s, err)
	}
	if d <= 0 {
		return 0, errors.New("cooldown must be positive")
	}
	return d, nil
}

func normalizeDayUnits(s string) (string, error) {
	scanner := cooldownScanner(s)
	var b strings.Builder
	for i := 0; i < len(s); {
		end, ok := scanner.scanDecimal(i)
		if !ok {
			b.WriteByte(s[i])
			i++
			continue
		}
		if end >= len(s) || s[end] != 'd' || !scanner.dayNumberCanStart(i) {
			b.WriteString(s[i:end])
			i = end
			continue
		}
		nanoseconds, err := dayNanoseconds(s[i:end])
		if err != nil {
			return "", err
		}
		b.WriteString(nanoseconds)
		b.WriteString("ns")
		i = end + 1
	}
	return b.String(), nil
}

// cooldownScanner is a --cooldown value being scanned for day-suffixed numbers.
type cooldownScanner string

func (s cooldownScanner) dayNumberCanStart(start int) bool {
	if start == 0 || s[start] != '.' {
		return true
	}
	if start == 1 && (s[0] == '+' || s[0] == '-') {
		return true
	}

	return slices.Contains([]uint8{'d', 'h', 'm', 's'}, s[start-1])
}

// scanDigits returns the index just past the digits at start, and whether any
// digit was there.
func (s cooldownScanner) scanDigits(start int) (int, bool) {
	i := start

	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}

	return i, start < i
}

// scanDecimal returns the index just past the decimal number at start, and
// whether any digit was there.
func (s cooldownScanner) scanDecimal(start int) (int, bool) {
	i, hasDigits := s.scanDigits(start)

	if i < len(s) && s[i] == '.' {
		var hasFractionDigits bool
		i, hasFractionDigits = s.scanDigits(i + 1)
		hasDigits = hasDigits || hasFractionDigits
	}

	return i, hasDigits
}

func dayNanoseconds(decimal string) (string, error) {
	days, ok := new(big.Rat).SetString(decimal)
	if !ok {
		return "", errors.New("invalid day value")
	}
	days.Mul(days, new(big.Rat).SetInt64(int64(24*time.Hour)))
	nanoseconds := new(big.Int).Quo(days.Num(), days.Denom())
	if !nanoseconds.IsInt64() {
		return "", errors.New("duration out of range")
	}
	return nanoseconds.String(), nil
}

// Run starts the proxy and child command, returning the child's exit code.
func Run(ctx context.Context, inv Invocation) int {
	opts, err := Parse(inv.Args)
	if err != nil {
		_, _ = fmt.Fprintf(inv.Stderr, "gomod-cooldown: %v\n", err)
		_, _ = fmt.Fprintln(inv.Stderr, "Try 'gomod-cooldown --help' for usage.")
		return 2
	}
	switch opts.action {
	case actionHelp:
		writeUsage(inv.Stdout)
		return 0
	case actionVersion:
		_, _ = fmt.Fprintf(inv.Stdout, "gomod-cooldown %s\n", version())
		return 0
	case actionRun:
		// Continue below.
	}
	err = inv.run(ctx, opts)
	if err != nil {
		return inv.childExitStatus(err)
	}

	return 0
}

func (inv Invocation) childExitStatus(err error) int {
	if startErr, ok := errors.AsType[*childStartError](err); ok {
		_, _ = fmt.Fprintf(inv.Stderr, "gomod-cooldown: %v\n", startErr)

		if startErr.notFound {
			return 127
		}

		return 126
	}

	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		return childExitCode(exit)
	}

	_, _ = fmt.Fprintf(inv.Stderr, "gomod-cooldown: %v\n", err)
	return 1
}

func version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" || info.Main.Version == "(devel)" {
		return "devel"
	}
	return info.Main.Version
}

type childStartError struct {
	command  string
	err      error
	notFound bool
}

func (e *childStartError) Error() string {
	return fmt.Sprintf("start child command %q: %v", e.command, e.err)
}

func (e *childStartError) Unwrap() error { return e.err }

func (inv Invocation) run(ctx context.Context, opts Options) error {
	client := &http.Client{Timeout: opts.UpstreamTimeout}
	started := time.Now()
	clock := func() time.Time { return started }
	source, err := opts.resolveSource(ctx, upstreamDeps{client: client, clock: clock})
	if err != nil {
		return err
	}
	p, err := proxy.New(proxy.Config{
		Upstream: opts.Upstream,
		Client:   client,
		Source:   source,
		Cooldown: opts.Cooldown,
		Now:      clock,
		Logger:   log.New(inv.Stderr, "gomod-cooldown: ", 0),
		Verbose:  opts.Verbose,
	})
	if err != nil {
		return fmt.Errorf("create proxy: %w", err)
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen on loopback: %w", err)
	}
	srv := &http.Server{Handler: p, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		// Preserve context values while allowing shutdown after command cancellation.
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	return inv.runChild(ctx, childCommand{command: opts.Command, proxyURL: "http://" + ln.Addr().String()})
}

func (opts Options) resolveSource(ctx context.Context, deps upstreamDeps) (availability.Source, error) {
	if opts.TimeSource == timeSourceCommit {
		return availability.CommitTimeSource{}, nil
	}
	if strings.TrimRight(opts.Upstream, "/") != "https://proxy.golang.org" {
		return nil, errors.New("time-source=combined requires --upstream=https://proxy.golang.org")
	}
	snapshot, err := (goindex.Fetcher{Client: deps.client, Now: deps.clock}).SnapshotForCooldown(ctx, opts.Cooldown)
	if err != nil {
		return nil, fmt.Errorf("load complete index snapshot: %w", err)
	}
	return availability.CombinedSource{Recent: snapshot.Recent}, nil
}

func (inv Invocation) runChild(ctx context.Context, child childCommand) error {
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, terminationSignals()...)
	defer signal.Stop(signals)
	//nolint:gosec // The caller explicitly supplies the argv after --; no shell is involved.
	cmd := exec.CommandContext(ctx, child.command[0], child.command[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inv.Stdin, inv.Stdout, inv.Stderr
	cmd.Env = environ(os.Environ()).withGOPROXY(child.proxyURL)
	prepared := inv.prepareChildProcess(cmd)
	defer prepared.restoreForeground()
	commandNotFound := errors.Is(cmd.Err, exec.ErrNotFound)
	if !commandNotFound {
		_, statErr := os.Stat(cmd.Path)
		commandNotFound = errors.Is(statErr, os.ErrNotExist)
	}
	cmd.Cancel = func() error {
		return childProcess{process: cmd.Process, group: prepared.processGroup}.cancel()
	}
	if err := cmd.Start(); err != nil {
		return &childStartError{
			command:  child.command[0],
			err:      err,
			notFound: commandNotFound || errors.Is(err, exec.ErrNotFound),
		}
	}
	forwardingDone := make(chan struct{})
	forwardingStopped := make(chan struct{})
	running := childProcess{process: cmd.Process, group: prepared.processGroup}
	go func() {
		defer close(forwardingStopped)
		running.forwardSignals(signalForwarding{signals: signals, done: forwardingDone})
	}()
	err := cmd.Wait()
	close(forwardingDone)
	<-forwardingStopped
	if err != nil {
		return fmt.Errorf("wait for child command: %w", err)
	}
	return nil
}

func (child childProcess) forwardSignals(forwarding signalForwarding) {
	for {
		select {
		case sig := <-forwarding.signals:
			_ = child.forwardSignal(sig)
		case <-forwarding.done:
			return
		}
	}
}

// withGOPROXY returns the environment with any GOPROXY entry replaced by value.
// It returns a plain []string so callers can assign it to exec.Cmd.Env
// without a conversion reading as if one were required.
func (env environ) withGOPROXY(value string) []string {
	result := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if !strings.HasPrefix(entry, "GOPROXY=") {
			result = append(result, entry)
		}
	}
	return append(result, "GOPROXY="+value)
}
