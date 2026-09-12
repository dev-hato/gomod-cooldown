package cli

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const privateIntegrationModulePath = "github.com/Goryudyuma/private-test"

func TestGoCommandFiltersPrivateModulesByDefault(t *testing.T) {
	for _, settings := range []string{"GOPRIVATE", "GONOPROXY", "GOENV", "GONOPROXY none"} {
		t.Run(settings, func(t *testing.T) {
			configurePrivateIntegrationGo(t)
			switch settings {
			case "GOPRIVATE":
				t.Setenv("GOPRIVATE", "github.com/Goryudyuma/*")
			case "GONOPROXY":
				t.Setenv("GONOPROXY", "github.com/Goryudyuma/*")
			case "GOENV":
				goenv := filepath.Join(t.TempDir(), "goenv")
				writeFile(t, goenv, []byte("GOPRIVATE=github.com/Goryudyuma/*\n"))
				t.Setenv("GOENV", goenv)
			case "GONOPROXY none":
				t.Setenv("GOPRIVATE", "github.com/Goryudyuma/*")
				t.Setenv("GONOPROXY", "none")
			}
			p := newRecordingModuleProxy(t, nil)
			// go get probes shorter module prefixes, which do not match the
			// configured github.com/Goryudyuma/* pattern.
			p.addPrefixNotFoundResponses(testProxyModule{path: privateIntegrationModulePath})
			importPath := privateIntegrationModulePath
			if settings == "GOPRIVATE" {
				importPath += "/subpkg/nested"
			}
			dir := writeIntegrationModuleFor(t, privateIntegrationModulePath, "v1.0.0", importPath)

			code, stdout, stderr := runPrivateGoThroughCooldown(t, p.server.URL, dir,
				"list", "-m", "-versions", privateIntegrationModulePath)
			if code != 0 {
				t.Fatalf("list exit=%d stderr=%s", code, stderr)
			}
			want := []string{privateIntegrationModulePath, "v1.0.0", "v1.1.0"}
			if got := strings.Fields(stdout); !slices.Equal(got, want) {
				t.Fatalf("versions=%q, want %q", got, want)
			}

			code, _, stderr = runPrivateGoThroughCooldown(t, p.server.URL, dir, "get", "-u", "./...")
			if code != 0 {
				t.Fatalf("upgrade exit=%d stderr=%s", code, stderr)
			}
			if got := requiredVersion(t, dir, privateIntegrationModulePath); got != "v1.1.0" {
				t.Fatalf("required version=%q, want v1.1.0", got)
			}

			code, _, stderr = runPrivateGoThroughCooldown(t, p.server.URL, dir,
				"mod", "download", privateIntegrationModulePath+"@v1.2.0")
			if code != 0 {
				t.Fatalf("explicit recent download exit=%d stderr=%s", code, stderr)
			}
			if p.countContains(moduleEndpoint(privateIntegrationModulePath, "")) != 0 {
				t.Fatalf("private module contacted public upstream: %v", p.allRequests())
			}
			p.assertNoUnknown(t)
		})
	}
}

func TestPrivateFilteringCannotBeDisabled(t *testing.T) {
	for _, option := range []string{"--include-private=false", "--include-private=true", "--include-private"} {
		t.Run(option, func(t *testing.T) {
			args := []string{option, "--", "must-not-run"}
			var stdout, stderr lockedBuffer
			if _, err := Parse(args, &stderr); err == nil {
				t.Fatalf("Parse accepted removed option %q", option)
			}
			code := Run(context.Background(), args, nil, &stdout, &stderr)
			if code != 2 || !strings.Contains(stderr.String(), "flag provided but not defined: -include-private") {
				t.Fatalf("Run(%q) exit=%d stderr=%s, want unknown-flag usage error", option, code, stderr.String())
			}
		})
	}
	var stdout, stderr lockedBuffer
	if code := Run(context.Background(), []string{"--help"}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("help exit=%d stderr=%s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "include-private") {
		t.Fatalf("help advertises removed option: %s", stdout.String())
	}
}

func TestGoCommandFiltersPublicAndPrivateUpgrades(t *testing.T) {
	configurePrivateIntegrationGo(t)
	t.Setenv("GOPRIVATE", "github.com/Goryudyuma/*")
	p := newRecordingModuleProxy(t, []testProxyModule{standardIntegrationModule()})
	p.addPrefixNotFoundResponses(testProxyModule{path: privateIntegrationModulePath})
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), fmt.Appendf(nil,
		"module example.com/gomod-cooldown-test/root\n\ngo 1.25.0\n\nrequire (\n\t%s v1.0.0\n\t%s v1.0.0\n)\n",
		privateIntegrationModulePath, integrationModulePath))
	writeFile(t, filepath.Join(dir, "root.go"), fmt.Appendf(nil,
		"package root\n\nimport (\n\t_ %q\n\t_ %q\n)\n", privateIntegrationModulePath, integrationModulePath))

	code, _, stderr := runPrivateGoThroughCooldown(t, p.server.URL, dir, "get", "-u", "./...")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	for _, path := range []string{privateIntegrationModulePath, integrationModulePath} {
		if got := requiredVersion(t, dir, path); got != "v1.1.0" {
			t.Fatalf("%s required version=%q, want v1.1.0", path, got)
		}
	}
	if p.countContains(moduleEndpoint(privateIntegrationModulePath, "")) != 0 {
		t.Fatalf("private module contacted public upstream: %v", p.allRequests())
	}
	if p.countContains(versionEndpoint(integrationModulePath, "v1.1.0", ".zip")) == 0 {
		t.Fatalf("public eligible version was not downloaded: %v", p.allRequests())
	}
	if p.countContains(versionEndpoint(integrationModulePath, "v1.2.0", ".zip")) != 0 {
		t.Fatalf("public version in cooldown was downloaded: %v", p.allRequests())
	}
	p.assertNoUnknown(t)
}

