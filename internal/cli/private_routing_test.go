package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dev-hato/gomod-cooldown/internal/availability"
	"golang.org/x/mod/module"
)

func TestRunDoesNotStartChildWithoutPrivateRouting(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := Run(t.Context(), []string{
		"--", executable, "-test.run=^TestRunProcessHelper$", "--", processHelperMarker, "exit", "7",
	}, nil, &stdout, &stderr)
	if code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "private module routing requires go on PATH") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	for _, flag := range []string{"--help", "--version"} {
		stdout.Reset()
		stderr.Reset()
		if code = Run(t.Context(), []string{flag}, nil, &stdout, &stderr); code != 0 || stderr.Len() != 0 || stdout.Len() == 0 {
			t.Fatalf("%s: code=%d stdout=%q stderr=%q", flag, code, stdout.String(), stderr.String())
		}
	}
}

type privateTestTransport func(*http.Request) (*http.Response, error)

func (f privateTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestPrivateTransportRoutesEveryPrivateEndpoint(t *testing.T) {
	for _, basePath := range []string{"", "/proxy/cache"} {
		for _, endpoint := range []string{
			"/@v/list", "/@latest", "/@v/v1.2.3.info", "/@v/v1.2.3.mod", "/@v/v1.2.3.zip",
			"/@v/v1.2.3.unsupported", "/@unknown",
		} {
			t.Run(basePath+endpoint, func(t *testing.T) {
				const escapedModule = "github.com/!goryudyuma/!private"
				wantPath := "/" + escapedModule + endpoint
				req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
					"https://public.example"+basePath+"/github.com/%21goryudyuma/%21private"+endpoint, nil)
				if err != nil {
					t.Fatal(err)
				}
				originalURL := req.URL.String()
				var directCalls, publicCalls int
				transport := privateTransport{
					patterns: "github.com/Goryudyuma/*", basePath: basePath,
					direct: privateTestTransport(func(got *http.Request) (*http.Response, error) {
						directCalls++
						if got.URL.Path != wantPath {
							t.Errorf("direct path=%q, want %q", got.URL.Path, wantPath)
						}
						if got.Context() != req.Context() {
							t.Error("direct request lost caller context")
						}
						return &http.Response{StatusCode: http.StatusBadGateway, Body: io.NopCloser(strings.NewReader("private failed"))}, nil
					}),
					public: privateTestTransport(func(*http.Request) (*http.Response, error) {
						publicCalls++
						return nil, errors.New("private request reached public transport")
					}),
				}
				resp, err := transport.RoundTrip(req)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = resp.Body.Close() }()
				if resp.StatusCode != http.StatusBadGateway || directCalls != 1 || publicCalls != 0 {
					t.Fatalf("status=%d direct calls=%d public calls=%d", resp.StatusCode, directCalls, publicCalls)
				}
				if req.URL.String() != originalURL {
					t.Fatalf("caller URL mutated to %q, want %q", req.URL.String(), originalURL)
				}
			})
		}
	}
}

func TestPrivateTransportDoesNotRetryPrivateErrorsPublicly(t *testing.T) {
	wantErr := errors.New("authentication failed")
	transport := privateTransport{
		patterns: "private.example/*",
		direct: privateTestTransport(func(*http.Request) (*http.Response, error) {
			return nil, wantErr
		}),
		public: privateTestTransport(func(*http.Request) (*http.Response, error) {
			t.Fatal("failed private request reached public transport")
			return nil, errors.New("private request reached public transport")
		}),
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://public.example/private.example/repo/@v/list", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := transport.RoundTrip(req)
	if resp != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("error=%v, want %v", err, wantErr)
	}
}

