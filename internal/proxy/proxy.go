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

// exchange is one HTTP request the proxy is answering.
type exchange struct {
	server *Server
	w      http.ResponseWriter
	r      *http.Request
}

// requestedVersion is the version a .info body must report back, or "" when
// any canonical version is acceptable.
type requestedVersion string

// pathSuffix is a GOPROXY endpoint suffix such as /@v/list or /@latest.
type pathSuffix string

const (
	listSuffix   pathSuffix = "/@v/list"
	latestSuffix pathSuffix = "/@latest"
)

// upstreamResponse is a complete upstream reply the proxy may forward.
type upstreamResponse struct {
	body        []byte
	status      int
	contentType string
}

// moduleInfo is validated .info metadata together with the module it describes.
type moduleInfo struct {
	module module.Version
	time   time.Time
}

// moduleVersions is a module path with the raw version list discovered for it.
type moduleVersions struct {
	path     string
	versions []string
}

// compatibleVersion is the highest non-+incompatible version seen for a module.
type compatibleVersion string

// keptVersions is the surviving version list for one module path, paired with
// the highest compatible version observed while filtering it.
type keptVersions struct {
	path       string
	versions   []string
	compatible compatibleVersion
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

// callWait is the outcome of waiting for an in-flight upstream call. retry
// reports that the caller must re-check the cache instead of using result.
type callWait[T any] struct {
	retry  bool
	result T
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
	module module.Version
	status int
}

// errFallbackToList signals that the caller must fall back to @v/list instead.
var errFallbackToList = errors.New("fall back to @v/list")

func (e *infoStatusError) Error() string {
	return fmt.Sprintf("upstream .info for %s@%s returned %d", e.module.Path, e.module.Version, e.status)
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

// at binds this .info result to the module path it was fetched for.
func (info VersionInfo) at(path string) moduleInfo {
	return moduleInfo{module: module.Version{Path: path, Version: info.Version}, time: info.Time}
}

// query turns the metadata into the availability question for this version.
func (m moduleInfo) query() availability.Query {
	return availability.Query{Module: m.module, CommitTime: m.time}
}

// info restates the metadata without the module path.
func (m moduleInfo) info() VersionInfo {
	return VersionInfo{Version: m.module.Version, Time: m.time}
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

// ServeHTTP keeps the two parameters required by http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "only GET is supported", http.StatusMethodNotAllowed)
		return
	}
	ex := exchange{server: s, w: w, r: r}
	// net/http decodes URL escaping in Path. Real Go clients percent-encode the
	// exclamation marks used by the module proxy protocol for uppercase letters.
	if mod := listSuffix.moduleFor(r.URL.Path); mod != "" {
		ex.handleList(r.Context(), mod)
		return
	}
	if mod := latestSuffix.moduleFor(r.URL.Path); mod != "" {
		ex.handleLatest(r.Context(), mod)
		return
	}
	ex.passthrough(r.Context())
}

// moduleFor returns the module path addressed by rawPath, or "" when rawPath does not address this suffix.
func (suffix pathSuffix) moduleFor(rawPath string) string {
	trimmed, ok := strings.CutSuffix(rawPath, string(suffix))
	if !ok {
		return ""
	}
	escaped := strings.TrimPrefix(trimmed, "/")
	if escaped == "" {
		return ""
	}
	path, err := module.UnescapePath(escaped)
	if err != nil {
		return ""
	}
	return path
}

// fetchDiscovery fetches the upstream discovery response for path.
// It returns nil once the request cannot be handled further,
// having already written the error or passthrough response itself.
func (ex exchange) fetchDiscovery(ctx context.Context, path string) *upstreamResponse {
	resp, err := ex.server.fetch(ctx, ex.r.URL.EscapedPath())
	if err != nil {
		ex.badGateway(err)
		return nil
	}
	if resp.status >= http.StatusMultipleChoices && resp.status < http.StatusBadRequest {
		ex.badGateway(fmt.Errorf("upstream redirected discovery request for %s", path))
		return nil
	}
	if resp.status != http.StatusOK {
		ex.writeUpstream(resp)
		return nil
	}

	return &resp
}

func (ex exchange) handleList(ctx context.Context, path string) {
	discovery := ex.fetchDiscovery(ctx, path)
	if discovery == nil {
		return
	}
	kept, err := ex.server.filter(ctx, moduleVersions{path: path, versions: parseList(discovery.body)})
	if err != nil {
		ex.badGateway(err)
		return
	}
	ex.w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	// GOPROXY list responses are plain text, not browser-rendered HTML.
	_, _ = io.WriteString(ex.w, strings.Join(kept, "\n"))
	if len(kept) > 0 {
		_, _ = io.WriteString(ex.w, "\n")
	}
}

func (ex exchange) handleLatest(ctx context.Context, path string) {
	discovery := ex.fetchDiscovery(ctx, path)
	if discovery == nil {
		return
	}
	latest, err := requestedVersion("").validateInfo(discovery.body)
	if err != nil {
		ex.badGateway(fmt.Errorf("invalid upstream latest for %s: %w", path, err))
		return
	}

	resolved, err := ex.server.resolveLatestTag(ctx, latest.at(path))
	if err != nil {
		if errors.Is(err, errFallbackToList) {
			ex.handleLatestFallback(ctx, path)
			return
		}

		ex.badGateway(err)
		return
	}

	latest = resolved

	allowed, err := ex.server.allowed(latest.at(path))
	if err != nil {
		ex.badGateway(err)
		return
	}
	incompatibleTag := strings.HasSuffix(latest.Version, "+incompatible") && !module.IsPseudoVersion(latest.Version)
	if allowed && !incompatibleTag {
		ex.writeUpstream(*discovery)
		return
	}
	// A compatible version hidden by the cooldown can otherwise make an older,
	// semantically higher +incompatible tag appear to be the latest version.
	// Pseudo-versions are absent from @v/list and must continue through @latest.
	// Reconcile such tags with the filtered list and the module-awareness check.
	ex.handleLatestFallback(ctx, path)
}

// resolveLatestTag reconciles a pseudo-version-free @latest with its tagged .info.
func (s *Server) resolveLatestTag(ctx context.Context, latest moduleInfo) (VersionInfo, error) {
	if module.IsPseudoVersion(latest.module.Version) {
		return latest.info(), nil
	}

	tagInfo, err := s.info(ctx, latest.module)
	if err != nil {
		if unavailableInfo(err) {
			return VersionInfo{}, errFallbackToList
		}

		return VersionInfo{}, err
	}

	if !tagInfo.Time.Equal(latest.time) {
		return VersionInfo{}, fmt.Errorf("inconsistent upstream latest for %s@%s", latest.module.Path, latest.module.Version)
	}

	return tagInfo, nil
}

func (ex exchange) handleLatestFallback(ctx context.Context, path string) {
	listPath, err := listSuffix.endpoint(path)
	if err != nil {
		ex.badGateway(err)
		return
	}
	list, err := ex.server.fetch(ctx, listPath)
	if err != nil {
		ex.badGateway(err)
		return
	}
	if list.status >= http.StatusMultipleChoices && list.status < http.StatusBadRequest {
		ex.badGateway(fmt.Errorf("upstream redirected discovery request for %s", path))
		return
	}
	if list.status != http.StatusOK {
		ex.writeUpstream(upstreamResponse{body: list.body, status: list.status, contentType: "text/plain; charset=utf-8"})
		return
	}
	kept, err := ex.server.filter(ctx, moduleVersions{path: path, versions: parseList(list.body)})
	if err != nil {
		ex.badGateway(err)
		return
	}
	chosen := chooseVersion(kept)
	if chosen == "" {
		http.NotFound(ex.w, ex.r)
		return
	}
	info, err := ex.server.info(ctx, module.Version{Path: path, Version: chosen})
	if err != nil {
		ex.badGateway(err)
		return
	}
	response, err := marshalInfo(info)
	if err != nil {
		ex.badGateway(fmt.Errorf("encode fallback .info for %s@%s: %w", path, chosen, err))
		return
	}
	ex.w.Header().Set("Content-Type", "application/json")
	// GOPROXY .info responses are JSON, not browser-rendered HTML.
	_, _ = ex.w.Write(response)
}

func (s *Server) filter(ctx context.Context, list moduleVersions) ([]string, error) {
	candidates := make([]string, 0, len(list.versions))
	for _, version := range list.versions {
		if module.IsPseudoVersion(version) {
			continue
		} // list must never gain pseudo-versions.
		if !canonical(version) {
			return nil, fmt.Errorf("invalid version %q in upstream list for %s", version, list.path)
		}
		candidates = append(candidates, version)
	}

	kept := keptVersions{path: list.path, versions: make([]string, 0, len(candidates))}
	latestCompatibleInCooldown := false
	for _, version := range candidates {
		info, err := s.info(ctx, module.Version{Path: list.path, Version: version})
		if err != nil {
			if unavailableInfo(err) {
				continue
			}
			return nil, err
		}
		ok, err := s.allowed(info.at(list.path))
		if err != nil {
			return nil, err
		}
		if !strings.HasSuffix(version, "+incompatible") &&
			(kept.compatible == "" || semver.Compare(version, string(kept.compatible)) > 0) {
			kept.compatible = compatibleVersion(version)
			latestCompatibleInCooldown = !ok
		}
		if ok {
			kept.versions = append(kept.versions, version)
		}
	}

	if !latestCompatibleInCooldown || !kept.containsHigherIncompatible() {
		return kept.versions, nil
	}

	aware, err := s.moduleAware(ctx, module.Version{Path: list.path, Version: string(kept.compatible)})
	if err != nil {
		return nil, err
	}

	if !aware {
		return kept.versions, nil
	}

	return s.excludeHigherIncompatible(kept), nil
}

func (s *Server) excludeHigherIncompatible(kept keptVersions) []string {
	filtered := kept.versions[:0]

	for _, version := range kept.versions {
		if kept.compatible.higherIncompatible(version) {
			s.logger.Printf("excluded module=%s version=%s reason=module-aware-compatible-version compatible_version=%s", kept.path, version, kept.compatible)
			continue
		}

		filtered = append(filtered, version)
	}

	return filtered
}

func (k keptVersions) containsHigherIncompatible() bool {
	return slices.ContainsFunc(k.versions, k.compatible.higherIncompatible)
}

func (compatible compatibleVersion) higherIncompatible(version string) bool {
	return strings.HasSuffix(version, "+incompatible") && semver.Compare(version, string(compatible)) > 0
}

func (s *Server) allowed(m moduleInfo) (bool, error) {
	a, err := s.source.AvailableAt(m.query())
	if err != nil {
		return false, fmt.Errorf("availability time for %s@%s: %w", m.module.Path, m.module.Version, err)
	}
	cutoff := s.now().Add(-s.cooldown)
	ok := a.AvailableAt.Before(cutoff) || a.AvailableAt.Equal(cutoff)
	if !ok {
		first := ""
		if a.FirstCached != nil {
			first = a.FirstCached.Format(time.RFC3339)
		}
		s.logger.Printf("excluded module=%s version=%s commit_time=%s first_cached_time=%s available_at=%s cutoff=%s", m.module.Path, m.module.Version, a.CommitTime.Format(time.RFC3339), first, a.AvailableAt.Format(time.RFC3339), cutoff.Format(time.RFC3339))
	} else if s.verbose {
		s.logger.Printf("allowed module=%s version=%s available_at=%s", m.module.Path, m.module.Version, a.AvailableAt.Format(time.RFC3339))
	}
	return ok, nil
}

func (s *Server) info(ctx context.Context, mod module.Version) (VersionInfo, error) {
	key := availability.Key(mod)
	for {
		s.cacheMu.Lock()
		c, ok := s.infos[key]
		if ok {
			s.cacheMu.Unlock()
			return c.info, c.err
		}
		if call, exists := s.inflight[key]; exists {
			s.cacheMu.Unlock()

			wait, err := call.wait(ctx, mod)
			if wait.retry {
				continue
			}
			if err != nil {
				return VersionInfo{}, err
			}

			return wait.result.info, wait.result.err
		}
		call := &infoCall{done: make(chan struct{})}
		s.inflight[key] = call
		s.cacheMu.Unlock()

		c = s.fetchInfo(ctx, mod)
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

// wait waits for an in-flight .info call. The returned error reports only that
// the wait itself failed, not an error carried by the cached result.
func (call *infoCall) wait(ctx context.Context, mod module.Version) (callWait[cachedInfo], error) {
	select {
	case <-call.done:
		if call.retryWaiters && ctx.Err() == nil {
			return callWait[cachedInfo]{retry: true}, nil
		}

		return callWait[cachedInfo]{result: call.result}, nil
	case <-ctx.Done():
		return callWait[cachedInfo]{}, fmt.Errorf("wait for .info for %s@%s: %w", mod.Path, mod.Version, ctx.Err())
	}
}

func (s *Server) fetchInfo(ctx context.Context, mod module.Version) cachedInfo {
	p, err := pathSuffix(".info").versionEndpoint(mod)
	if err != nil {
		return cachedInfo{err: err}
	}
	resp, err := s.fetch(ctx, p)
	if err == nil && resp.status != http.StatusOK {
		err = &infoStatusError{module: mod, status: resp.status}
	}
	var info VersionInfo
	if err == nil {
		info, err = requestedVersion(mod.Version).validateInfo(resp.body)
	}
	if err != nil {
		err = fmt.Errorf("get .info for %s@%s: %w", mod.Path, mod.Version, err)
	}
	return cachedInfo{info: info, err: err}
}

func (s *Server) moduleAware(ctx context.Context, mod module.Version) (bool, error) {
	key := availability.Key(mod)
	for {
		s.cacheMu.Lock()
		cached, ok := s.moduleAwareness[key]
		if ok {
			s.cacheMu.Unlock()
			return cached.aware, cached.err
		}
		if call, exists := s.awarenessInflight[key]; exists {
			s.cacheMu.Unlock()

			wait, err := call.wait(ctx, mod)
			if wait.retry {
				continue
			}
			if err != nil {
				return false, err
			}

			return wait.result.aware, wait.result.err
		}
		call := &moduleAwarenessCall{done: make(chan struct{})}
		s.awarenessInflight[key] = call
		s.cacheMu.Unlock()

		cached = s.fetchModuleAwareness(ctx, mod)
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

// wait waits for an in-flight .mod call. The returned error reports only that
// the wait itself failed, not an error carried by the cached result.
func (call *moduleAwarenessCall) wait(ctx context.Context, mod module.Version) (callWait[cachedModuleAwareness], error) {
	select {
	case <-call.done:
		if call.retryWaiters && ctx.Err() == nil {
			return callWait[cachedModuleAwareness]{retry: true}, nil
		}

		return callWait[cachedModuleAwareness]{result: call.result}, nil
	case <-ctx.Done():
		return callWait[cachedModuleAwareness]{}, fmt.Errorf("wait for .mod for %s@%s: %w", mod.Path, mod.Version, ctx.Err())
	}
}

func (s *Server) fetchModuleAwareness(ctx context.Context, mod module.Version) cachedModuleAwareness {
	p, err := pathSuffix(".mod").versionEndpoint(mod)
	if err != nil {
		return cachedModuleAwareness{err: err}
	}
	resp, err := s.fetch(ctx, p)
	if err == nil && resp.status != http.StatusOK {
		err = fmt.Errorf("upstream .mod for %s@%s returned %d", mod.Path, mod.Version, resp.status)
	}
	if err != nil {
		return cachedModuleAwareness{err: fmt.Errorf("get .mod for %s@%s: %w", mod.Path, mod.Version, err)}
	}
	legacy := fmt.Appendf(nil, "module %s\n", modfile.AutoQuote(mod.Path))
	return cachedModuleAwareness{aware: !bytes.Equal(resp.body, legacy)}
}

func (suffix pathSuffix) endpoint(path string) (string, error) {
	escaped, err := module.EscapePath(path)
	if err != nil {
		return "", fmt.Errorf("escape module path %q: %w", path, err)
	}
	return "/" + escaped + string(suffix), nil
}

func (suffix pathSuffix) versionEndpoint(mod module.Version) (string, error) {
	escaped, err := module.EscapeVersion(mod.Version)
	if err != nil {
		return "", fmt.Errorf("escape module version %q: %w", mod.Version, err)
	}
	return pathSuffix("/@v/" + escaped + string(suffix)).endpoint(mod.Path)
}

func (s *Server) fetch(ctx context.Context, rawPath string) (upstreamResponse, error) {
	target, err := s.upstreamURL(rawPath)
	if err != nil {
		return upstreamResponse{}, err
	}
	//nolint:gosec // target is constructed from the validated fixed upstream URL.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return upstreamResponse{}, fmt.Errorf("create upstream request: %w", err)
	}
	if s.verbose {
		s.logger.Printf("upstream GET %s", target)
	}
	//nolint:gosec // req targets only the validated configured upstream.
	resp, err := s.client.Do(req)
	if err != nil {
		return upstreamResponse{}, fmt.Errorf("upstream request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return upstreamResponse{}, fmt.Errorf("read upstream response: %w", err)
	}
	return upstreamResponse{body: body, status: resp.StatusCode, contentType: resp.Header.Get("Content-Type")}, nil
}

func (ex exchange) passthrough(ctx context.Context) {
	target, err := ex.server.upstreamURL(ex.r.URL.EscapedPath())
	if err != nil {
		ex.badGateway(err)
		return
	}
	// target is constructed from the validated fixed upstream URL.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		ex.badGateway(err)
		return
	}
	// req targets only the validated configured upstream.
	resp, err := ex.server.client.Do(req)
	if err != nil {
		ex.badGateway(fmt.Errorf("upstream request: %w", err))
		return
	}
	defer func() { _ = resp.Body.Close() }()
	ex.copyHeaders(resp.Header)
	ex.w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(ex.w, resp.Body)
}

func (ex exchange) writeUpstream(resp upstreamResponse) {
	if resp.contentType != "" {
		ex.w.Header().Set("Content-Type", resp.contentType)
	}

	ex.w.WriteHeader(resp.status)
	// The proxy forwards module-protocol bytes, not HTML for browsers.
	_, _ = ex.w.Write(resp.body)
}

func (ex exchange) badGateway(err error) {
	ex.server.logger.Printf("proxy error: %v", err)
	http.Error(ex.w, "gomod-cooldown: "+err.Error(), http.StatusBadGateway)
}

func (ex exchange) copyHeaders(src http.Header) {
	dst := ex.w.Header()

	for key, values := range src {
		if isHopHeader(key) {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
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

func (requested requestedVersion) validateInfo(body []byte) (VersionInfo, error) {
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
	if requested != "" && *raw.Version != string(requested) {
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

func isHopHeader(key string) bool {
	switch strings.ToLower(key) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}
