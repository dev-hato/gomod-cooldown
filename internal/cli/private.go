package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/dev-hato/gomod-cooldown/internal/availability"
	"github.com/dev-hato/gomod-cooldown/internal/privateproxy"
	"golang.org/x/mod/module"
)

type privateAccess struct {
	patterns  string
	childEnv  []string
	transport http.RoundTripper
	dir       string
}

func (p *privateAccess) close() error {
	if err := os.RemoveAll(p.dir); err != nil {
		return fmt.Errorf("remove private module workspace: %w", err)
	}
	return nil
}

func newPrivateAccess(ctx context.Context, opts Options, env []string) (*privateAccess, error) {
	upstream, err := url.Parse(opts.Upstream)
	if err != nil || upstream.Host == "" || (upstream.Scheme != "http" && upstream.Scheme != "https") || upstream.User != nil || upstream.RawQuery != "" || upstream.Fragment != "" {
		return nil, fmt.Errorf("invalid upstream URL %q", opts.Upstream)
	}
	goExecutable, err := exec.LookPath("go")
	if err != nil {
		return nil, fmt.Errorf("private module routing requires go on PATH: %w", err)
	}
	dir, err := os.MkdirTemp("", "gomod-cooldown-private-")
	if err != nil {
		return nil, fmt.Errorf("create private module workspace: %w", err)
	}
	p := &privateAccess{dir: dir}
	ready := false
	defer func() {
		if !ready {
			_ = p.close()
		}
	}()
	// Keep Go from finding a caller's go.mod even when TMPDIR is inside a module.
	if err = os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module gomod-cooldown.invalid/private-fetch\n\ngo 1.18\n"), 0o600); err != nil {
		return nil, fmt.Errorf("initialize private module workspace: %w", err)
	}
	backendEnv := withEnvironment(env, map[string]string{
		"GO111MODULE": "on", "GOFLAGS": "-modcacherw", "GOWORK": "off", "GOTOOLCHAIN": "local",
		"GOPROXY": "direct", "GOSUMDB": "off", "GOMODCACHE": filepath.Join(dir, "modcache"),
	})
	settings, err := privateGoSettings(ctx, goExecutable, dir, backendEnv, opts.UpstreamTimeout)
	if err != nil {
		return nil, err
	}
	p.patterns = joinPatterns(settings.GOPRIVATE, settings.GONOPROXY)
	p.childEnv = withEnvironment(env, map[string]string{
		"GONOPROXY": "none",
		// The outer Go command also verifies downloads. Keep routed private paths
		// off the public checksum database, including when GONOSUMDB was explicit.
		"GONOSUMDB": joinPatterns(settings.GONOSUMDB, p.patterns),
	})
	p.transport = privateTransport{
		patterns: p.patterns,
		basePath: strings.TrimRight(upstream.Path, "/"),
		direct: privateproxy.New(privateproxy.Config{
			GoExecutable: goExecutable, Dir: dir, Env: backendEnv,
		}),
		public: http.DefaultTransport,
	}
	ready = true
	return p, nil
}

type goPrivateSettings struct {
	GOPRIVATE string
	GONOPROXY string
	GONOSUMDB string
}

func privateGoSettings(ctx context.Context, executable, dir string, env []string, timeout time.Duration) (goPrivateSettings, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "env", "-json", "GOPRIVATE", "GONOPROXY", "GONOSUMDB")
	cmd.Dir, cmd.Env = dir, env
	cmd.WaitDelay = time.Second
	body, err := cmd.Output()
	if err != nil {
		return goPrivateSettings{}, fmt.Errorf("read private module Go settings: %w", err)
	}
	var settings goPrivateSettings
	if err = json.Unmarshal(body, &settings); err != nil {
		return settings, fmt.Errorf("decode private module Go settings: %w", err)
	}
	return settings, nil
}

// privateTransport routes before any HTTP request is sent to the public proxy.
// Requests for a matching module never fall back to the public transport.
type privateTransport struct {
	patterns string
	basePath string
	direct   http.RoundTripper
	public   http.RoundTripper
}

func (t privateTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	path, ok := strings.CutPrefix(req.URL.Path, t.basePath+"/")
	if !ok {
		return moduleRoundTrip(t.public, req)
	}
	escapedModule, _, hasEndpoint := strings.Cut(path, "/@")
	if !hasEndpoint {
		return moduleRoundTrip(t.public, req)
	}
	modulePath, err := module.UnescapePath(escapedModule)
	if err != nil {
		return nil, fmt.Errorf("invalid module request path: %w", err)
	}
	if !module.MatchPrefixPatterns(t.patterns, modulePath) {
		return moduleRoundTrip(t.public, req)
	}
	directReq := req.Clone(req.Context())
	directReq.URL.Path, directReq.URL.RawPath = "/"+path, ""
	return moduleRoundTrip(t.direct, directReq)
}

func moduleRoundTrip(transport http.RoundTripper, req *http.Request) (*http.Response, error) {
	resp, err := transport.RoundTrip(req)
	if err != nil {
		return resp, fmt.Errorf("module upstream request: %w", err)
	}
	return resp, nil
}

type privateAvailability struct {
	patterns string
	public   availability.Source
}

func (s privateAvailability) AvailableAt(ctx context.Context, path, version string, commitTime time.Time) (availability.Availability, error) {
	source := s.public
	if module.MatchPrefixPatterns(s.patterns, path) {
		source = availability.CommitTimeSource{}
	}
	result, err := source.AvailableAt(ctx, path, version, commitTime)
	if err != nil {
		return result, fmt.Errorf("module availability: %w", err)
	}
	return result, nil
}

func joinPatterns(patterns ...string) string {
	var nonempty []string
	for _, pattern := range patterns {
		if pattern != "" {
			nonempty = append(nonempty, pattern)
		}
	}
	return strings.Join(nonempty, ",")
}

func withEnvironment(env []string, overrides map[string]string) []string {
	result := make([]string, 0, len(env)+len(overrides))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if _, replace := overrides[key]; !replace {
			result = append(result, entry)
		}
	}
	keys := make([]string, 0, len(overrides))
	for key := range overrides {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		result = append(result, key+"="+overrides[key])
	}
	return result
}