func TestPrivateTransportPreservesPublicAndSumDBRequests(t *testing.T) {
	for _, path := range []string{
		"/cache/public.example/repo/@v/list",
		"/cache/public.example/repo/@v/v1.2.3.zip",
		"/cache/sumdb/sum.golang.org/supported",
		"/cache/sumdb/sum.golang.org/lookup/private.example/repo@v1.2.3",
		"/cache/sumdb/sum.golang.org/tile/8/0/001",
	} {
		t.Run(path, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://public.example"+path, nil)
			if err != nil {
				t.Fatal(err)
			}
			publicCalls := 0
			transport := privateTransport{
				patterns: "private.example/*", basePath: "/cache",
				direct: privateTestTransport(func(*http.Request) (*http.Response, error) {
					t.Fatal("public request reached private transport")
					return nil, errors.New("public request reached private transport")
				}),
				public: privateTestTransport(func(got *http.Request) (*http.Response, error) {
					publicCalls++
					if got != req {
						t.Error("public transport did not receive original request")
					}
					return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
				}),
			}
			resp, err := transport.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			if publicCalls != 1 {
				t.Fatalf("public calls=%d, want 1", publicCalls)
			}
		})
	}
}

func TestPrivateAvailabilityPreservesPublicCombinedSource(t *testing.T) {
	commit := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	cached := commit.Add(24 * time.Hour)
	source := privateAvailability{
		patterns: "private.example/*",
		public: availability.CombinedSource{Recent: map[string]time.Time{
			availability.Key("private.example/repo", "v1.0.0"): cached,
			availability.Key("public.example/repo", "v1.0.0"):  cached,
		}},
	}
	private, err := source.AvailableAt(t.Context(), "private.example/repo", "v1.0.0", commit)
	if err != nil {
		t.Fatal(err)
	}
	if !private.CommitTime.Equal(commit) || !private.AvailableAt.Equal(commit) || private.FirstCached != nil {
		t.Fatalf("private availability=%+v, want only commit time %s", private, commit)
	}
	public, err := source.AvailableAt(t.Context(), "public.example/repo", "v1.0.0", commit)
	if err != nil {
		t.Fatal(err)
	}
	if !public.CommitTime.Equal(commit) || !public.AvailableAt.Equal(cached) || public.FirstCached == nil || !public.FirstCached.Equal(cached) {
		t.Fatalf("public availability=%+v, want combined first-cached time %s", public, cached)
	}
}

type privateSettingsTest struct {
	name       string
	file       string
	private    string
	noproxy    string
	nosumdb    string
	wantPaths  []string
	wantNoSums []string
}

func TestNewPrivateAccessUsesEffectiveGoSettings(t *testing.T) {
	goExecutable, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go is required to resolve effective Go settings")
	}
	for _, test := range []privateSettingsTest{
		{
			name: "GOENV defaults", file: "GOPRIVATE=file.example/*\n",
			wantPaths: []string{"file.example/repo"}, wantNoSums: []string{"file.example/repo"},
		},
		{
			name: "explicit GOENV settings", file: "GOPRIVATE=file.example/*\nGONOPROXY=direct.example/*\nGONOSUMDB=none\n",
			wantPaths: []string{"file.example/repo", "direct.example/repo"}, wantNoSums: []string{"file.example/repo", "direct.example/repo"},
		},
		{
			name: "environment overrides file", file: "GOPRIVATE=file.example/*\nGONOPROXY=file-direct.example/*\nGONOSUMDB=file-sums.example/*\n",
			private: "env.example/*", noproxy: "direct.example/*", nosumdb: "none",
			wantPaths: []string{"env.example/repo", "direct.example/repo"}, wantNoSums: []string{"env.example/repo", "direct.example/repo"},
		},
		{
			name: "preserve explicit sumdb exclusions", private: "private.example/*", noproxy: "none", nosumdb: "sum-only.example/*",
			wantPaths: []string{"private.example/repo"}, wantNoSums: []string{"private.example/repo", "sum-only.example/repo"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			checkPrivateGoSettings(t, goExecutable, test)
		})
	}
}

