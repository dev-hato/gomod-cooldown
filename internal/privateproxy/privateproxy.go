// Package privateproxy serves the GOPROXY protocol through a separate Go
// process, allowing private modules to use the caller's existing VCS credentials.
package privateproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/mod/module"
)

// Config supplies an isolated working directory and environment for the Go
// subprocess. Env must configure direct VCS access and a separate module cache.
type Config struct {
	GoExecutable string
	Dir          string
	Env          []string
}

// Backend implements http.RoundTripper for private module requests. Its caller
// routes matching module paths here instead of contacting a public proxy.
type Backend struct {
	goExecutable string
	dir          string
	env          []string
}

// New constructs a backend. Config.Dir must be an isolated directory outside
// the user's main module, and must remain available until responses are closed.
func New(cfg Config) *Backend {
	if cfg.GoExecutable == "" {
		cfg.GoExecutable = "go"
	}
	return &Backend{goExecutable: cfg.GoExecutable, dir: cfg.Dir, env: slices.Clone(cfg.Env)}
}

type endpoint struct {
	path  string
	query string
	kind  string
}

const (
	kindList    = "list"
	kindMod     = "mod"
	queryLatest = "latest"
)

type goModule struct {
	Path     string
	Version  string
	Versions []string
	Time     time.Time
	GoMod    string
	Zip      string
	Error    json.RawMessage
}

// RoundTrip serves one decoded GOPROXY path. Zip files are streamed directly
// from the isolated module cache, without buffering them in memory.
func (b *Backend) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		defer func() { _ = req.Body.Close() }()
	}
	if req.Method != http.MethodGet {
		return textResponse(req, http.StatusMethodNotAllowed, "only GET is supported\n"), nil
	}
	e, err := parseEndpoint(req.URL.Path)
	if err != nil || req.URL.RawQuery != "" {
		return textResponse(req, http.StatusBadRequest, "invalid module proxy request\n"), nil
	}
	if b.dir == "" {
		return nil, errors.New("private module backend requires an isolated working directory")
	}
	if e.kind == "zip" {
		return b.download(req, e)
	}
	return b.metadata(req, e)
}

func parseEndpoint(path string) (endpoint, error) {
	trimmed, ok := strings.CutPrefix(path, "/")
	if !ok {
		return endpoint{}, errors.New("missing leading slash")
	}
	escapedPath, suffix, found := strings.Cut(trimmed, "/@")
	if !found {
		return endpoint{}, errors.New("unknown module endpoint")
	}
	modPath, err := module.UnescapePath(escapedPath)
	if err != nil {
		return endpoint{}, fmt.Errorf("invalid module path: %w", err)
	}
	if suffix == queryLatest {
		return endpoint{path: modPath, query: queryLatest, kind: "info"}, nil
	}
	if suffix == "v/list" {
		return endpoint{path: modPath, kind: kindList}, nil
	}
	versionSuffix, ok := strings.CutPrefix(suffix, "v/")
	if !ok {
		return endpoint{}, errors.New("unknown module endpoint")
	}
	for _, kind := range []string{"info", kindMod, "zip"} {
		if query, matches := strings.CutSuffix(versionSuffix, "."+kind); matches {
			version, err := module.UnescapeVersion(query)
			if err != nil || strings.ContainsAny(version, "@/\\") {
				return endpoint{}, errors.New("invalid module query")
			}
			return endpoint{path: modPath, query: version, kind: kind}, nil
		}
	}
	return endpoint{}, errors.New("unknown module endpoint")
}

func (b *Backend) metadata(req *http.Request, e endpoint) (*http.Response, error) {
	args := []string{kindList, "-m", "-json"}
	if e.kind == kindList || e.query == queryLatest {
		// Discovery exposes retracted releases for the client to evaluate.
		// Exact versions and revisions already allow retractions; adding this
		// flag would unnecessarily make them depend on the latest go.mod.
		args = append(args, "-retracted")
	}
	if e.kind == kindList {
		args = append(args, "-versions")
	}
	query := e.path
	if e.kind != kindList {
		query += "@" + e.query
	}
	args = append(args, query)
	var result goModule
	if err := b.run(req.Context(), args, &result); err != nil {
		return commandResponse(req, err)
	}
	if result.Path != e.path {
		return nil, errors.New("private module metadata returned a different module path")
	}
	if e.kind == kindMod {
		return cacheFileResponse(req, result.GoMod, "text/plain; charset=utf-8")
	}
	if e.kind == kindList {
		for _, version := range result.Versions {
			if module.CanonicalVersion(version) != version || version == "" {
				return nil, errors.New("private module metadata returned an invalid version")
			}
		}
		body := strings.Join(result.Versions, "\n")
		if body != "" {
			body += "\n"
		}
		return textResponse(req, http.StatusOK, body), nil
	}
	if result.Version == "" || module.CanonicalVersion(result.Version) != result.Version || result.Time.IsZero() {
		return nil, errors.New("private module metadata omitted a valid version or commit time")
	}
	body, err := json.Marshal(struct {
		Version string
		Time    time.Time
	}{Version: result.Version, Time: result.Time})
	if err != nil {
		return nil, fmt.Errorf("encode private module metadata: %w", err)
	}
	return response(req, http.StatusOK, "application/json", io.NopCloser(bytes.NewReader(body)), int64(len(body))), nil
}

