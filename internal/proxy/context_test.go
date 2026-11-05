package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dev-hato/gomod-cooldown/internal/availability"
)

type requestContextKey struct{}

type contextRoundTripper func(*http.Request) (*http.Response, error)

func (f contextRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type contextResponse struct {
	path string
	body string
}

type contextRoute struct {
	name     string
	path     string
	steps    []contextResponse
	wantBody string
	cancelAt string
}

func TestServeHTTPPropagatesRequestContext(t *testing.T) {
	oldInfo := info("v1.0.0", now.Add(-30*24*time.Hour))
	recentInfo := info("v1.1.0", now)
	routes := []contextRoute{
		{
			name: "list", path: "/example.com/m/@v/list", wantBody: "v1.0.0\n",
			steps: []contextResponse{
				{path: "/example.com/m/@v/list", body: "v1.0.0\n"},
				{path: "/example.com/m/@v/v1.0.0.info", body: oldInfo},
			},
		},
		{
			name: "latest", path: "/example.com/m/@latest", wantBody: oldInfo,
			steps: []contextResponse{
				{path: "/example.com/m/@latest", body: oldInfo},
				{path: "/example.com/m/@v/v1.0.0.info", body: oldInfo},
			},
		},
		{
			name: "latest fallback", path: "/example.com/m/@latest", wantBody: oldInfo,
			steps: []contextResponse{
				{path: "/example.com/m/@latest", body: recentInfo},
				{path: "/example.com/m/@v/v1.1.0.info", body: recentInfo},
				{path: "/example.com/m/@v/list", body: "v1.0.0\n"},
				{path: "/example.com/m/@v/v1.0.0.info", body: oldInfo},
			},
		},
		{
			name: "passthrough", path: "/example.com/m/@v/v1.0.0.zip", wantBody: "zip",
			steps: []contextResponse{{path: "/example.com/m/@v/v1.0.0.zip", body: "zip"}},
		},
	}
	for _, route := range routes {
		t.Run(route.name+"/values", route.check)
		for _, step := range route.steps {
			route.cancelAt = step.path
			t.Run(route.name+"/cancel"+step.path, route.check)
		}
	}
}

func (route contextRoute) check(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.WithValue(t.Context(), requestContextKey{}, "request value"))
	defer cancel()
	calls := 0
	transport := contextRoundTripper(func(r *http.Request) (*http.Response, error) {
		if calls >= len(route.steps) || r.URL.Path != route.steps[calls].path {
			t.Fatalf("unexpected upstream request %d: %s", calls, r.URL.Path)
		}
		step := route.steps[calls]
		calls++
		if got := r.Context().Value(requestContextKey{}); got != "request value" {
			t.Errorf("upstream %s context value = %v", r.URL.Path, got)
		}
		if r.URL.Path == route.cancelAt {
			cancel()
			select {
			case <-r.Context().Done():
			default:
				t.Fatalf("upstream %s context was not canceled", r.URL.Path)
			}
			return nil, fmt.Errorf("upstream request canceled: %w", r.Context().Err())
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(step.body)),
		}, nil
	})
	s, err := New(Config{
		Upstream: "https://proxy.example", Client: &http.Client{Transport: transport},
		Source: availability.CommitTimeSource{}, Cooldown: 14 * 24 * time.Hour,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRecorder()
	s.ServeHTTP(r, httptest.NewRequestWithContext(ctx, http.MethodGet, route.path, nil))
	if route.cancelAt != "" {
		if r.Code != http.StatusBadGateway || !strings.Contains(r.Body.String(), context.Canceled.Error()) {
			t.Fatalf("canceled request: status=%d body=%q", r.Code, r.Body.String())
		}
		return
	}
	if r.Code != http.StatusOK || r.Body.String() != route.wantBody || calls != len(route.steps) {
		t.Fatalf("status=%d body=%q upstream calls=%d", r.Code, r.Body.String(), calls)
	}
}