func configurePrivateIntegrationGo(t *testing.T) {
	t.Helper()
	configureIsolatedGo(t)
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is required for private-module integration tests")
	}
	root := t.TempDir()
	// Keep each invocation's private fetch workspace observable after Run exits.
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(key, root)
	}
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	globalConfig := filepath.Join(root, "gitconfig")
	writeFile(t, globalConfig, nil)
	for key, value := range map[string]string{
		"GIT_CONFIG_GLOBAL": globalConfig, "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_COUNT": "0",
		"GIT_CONFIG_PARAMETERS": "", "GIT_ALLOW_PROTOCOL": "file", "GIT_TERMINAL_PROMPT": "0",
		"GIT_AUTHOR_NAME": "Integration Test", "GIT_AUTHOR_EMAIL": "integration@example.invalid",
		"GIT_COMMITTER_NAME": "Integration Test", "GIT_COMMITTER_EMAIL": "integration@example.invalid",
		// GONOPROXY can select a module while GOPRIVATE is empty, so allow this
		// exact fixture even when Go classifies it as public for GOVCS purposes.
		"GOVCS": privateIntegrationModulePath + ":git,public:off,private:git",
		// Block accidental metadata requests outside the loopback test servers.
		"HTTP_PROXY": "http://127.0.0.1:1", "HTTPS_PROXY": "http://127.0.0.1:1",
		"ALL_PROXY": "http://127.0.0.1:1", "NO_PROXY": "localhost,127.0.0.1,::1",
		"http_proxy": "http://127.0.0.1:1", "https_proxy": "http://127.0.0.1:1",
		"all_proxy": "http://127.0.0.1:1", "no_proxy": "localhost,127.0.0.1,::1",
	} {
		t.Setenv(key, value)
	}
	runGit := func(stamp time.Time, args ...string) {
		t.Helper()
		cmd := exec.Command(git, args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE="+stamp.Format(time.RFC3339), "GIT_COMMITTER_DATE="+stamp.Format(time.RFC3339))
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}
	now := time.Now().UTC().Truncate(time.Second)
	runGit(now, "init", "--quiet")
	writeFile(t, filepath.Join(repo, "go.mod"), []byte("module "+privateIntegrationModulePath+"\n\ngo 1.20\n"))
	if err := os.MkdirAll(filepath.Join(repo, "subpkg", "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(repo, "subpkg", "nested", "nested.go"), []byte("package nested\n"))
	for _, version := range []struct {
		name string
		age  time.Duration
	}{
		{"v1.0.0", 60 * 24 * time.Hour},
		{"v1.1.0", 30 * 24 * time.Hour},
		{"v1.2.0", time.Hour},
	} {
		stamp := now.Add(-version.age)
		writeFile(t, filepath.Join(repo, "dep.go"), []byte("package dep\n\nconst Version = "+fmt.Sprintf("%q", version.name)+"\n"))
		runGit(stamp, "add", "go.mod", "dep.go", "subpkg")
		runGit(stamp, "-c", "commit.gpgSign=false", "commit", "--quiet", "-m", version.name)
		runGit(stamp, "-c", "tag.gpgSign=false", "tag", version.name)
	}
	runGit(now, "config", "--global", "url."+privateIntegrationFileURL(repo)+".insteadOf", "https://"+privateIntegrationModulePath)
}

func privateIntegrationFileURL(repo string) string {
	repoPath := filepath.ToSlash(repo)
	if !strings.HasPrefix(repoPath, "/") {
		repoPath = "/" + repoPath
	}
	return (&url.URL{Scheme: "file", Path: repoPath}).String()
}

func runPrivateGoThroughCooldown(t *testing.T, upstream, dir string, goArgs ...string) (int, string, string) {
	t.Helper()
	goExecutable, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("find go executable: %v", err)
	}
	args := []string{"--time-source=commit", "--cooldown=14d", "--upstream=" + upstream, "--upstream-timeout=5s"}
	args = append(args, "--", goExecutable, "-C", dir)
	args = append(args, goArgs...)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var stdout, stderr lockedBuffer
	code := Run(ctx, args, nil, &stdout, &stderr)
	leftovers, err := filepath.Glob(filepath.Join(os.TempDir(), "gomod-cooldown-private-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("private fetch workspaces remain after Run: %v", leftovers)
	}
	return code, stdout.String(), stderr.String()
}
