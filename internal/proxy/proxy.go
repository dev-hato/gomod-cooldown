// Package proxy implements the small, temporary GOPROXY used by the CLI.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/dev-hato/gomod-cooldown/internal/availability"
	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

// Config configures a temporary GOPROXY server.
type Config struct {
	Upstream string
	Client   *http.Client
	Source   availability.Source
	Cooldown time.Duration
	Now      func() time.Time
	Logger   *log.Logger
	Verbose  bool
}

// Server filters only GOPROXY discovery endpoints.
type Server struct {
	upstream *url.URL
	client   *http.Client
	source   availability.Source
	cooldown time.Duration
	now      func() time.Time
	logger   *log.Logger
	verbose  bool

	cacheMu           sync.Mutex
	infos             map[string]cachedInfo
	inflight          map[string]*infoCall
	moduleAwareness   map[string]cachedModuleAwareness
	awarenessInflight map[string]*moduleAwarenessCall
}

type cachedInfo struct {
	info VersionInfo
	err  error
}

type infoCall struct {
	done         chan struct{}
	result       cachedInfo
	retryWaiters bool
}

type cachedModuleAwareness struct {
	aware bool
	err   error
}

type moduleAwarenessCall struct {
	done         chan struct{}
	result       cachedModuleAwareness
	retryWaiters bool
}

type infoStatusError struct {
	path    string
	version string
	status  int
}

// errFallbackToList signals that the caller must fall back to @v/list instead.
var errFallbackToList = errors.New("fall back to @v/list")

func (e *infoStatusError) Error() string {
	return fmt.Sprintf("upstream .info for %s@%s returned %d", e.path, e.version, e.status)
}

func unavailableInfo(err error) bool {
	var statusErr *infoStatusError
	return errors.As(err, &statusErr) && slices.Contains([]int{http.StatusNotFound, http.StatusGone}, statusErr.status)
}

// VersionInfo is validated metadata returned by a GOPROXY .info endpoint.
type VersionInfo struct {
	Version string
	Time    time.Time
}

// New validates configuration and creates a proxy server.
func New(cfg Config) (*Server, error) {
	if cfg.Cooldown <= 0 {
		return nil, errors.New("cooldown must be positive")
	}
	u, err := url.Parse(cfg.Upstream)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("invalid upstream URL %q", cfg.Upstream)
	}
	if cfg.Source == nil {
		return nil, errors.New("availability source is required")
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 30 * time.Second}
	}
	// Keep redirects as upstream responses: the proxy must not contact a host
	// chosen by an upstream Location header.
	client := *cfg.Client
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = log.New(io.Discard, "", 0)
	}
	return &Server{
		upstream:          u,
		client:            &client,
		source:            cfg.Source,
		cooldown:          cfg.Cooldown,
		now:               cfg.Now,
		logger:            cfg.Logger,
		verbose:           cfg.Verbose,
		infos:             make(map[string]cachedInfo),
		inflight:          make(map[string]*infoCall),
		moduleAwareness:   make(map[string]cachedModuleAwareness),
		awarenessInflight: make(map[string]*moduleAwarenessCall),
	}, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "only GET is supported", http.StatusMethodNotAllowed)
		return
	}
	// net/http decodes URL escaping in Path. Real Go clients percent-encode the
	// exclamation marks used by the module proxy protocol for uppercase letters.
	if mod, ok := moduleFor(r.URL.Path, "/@v/list"); ok {
		s.handleList(w, r, mod)
		return
	}
	if mod, ok := moduleFor(r.URL.Path, "/@latest"); ok {
		s.handleLatest(w, r, mod)
		return
	}
	s.passthrough(w, r)
}

func moduleFor(rawPath, suffix string) (string, bool) {
	trimmed, ok := strings.CutSuffix(rawPath, suffix)
	if !ok {
		return "", false
	}
	escaped := strings.TrimPrefix(trimmed, "/")
	if escaped == "" {
		return "", false
	}
	path, err := module.UnescapePath(escaped)
	if err != nil {
		return "", false
	}
	return path, true
}

