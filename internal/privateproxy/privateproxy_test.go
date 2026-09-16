package privateproxy

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/mod/module"
)

const fixtureModule = "github.com/cooldown-test/PrivateRepo"

func TestBackendDirectGitMetadata(t *testing.T) {
	b := gitBackend(t, true)
	t.Run("list includes retracted version", func(t *testing.T) {
		got := string(readBackend(t, b, "/@v/list"))
		if got != "v1.0.0\nv1.1.0\n" {
			t.Fatalf("list=%q", got)
		}
	})
	for _, suffix := range []string{"/@latest", "/@v/v1.1.0.info", "/@v/!feature.info"} {
		t.Run(suffix, func(t *testing.T) {
			var info struct {
				Version string
				Time    time.Time
			}
			if err := json.Unmarshal(readBackend(t, b, suffix), &info); err != nil {
				t.Fatal(err)
			}
			if info.Version != "v1.1.0" || !info.Time.Equal(time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)) {
				t.Fatalf("metadata=%+v", info)
			}
		})
	}
}

func TestBackendModDoesNotDownloadZip(t *testing.T) {
	b := gitBackend(t, true)
	got := readBackend(t, b, "/@v/v1.0.0.mod")
	if !bytes.Contains(got, []byte("module "+fixtureModule+"\n")) {
		t.Fatalf("go.mod=%s", got)
	}
	err := filepath.WalkDir(filepath.Dir(b.dir), func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.HasSuffix(path, ".zip") {
			t.Errorf("metadata lookup downloaded archive: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestBackendZipStreamsCacheFile(t *testing.T) {
	b := gitBackend(t, true)
	resp := requestBackend(t, b, "/@v/v1.0.0.zip")
	defer resp.Body.Close()
	if _, ok := resp.Body.(*os.File); !ok {
		t.Fatalf("response body=%T, want *os.File", resp.Body)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.ContentLength != int64(len(body)) {
		t.Fatalf("ContentLength=%d length=%d", resp.ContentLength, len(body))
	}
	archive, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	file, err := archive.Open(fixtureModule + "@v1.0.0/dep.go")
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
}

func TestBackendMissingVersionAndModule(t *testing.T) {
	b := gitBackend(t, true)
	escaped, _ := module.EscapePath(fixtureModule)
	for _, suffix := range []string{"/@v/v1.9.0.info", "/nested/@latest"} {
		t.Run(suffix, func(t *testing.T) {
			resp, err := b.RoundTrip(httptest.NewRequest(http.MethodGet, "/"+escaped+suffix, nil))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("status=%d", resp.StatusCode)
			}
		})
	}
}

func requestBackend(t *testing.T, b *Backend, suffix string) *http.Response {
	t.Helper()
	escaped, err := module.EscapePath(fixtureModule)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := b.RoundTrip(httptest.NewRequest(http.MethodGet, "/"+escaped+suffix, nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("%s status=%d body=%s", suffix, resp.StatusCode, data)
	}
	return resp
}

func readBackend(t *testing.T, b *Backend, suffix string) []byte {
	t.Helper()
	resp := requestBackend(t, b, suffix)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestBackendUntaggedListAndLatest(t *testing.T) {
	b := gitBackend(t, false)
	escaped, _ := module.EscapePath(fixtureModule)
	for _, kind := range []string{"/@v/list", "/@latest"} {
		resp, err := b.RoundTrip(httptest.NewRequest(http.MethodGet, "/"+escaped+kind, nil))
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status=%d body=%s err=%v", kind, resp.StatusCode, body, err)
		}
		if kind == "/@v/list" {
			if len(body) != 0 {
				t.Fatalf("untagged list=%q", body)
			}
		} else {
			var result goModule
			if err := json.Unmarshal(body, &result); err != nil || !module.IsPseudoVersion(result.Version) {
				t.Fatalf("latest=%s err=%v", body, err)
			}
		}
	}
}

func TestBackendRejectsInvalidPathsBeforeCommand(t *testing.T) {
	b := New(Config{GoExecutable: "must-not-run", Dir: t.TempDir()})
	for _, path := range []string{
		"/example.com/foo/@v/.info", "/example.com/foo/@v/../../latest.info",
		"/example.com/foo/@v/latest@other.info", "/example.com/foo/@v/latest\\other.info",
		"/example.com/foo/@v/latest.info?query=other", "/../foo/@latest",
		"/example.com/../foo/@latest", "/example.com/foo/@v/!A.info",
		"/example.com/foo/@v/list/extra", "/example.com/foo/@v/unknown",
	} {
		t.Run(path, func(t *testing.T) {
			resp, err := b.RoundTrip(httptest.NewRequest(http.MethodGet, path, nil))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status=%d", resp.StatusCode)
			}
		})
	}
}

func TestBackendRequiresIsolatedDirectory(t *testing.T) {
	b := New(Config{GoExecutable: "must-not-run"})
	resp, err := b.RoundTrip(httptest.NewRequest(http.MethodGet, "/example.com/mod/@latest", nil))
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "isolated working directory") {
		t.Fatalf("error=%v", err)
	}
}

func TestMissingModuleKeepsOperationalErrors(t *testing.T) {
	for _, message := range []string{
		"git ls-remote: exit status 128: Repository not found.",
		"unknown revision v1.0.0: authentication failed",
		"404 Not Found: terminal prompts disabled",
		"dial tcp: network unavailable", "context deadline exceeded",
		"GOVCS disallows using git for private module",
	} {
		if missingModule(message) {
			t.Errorf("operational failure became not found: %s", message)
		}
	}
}

func TestBackendCanceledRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	b := New(Config{Dir: t.TempDir()})
	resp, err := b.RoundTrip(httptest.NewRequestWithContext(ctx, http.MethodGet, "/example.com/mod/@latest", nil))
	if resp != nil {
		resp.Body.Close()
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
}

func TestBackendServesFutureToolchainModule(t *testing.T) {
	b := gitBackendWithGoVersion(t, true, "999.0")
	escaped, _ := module.EscapePath(fixtureModule)
	for _, suffix := range []string{"/@v/list", "/@latest", "/@v/v1.1.0.info", "/@v/v1.1.0.mod"} {
		t.Run(suffix, func(t *testing.T) {
			resp, err := b.RoundTrip(httptest.NewRequest(http.MethodGet, "/"+escaped+suffix, nil))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil || resp.StatusCode != http.StatusOK || len(body) == 0 {
				t.Fatalf("status=%d body=%s err=%v", resp.StatusCode, body, err)
			}
		})
	}
}

func TestBackendPreservesNewerToolchainDownloadError(t *testing.T) {
	b := gitBackendWithGoVersion(t, true, "999.0")
	escaped, _ := module.EscapePath(fixtureModule)
	resp, err := b.RoundTrip(httptest.NewRequest(http.MethodGet, "/"+escaped+"/@v/v1.1.0.zip", nil))
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "requires go >= 999.0") {
		t.Fatalf("response=%v error=%v", resp, err)
	}
}

func TestBackendPinnedVersionIgnoresMalformedLatest(t *testing.T) {
	b := gitBackendWithGoVersion(t, true, "invalid-go-version")
	for _, suffix := range []string{"/@v/v1.0.0.info", "/@v/v1.0.0.mod", "/@v/v1.0.0.zip"} {
		t.Run(suffix, func(t *testing.T) {
			if body := readBackend(t, b, suffix); len(body) == 0 {
				t.Fatal("empty pinned version response")
			}
		})
	}
}

func TestBackendLatestIncludesRetractedRelease(t *testing.T) {
	b := gitBackendWithGoVersion(t, true, "1.20\n\nretract v1.1.0")
	var result goModule
	if err := json.Unmarshal(readBackend(t, b, "/@latest"), &result); err != nil {
		t.Fatal(err)
	}
	if result.Version != "v1.1.0" {
		t.Fatalf("raw latest=%q, want retracted v1.1.0", result.Version)
	}
}

func gitBackend(t *testing.T, tags bool) *Backend {
	t.Helper()
	return gitBackendWithGoVersion(t, tags, "1.20")
}

func gitBackendWithGoVersion(t *testing.T, tags bool, goVersion string) *Backend {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	root := t.TempDir()
	repo, work := filepath.Join(root, "repository"), filepath.Join(root, "work")
	for _, dir := range []string{repo, work} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	env := append(os.Environ(),
		"GOENV=off", "GO111MODULE=on", "GOPROXY=direct", "GOSUMDB=off", "GONOPROXY=none", "GONOSUMDB=*",
		"GOWORK=off", "GOFLAGS=-modcacherw", "GOTOOLCHAIN=local", "GOVCS=*:git", "GOPRIVATE=github.com/cooldown-test/*",
		"GOMODCACHE="+filepath.Join(root, "cache"),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+filepath.Join(root, "no-global-config"),
		"GIT_ALLOW_PROTOCOL=file", "GIT_CONFIG_PARAMETERS=",
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=url."+localFileURL(repo)+".insteadOf",
		"GIT_CONFIG_VALUE_0=https://"+fixtureModule, "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Cooldown test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Cooldown test", "GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_AUTHOR_DATE=2024-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2024-01-01T00:00:00Z",
	)
	git := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir, cmd.Env = repo, env
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}
	git("init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module "+fixtureModule+"\n\ngo 1.20\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "dep.go"), []byte("package dep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-m", "initial")
	if tags {
		git("tag", "v1.0.0")
		if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module "+fixtureModule+"\n\ngo "+goVersion+"\n\nretract v1.0.0\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		env = append(env, "GIT_AUTHOR_DATE=2024-02-01T00:00:00Z", "GIT_COMMITTER_DATE=2024-02-01T00:00:00Z")
		git("add", ".")
		git("commit", "-m", "retract old release")
		git("tag", "v1.1.0")
		git("branch", "Feature")
	}
	backend := New(Config{Dir: work, Env: env})
	// New must own its environment even when the caller reuses its slice.
	env[slices.Index(env, "GOPROXY=direct")] = "GOPROXY=off"
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove private cache: %v", err)
		}
	})
	return backend
}

func localFileURL(path string) string {
	urlPath := filepath.ToSlash(path)
	if !strings.HasPrefix(urlPath, "/") {
		urlPath = "/" + urlPath
	}
	return (&url.URL{Scheme: "file", Path: urlPath}).String()
}