func checkPrivateGoSettings(t *testing.T, goExecutable string, test privateSettingsTest) {
	t.Helper()
	goenv := filepath.Join(t.TempDir(), "goenv")
	if err := os.WriteFile(goenv, []byte(test.file), 0o600); err != nil {
		t.Fatal(err)
	}
	env := withEnvironment(os.Environ(), map[string]string{
		"GOENV": goenv, "GOPRIVATE": test.private, "GONOPROXY": test.noproxy, "GONOSUMDB": test.nosumdb,
		"GOTOOLCHAIN": "local", "GOWORK": "off", "GOFLAGS": "", "GOTELEMETRY": "off",
	})
	originalEnv := slices.Clone(env)
	originalProcessEnv := os.Environ()
	access, err := newPrivateAccess(t.Context(), Options{Upstream: "https://public.example/cache", UpstreamTimeout: 10 * time.Second}, env)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := access.close(); err != nil {
			t.Error(err)
		}
	}()
	for _, path := range test.wantPaths {
		if !module.MatchPrefixPatterns(access.patterns, path) {
			t.Errorf("patterns=%q do not route %q", access.patterns, path)
		}
	}
	if module.MatchPrefixPatterns(access.patterns, "sum-only.example/repo") || module.MatchPrefixPatterns(access.patterns, "public.example/repo") {
		t.Errorf("patterns=%q unexpectedly route a public module", access.patterns)
	}
	settings, err := privateGoSettings(t.Context(), goExecutable, access.dir, access.childEnv, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if settings.GONOPROXY != "none" {
		t.Errorf("child GONOPROXY=%q, want none", settings.GONOPROXY)
	}
	for _, path := range test.wantNoSums {
		if !module.MatchPrefixPatterns(settings.GONOSUMDB, path) {
			t.Errorf("child GONOSUMDB=%q does not exclude %q", settings.GONOSUMDB, path)
		}
	}
	if !slices.Equal(env, originalEnv) {
		t.Error("private access mutated caller environment slice")
	}
	if !slices.Equal(os.Environ(), originalProcessEnv) {
		t.Error("private access mutated process environment")
	}
	gotFile, err := os.ReadFile(goenv)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotFile) != test.file {
		t.Errorf("GOENV file was modified to %q", gotFile)
	}
}

func TestPrivateAccessCloseRemovesNestedCache(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private-workspace")
	cacheDir := filepath.Join(dir, "modcache", "private.example", "repo@v1.0.0", "subpackage")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "private.go"), []byte("package private\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	access := &privateAccess{dir: dir}
	if err := access.close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("private workspace remains after close: %v", err)
	}
	if err := access.close(); err != nil {
		t.Fatalf("closing removed workspace: %v", err)
	}
}

type privateTestSource func(context.Context, string, string, time.Time) (availability.Availability, error)

func (f privateTestSource) AvailableAt(ctx context.Context, path, version string, commit time.Time) (availability.Availability, error) {
	return f(ctx, path, version, commit)
}

func TestPrivateAvailabilityPropagatesPublicFailureOnly(t *testing.T) {
	wantErr := errors.New("public index unavailable")
	publicCalls := 0
	source := privateAvailability{
		patterns: "private.example/*",
		public: privateTestSource(func(context.Context, string, string, time.Time) (availability.Availability, error) {
			publicCalls++
			return availability.Availability{}, wantErr
		}),
	}
	if _, err := source.AvailableAt(t.Context(), "private.example/repo", "v1.0.0", time.Now()); err != nil || publicCalls != 0 {
		t.Fatalf("private lookup error=%v public calls=%d", err, publicCalls)
	}
	if _, err := source.AvailableAt(t.Context(), "public.example/repo", "v1.0.0", time.Now()); !errors.Is(err, wantErr) || publicCalls != 1 {
		t.Fatalf("public lookup error=%v public calls=%d", err, publicCalls)
	}
}