// fetchDiscovery fetches the upstream discovery response for path,
// writing an error or passthrough response to w when the request cannot be handled further.
// The final return value reports whether the caller should continue processing body and contentType.
func (s *Server) fetchDiscovery(w http.ResponseWriter, r *http.Request, path string) ([]byte, string, bool) {
	body, status, contentType, err := s.fetch(r.Context(), r.URL.EscapedPath())
	if err != nil {
		s.badGateway(w, err)
		return nil, "", false
	}
	if status >= http.StatusMultipleChoices && status < http.StatusBadRequest {
		s.badGateway(w, fmt.Errorf("upstream redirected discovery request for %s", path))
		return nil, "", false
	}
	if status != http.StatusOK {
		s.writeUpstream(w, status, contentType, body)
		return nil, "", false
	}

	return body, contentType, true
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request, path string) {
	body, _, ok := s.fetchDiscovery(w, r, path)
	if !ok {
		return
	}
	versions := parseList(body)
	kept, err := s.filter(r.Context(), path, versions)
	if err != nil {
		s.badGateway(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	//nolint:gosec // GOPROXY list responses are plain text, not browser-rendered HTML.
	_, _ = io.WriteString(w, strings.Join(kept, "\n"))
	if len(kept) > 0 {
		_, _ = io.WriteString(w, "\n")
	}
}

func (s *Server) handleLatest(w http.ResponseWriter, r *http.Request, path string) {
	body, contentType, ok := s.fetchDiscovery(w, r, path)
	if !ok {
		return
	}
	latest, err := validateInfo(body, "")
	if err != nil {
		s.badGateway(w, fmt.Errorf("invalid upstream latest for %s: %w", path, err))
		return
	}

	resolved, err := s.resolveLatestTag(r.Context(), path, latest)
	if err != nil {
		if errors.Is(err, errFallbackToList) {
			s.handleLatestFallback(w, r, path)
			return
		}

		s.badGateway(w, err)
		return
	}

	latest = resolved

	allowed, err := s.allowed(r.Context(), path, latest)
	if err != nil {
		s.badGateway(w, err)
		return
	}
	incompatibleTag := strings.HasSuffix(latest.Version, "+incompatible") && !module.IsPseudoVersion(latest.Version)
	if allowed && !incompatibleTag {
		s.writeUpstream(w, http.StatusOK, contentType, body)
		return
	}
	// A compatible version hidden by the cooldown can otherwise make an older,
	// semantically higher +incompatible tag appear to be the latest version.
	// Pseudo-versions are absent from @v/list and must continue through @latest.
	// Reconcile such tags with the filtered list and the module-awareness check.
	s.handleLatestFallback(w, r, path)
}

// resolveLatestTag reconciles a pseudo-version-free @latest with its tagged .info.
func (s *Server) resolveLatestTag(ctx context.Context, path string, latest VersionInfo) (VersionInfo, error) {
	if module.IsPseudoVersion(latest.Version) {
		return latest, nil
	}

	tagInfo, err := s.info(ctx, path, latest.Version)
	if err != nil {
		if unavailableInfo(err) {
			return VersionInfo{}, errFallbackToList
		}

		return VersionInfo{}, err
	}

	if !tagInfo.Time.Equal(latest.Time) {
		return VersionInfo{}, fmt.Errorf("inconsistent upstream latest for %s@%s", path, latest.Version)
	}

	return tagInfo, nil
}

func (s *Server) handleLatestFallback(w http.ResponseWriter, r *http.Request, path string) {
	listPath, err := endpoint(path, "/@v/list")
	if err != nil {
		s.badGateway(w, err)
		return
	}
	listBody, listStatus, _, err := s.fetch(r.Context(), listPath)
	if err != nil {
		s.badGateway(w, err)
		return
	}
	if listStatus >= http.StatusMultipleChoices && listStatus < http.StatusBadRequest {
		s.badGateway(w, fmt.Errorf("upstream redirected discovery request for %s", path))
		return
	}
	if listStatus != http.StatusOK {
		s.writeUpstream(w, listStatus, "text/plain; charset=utf-8", listBody)
		return
	}
	versions := parseList(listBody)
	kept, err := s.filter(r.Context(), path, versions)
	if err != nil {
		s.badGateway(w, err)
		return
	}
	chosen := chooseVersion(kept)
	if chosen == "" {
		http.NotFound(w, r)
		return
	}
	info, err := s.info(r.Context(), path, chosen)
	if err != nil {
		s.badGateway(w, err)
		return
	}
	response, err := marshalInfo(info)
	if err != nil {
		s.badGateway(w, fmt.Errorf("encode fallback .info for %s@%s: %w", path, chosen, err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	//nolint:gosec // GOPROXY .info responses are JSON, not browser-rendered HTML.
	_, _ = w.Write(response)
}

func (s *Server) filter(ctx context.Context, path string, versions []string) ([]string, error) {
	candidates := make([]string, 0, len(versions))
	for _, version := range versions {
		if module.IsPseudoVersion(version) {
			continue
		} // list must never gain pseudo-versions.
		if !canonical(version) {
			return nil, fmt.Errorf("invalid version %q in upstream list for %s", version, path)
		}
		candidates = append(candidates, version)
	}

	kept := make([]string, 0, len(candidates))
	latestCompatible := ""
	latestCompatibleInCooldown := false
	for _, version := range candidates {
		info, err := s.info(ctx, path, version)
		if err != nil {
			if unavailableInfo(err) {
				continue
			}
			return nil, err
		}
		ok, err := s.allowed(ctx, path, info)
		if err != nil {
			return nil, err
		}
		if !strings.HasSuffix(version, "+incompatible") &&
			(latestCompatible == "" || semver.Compare(version, latestCompatible) > 0) {
			latestCompatible = version
			latestCompatibleInCooldown = !ok
		}
		if ok {
			kept = append(kept, version)
		}
	}

	if !latestCompatibleInCooldown || !containsHigherIncompatible(kept, latestCompatible) {
		return kept, nil
	}

	aware, err := s.moduleAware(ctx, path, latestCompatible)
	if err != nil {
		return nil, err
	}

	if !aware {
		return kept, nil
	}

	return s.excludeHigherIncompatible(path, kept, latestCompatible), nil
}

func (s *Server) excludeHigherIncompatible(path string, kept []string, compatible string) []string {
	filtered := kept[:0]

	for _, version := range kept {
		if higherIncompatible(version, compatible) {
			s.logger.Printf("excluded module=%s version=%s reason=module-aware-compatible-version compatible_version=%s", path, version, compatible)
			continue
		}

		filtered = append(filtered, version)
	}

	return filtered
}

func containsHigherIncompatible(versions []string, compatible string) bool {
	return slices.ContainsFunc(versions, func(version string) bool {
		return higherIncompatible(version, compatible)
	})
}

func higherIncompatible(version, compatible string) bool {
	return strings.HasSuffix(version, "+incompatible") && semver.Compare(version, compatible) > 0
}

func (s *Server) allowed(ctx context.Context, path string, info VersionInfo) (bool, error) {
	a, err := s.source.AvailableAt(ctx, path, info.Version, info.Time)
	if err != nil {
		return false, fmt.Errorf("availability time for %s@%s: %w", path, info.Version, err)
	}
	cutoff := s.now().Add(-s.cooldown)
	ok := a.AvailableAt.Before(cutoff) || a.AvailableAt.Equal(cutoff)
	if !ok {
		first := ""
		if a.FirstCached != nil {
			first = a.FirstCached.Format(time.RFC3339)
		}
		s.logger.Printf("excluded module=%s version=%s commit_time=%s first_cached_time=%s available_at=%s cutoff=%s", path, info.Version, a.CommitTime.Format(time.RFC3339), first, a.AvailableAt.Format(time.RFC3339), cutoff.Format(time.RFC3339))
	} else if s.verbose {
		s.logger.Printf("allowed module=%s version=%s available_at=%s", path, info.Version, a.AvailableAt.Format(time.RFC3339))
	}
	return ok, nil
}

func (s *Server) info(ctx context.Context, path, version string) (VersionInfo, error) {
	key := path + "\x00" + version
	for {
		s.cacheMu.Lock()
		c, ok := s.infos[key]
		if ok {
			s.cacheMu.Unlock()
			return c.info, c.err
		}
		if call, exists := s.inflight[key]; exists {
			s.cacheMu.Unlock()

			retry, result, err := waitForInfoCall(ctx, call, path, version)
			if retry {
				continue
			}
			if err != nil {
				return VersionInfo{}, err
			}

			return result.info, result.err
		}
		call := &infoCall{done: make(chan struct{})}
		s.inflight[key] = call
		s.cacheMu.Unlock()

		c = s.fetchInfo(ctx, path, version)
		s.cacheMu.Lock()
		if c.err == nil || unavailableInfo(c.err) {
			s.infos[key] = c
		}
		call.result = c
		call.retryWaiters = ctx.Err() != nil && errors.Is(c.err, ctx.Err())
		delete(s.inflight, key)
		close(call.done)
		s.cacheMu.Unlock()
		return c.info, c.err
	}
}

// waitForInfoCall waits for an in-flight .info call. retry reports that the caller must re-check the cache;
// err reports only that the wait itself failed, not an error carried by the cached result.
func waitForInfoCall(ctx context.Context, call *infoCall, path, version string) (bool, cachedInfo, error) {
	select {
	case <-call.done:
		if call.retryWaiters && ctx.Err() == nil {
			return true, cachedInfo{}, nil
		}

		return false, call.result, nil
	case <-ctx.Done():
		return false, cachedInfo{}, fmt.Errorf("wait for .info for %s@%s: %w", path, version, ctx.Err())
	}
}

func (s *Server) fetchInfo(ctx context.Context, path, version string) cachedInfo {
	p, err := versionEndpoint(path, version, ".info")
	if err != nil {
		return cachedInfo{err: err}
	}
	body, status, _, err := s.fetch(ctx, p)
	if err == nil && status != http.StatusOK {
		err = &infoStatusError{path: path, version: version, status: status}
	}
	var info VersionInfo
	if err == nil {
		info, err = validateInfo(body, version)
	}
	if err != nil {
		err = fmt.Errorf("get .info for %s@%s: %w", path, version, err)
	}
	return cachedInfo{info: info, err: err}
}

func (s *Server) moduleAware(ctx context.Context, path, version string) (bool, error) {
	key := path + "\x00" + version
	for {
		s.cacheMu.Lock()
		cached, ok := s.moduleAwareness[key]
		if ok {
			s.cacheMu.Unlock()
			return cached.aware, cached.err
		}
		if call, exists := s.awarenessInflight[key]; exists {
			s.cacheMu.Unlock()

			retry, result, err := waitForAwarenessCall(ctx, call, path, version)
			if retry {
				continue
			}
			if err != nil {
				return false, err
			}

			return result.aware, result.err
		}
		call := &moduleAwarenessCall{done: make(chan struct{})}
		s.awarenessInflight[key] = call
		s.cacheMu.Unlock()

		cached = s.fetchModuleAwareness(ctx, path, version)
		s.cacheMu.Lock()
		if cached.err == nil {
			s.moduleAwareness[key] = cached
		}
		call.result = cached
		call.retryWaiters = ctx.Err() != nil && errors.Is(cached.err, ctx.Err())
		delete(s.awarenessInflight, key)
		close(call.done)
		s.cacheMu.Unlock()
		return cached.aware, cached.err
	}
}

// waitForAwarenessCall waits for an in-flight .mod call. retry reports that the caller must re-check the cache;
// err reports only that the wait itself failed, not an error carried by the cached result.
func waitForAwarenessCall(ctx context.Context, call *moduleAwarenessCall, path, version string) (bool, cachedModuleAwareness, error) {
	select {
	case <-call.done:
		if call.retryWaiters && ctx.Err() == nil {
			return true, cachedModuleAwareness{}, nil
		}

		return false, call.result, nil
	case <-ctx.Done():
		return false, cachedModuleAwareness{}, fmt.Errorf("wait for .mod for %s@%s: %w", path, version, ctx.Err())
	}
}

func (s *Server) fetchModuleAwareness(ctx context.Context, path, version string) cachedModuleAwareness {
	p, err := versionEndpoint(path, version, ".mod")
	if err != nil {
		return cachedModuleAwareness{err: err}
	}
	body, status, _, err := s.fetch(ctx, p)
	if err == nil && status != http.StatusOK {
		err = fmt.Errorf("upstream .mod for %s@%s returned %d", path, version, status)
	}
	if err != nil {
		return cachedModuleAwareness{err: fmt.Errorf("get .mod for %s@%s: %w", path, version, err)}
	}
	legacy := fmt.Appendf(nil, "module %s\n", modfile.AutoQuote(path))
	return cachedModuleAwareness{aware: !bytes.Equal(body, legacy)}
}

func endpoint(path, suffix string) (string, error) {
	escaped, err := module.EscapePath(path)
	if err != nil {
		return "", fmt.Errorf("escape module path %q: %w", path, err)
	}
	return "/" + escaped + suffix, nil
}

func versionEndpoint(path, version, suffix string) (string, error) {
	escaped, err := module.EscapeVersion(version)
	if err != nil {
		return "", fmt.Errorf("escape module version %q: %w", version, err)
	}
	return endpoint(path, "/@v/"+escaped+suffix)
}

func (s *Server) fetch(ctx context.Context, rawPath string) ([]byte, int, string, error) {
	target, err := s.upstreamURL(rawPath)
	if err != nil {
		return nil, 0, "", err
	}
	//nolint:gosec // target is constructed from the validated fixed upstream URL.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, 0, "", fmt.Errorf("create upstream request: %w", err)
	}
	if s.verbose {
		s.logger.Printf("upstream GET %s", target)
	}
	//nolint:gosec // req targets only the validated configured upstream.
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, 0, "", fmt.Errorf("upstream request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, "", fmt.Errorf("read upstream response: %w", err)
	}
	return body, resp.StatusCode, resp.Header.Get("Content-Type"), nil
}

func (s *Server) passthrough(w http.ResponseWriter, r *http.Request) {
	target, err := s.upstreamURL(r.URL.EscapedPath())
	if err != nil {
		s.badGateway(w, err)
		return
	}
	//nolint:gosec // target is constructed from the validated fixed upstream URL.
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		s.badGateway(w, err)
		return
	}
	//nolint:gosec // req targets only the validated configured upstream.
	resp, err := s.client.Do(req)
	if err != nil {
		s.badGateway(w, fmt.Errorf("upstream request: %w", err))
		return
	}
	defer func() { _ = resp.Body.Close() }()
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (s *Server) writeUpstream(w http.ResponseWriter, status int, contentType string, body []byte) {
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.WriteHeader(status)
	//nolint:gosec // The proxy forwards module-protocol bytes, not HTML for browsers.
	_, _ = w.Write(body)
}
func (s *Server) badGateway(w http.ResponseWriter, err error) {
	s.logger.Printf("proxy error: %v", err)
	http.Error(w, "gomod-cooldown: "+err.Error(), http.StatusBadGateway)
}

func parseList(body []byte) []string {
	lines := strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n")
	versions := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) != "" {
			// Preserve non-empty input verbatim. Whitespace in a version is invalid
			// and must cause a 502 instead of being silently normalized.
			versions = append(versions, line)
		}
	}
	return versions
}

func canonical(v string) bool { return semver.IsValid(v) && module.CanonicalVersion(v) == v }

func validateInfo(body []byte, requested string) (VersionInfo, error) {
	var raw struct {
		Version *string          `json:"Version"`
		Time    *json.RawMessage `json:"Time"`
	}
	d := json.NewDecoder(bytes.NewReader(body))
	if err := d.Decode(&raw); err != nil {
		return VersionInfo{}, fmt.Errorf("invalid JSON: %w", err)
	}
	if d.Decode(&struct{}{}) != io.EOF {
		return VersionInfo{}, errors.New("invalid JSON: trailing value")
	}
	if raw.Version == nil || *raw.Version == "" {
		return VersionInfo{}, errors.New("missing Version")
	}
	if !canonical(*raw.Version) {
		return VersionInfo{}, fmt.Errorf("non-canonical Version %q", *raw.Version)
	}
	if requested != "" && *raw.Version != requested {
		return VersionInfo{}, fmt.Errorf("version %q does not match requested %q", *raw.Version, requested)
	}
	if raw.Time == nil || string(bytes.TrimSpace(*raw.Time)) == "null" {
		return VersionInfo{}, errors.New("missing Time")
	}
	var stamp string
	if err := json.Unmarshal(*raw.Time, &stamp); err != nil {
		return VersionInfo{}, fmt.Errorf("invalid Time: %w", err)
	}

	t, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return VersionInfo{}, fmt.Errorf("invalid Time: %w", err)
	}

	if t.IsZero() {
		return VersionInfo{}, errors.New("zero Time")
	}
	return VersionInfo{Version: *raw.Version, Time: t}, nil
}

func chooseVersion(versions []string) string {
	bestRelease, bestPre := "", ""
	for _, v := range versions {
		if !canonical(v) || module.IsPseudoVersion(v) {
			continue
		}
		if semver.Prerelease(v) == "" {
			if bestRelease == "" || semver.Compare(v, bestRelease) > 0 {
				bestRelease = v
			}
		} else if bestPre == "" || semver.Compare(v, bestPre) > 0 {
			bestPre = v
		}
	}
	if bestRelease != "" {
		return bestRelease
	}
	return bestPre
}

func marshalInfo(info VersionInfo) ([]byte, error) {
	result, err := json.Marshal(struct {
		Version string    `json:"Version"`
		Time    time.Time `json:"Time"`
	}{info.Version, info.Time})
	if err != nil {
		return nil, fmt.Errorf("marshal .info: %w", err)
	}
	return result, nil
}

func (s *Server) upstreamURL(rawPath string) (string, error) {
	decoded, err := url.PathUnescape(rawPath)
	if err != nil {
		return "", fmt.Errorf("invalid request path: %w", err)
	}
	target := *s.upstream
	target.Path = strings.TrimRight(s.upstream.Path, "/") + decoded
	target.RawPath = strings.TrimRight(s.upstream.EscapedPath(), "/") + rawPath
	target.RawQuery = ""
	return target.String(), nil
}

func copyHeaders(dst, src http.Header) {
	for key, values := range src {
		if isHopHeader(key) {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func isHopHeader(key string) bool {
	switch strings.ToLower(key) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade": // codespell:ignore te
		return true
	default:
		return false
	}
}