func (b *Backend) download(req *http.Request, e endpoint) (*http.Response, error) {
	var result goModule
	if err := b.run(req.Context(), []string{kindMod, "download", "-json", e.path + "@" + e.query}, &result); err != nil {
		// Some toolchain-version failures happen after the verified archive
		// was cached. Serve it only when Go explicitly reports that file.
		if result.Zip == "" || !strings.Contains(result.errorMessage(), "requires go >= ") {
			return commandResponse(req, err)
		}
	}
	if result.Path != e.path || result.Version == "" || module.CanonicalVersion(result.Version) != result.Version {
		return nil, errors.New("private module download returned invalid module metadata")
	}
	return cacheFileResponse(req, result.Zip, "application/zip")
}

func cacheFileResponse(req *http.Request, path, contentType string) (*http.Response, error) {
	if path == "" {
		return nil, errors.New("private module download omitted requested cache file")
	}
	//nolint:gosec // The path is produced by the configured Go executable, not by the request.
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open private module cache file: %w", err)
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		if err != nil {
			return nil, fmt.Errorf("inspect private module cache file: %w", err)
		}
		return nil, errors.New("private module cache file is not a regular file")
	}
	return response(req, http.StatusOK, contentType, file, info.Size()), nil
}

type commandError struct {
	message string
	err     error
}

func (e *commandError) Error() string { return "private module lookup: " + e.message }
func (e *commandError) Unwrap() error { return e.err }

func (b *Backend) run(ctx context.Context, args []string, result *goModule) error {
	//nolint:gosec // Executable is configured by the caller; validated module queries are separate arguments.
	cmd := exec.CommandContext(ctx, b.goExecutable, args...)
	cmd.Dir, cmd.Env = b.dir, b.env
	cmd.WaitDelay = time.Second
	prepareCommand(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return fmt.Errorf("private module lookup: %w", ctx.Err())
	}
	if err != nil {
		// Download failures are reported as JSON on stdout; list failures use
		// stderr. Preserve partial download metadata when Go reports it.
		_ = json.Unmarshal(stdout.Bytes(), result)
		message := strings.TrimSpace(stderr.String())
		if failure := result.errorMessage(); failure != "" {
			message = failure
		}
		if message == "" {
			message = err.Error()
		}
		return &commandError{message: message, err: err}
	}
	if err := json.Unmarshal(stdout.Bytes(), result); err != nil {
		return fmt.Errorf("decode private module metadata: %w", err)
	}
	if failure := result.errorMessage(); failure != "" {
		return &commandError{message: failure}
	}
	return nil
}

func (m *goModule) errorMessage() string {
	if len(m.Error) == 0 || string(m.Error) == "null" {
		return ""
	}
	// go list represents Error as an object, while go mod download uses a string.
	var message string
	if json.Unmarshal(m.Error, &message) == nil {
		return message
	}
	var detail struct{ Err string }
	if json.Unmarshal(m.Error, &detail) == nil && detail.Err != "" {
		return detail.Err
	}
	return "unrecognized Go command error"
}

func commandResponse(req *http.Request, err error) (*http.Response, error) {
	var commandErr *commandError
	if errors.As(err, &commandErr) && missingModule(commandErr.message) {
		return textResponse(req, http.StatusNotFound, "private module or version not found\n"), nil
	}
	// Operational and authentication failures must not become 404s: Go may
	// otherwise continue resolving a different package prefix after a failure.
	return nil, err
}

func missingModule(message string) bool {
	message = strings.ToLower(message)
	for _, marker := range []string{"authentication", "permission denied", "could not read username", "could not read password", "terminal prompts disabled", "unauthorized", "forbidden"} {
		if strings.Contains(message, marker) {
			return false
		}
	}
	for _, marker := range []string{"unknown revision ", "no matching versions for query ", "invalid github.com import path ", "no go.mod file", "missing go.mod", "404 not found"} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	if strings.Contains(message, "missing ") && strings.Contains(message, "/go.mod at revision ") {
		return true
	}
	return false
}

func textResponse(req *http.Request, status int, body string) *http.Response {
	return response(req, status, "text/plain; charset=utf-8", io.NopCloser(strings.NewReader(body)), int64(len(body)))
}

func response(req *http.Request, status int, contentType string, body io.ReadCloser, length int64) *http.Response {
	return &http.Response{
		Status:        strconv.Itoa(status) + " " + http.StatusText(status),
		StatusCode:    status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{"Content-Type": []string{contentType}},
		Body:          body,
		ContentLength: length,
		Request:       req,
	}
}
