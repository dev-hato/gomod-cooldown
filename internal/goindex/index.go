// Package goindex downloads a complete recent snapshot of index.golang.org.
package goindex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/dev-hato/gomod-cooldown/internal/availability"
	"golang.org/x/mod/module"
)

const recordsLimit = 2000

// Record is one timestamped module-version entry from the index feed.
type Record struct {
	Path      string
	Version   string
	Timestamp time.Time
}

// CooldownSnapshot is a recent-version snapshot together with the exact cutoff
// it was taken for.
type CooldownSnapshot struct {
	Recent map[string]time.Time
	Cutoff time.Time
}

// Fetcher reads the chronological NDJSON feed. Index's documented ordering is
// essential: when a page has fewer than limit records, the snapshot is complete.
type Fetcher struct {
	BaseURL string
	Client  *http.Client
	Now     func() time.Time
}

// SnapshotForCooldown derives a cutoff from the injectable clock and returns
// the exact cutoff used. Callers should use that same cutoff clock for later
// decisions so the snapshot and proxy cannot drift apart. The returned snapshot
// is meaningful only when the error is nil.
func (f Fetcher) SnapshotForCooldown(ctx context.Context, cooldown time.Duration) (CooldownSnapshot, error) {
	now := f.Now
	if now == nil {
		now = time.Now
	}
	cutoff := now().Add(-cooldown)

	recent, err := f.Snapshot(ctx, cutoff)
	if err != nil {
		return CooldownSnapshot{}, err
	}

	return CooldownSnapshot{Recent: recent, Cutoff: cutoff}, nil
}

// Snapshot returns every unique module version first cached at or after cutoff.
func (f Fetcher) Snapshot(ctx context.Context, cutoff time.Time) (map[string]time.Time, error) {
	feed, err := f.feed()
	if err != nil {
		return nil, err
	}
	// Start one nanosecond before the cutoff so a record exactly at the boundary
	// cannot be lost if the service treats since as exclusive.
	cursor := cutoff.Add(-time.Nanosecond).UTC()
	snapshot := recentVersions{recent: make(map[string]time.Time), cutoff: cutoff}
	for {
		records, err := feed.fetchRecords(ctx, cursor)
		if err != nil {
			return nil, err
		}
		last, err := snapshot.add(records)
		if err != nil {
			return nil, err
		}
		if len(records) < recordsLimit {
			return snapshot.recent, nil
		}
		if last.IsZero() || last.Before(cursor) || last.Equal(cursor) {
			return nil, fmt.Errorf("index cursor did not advance from %s", cursor.Format(time.RFC3339Nano))
		}
		cursor = last
	}
}

// indexFeed is a validated index endpoint together with the client reading it.
type indexFeed struct {
	base   *url.URL
	client *http.Client
}

func (f Fetcher) feed() (indexFeed, error) {
	base := f.BaseURL
	if base == "" {
		base = "https://index.golang.org"
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return indexFeed{}, fmt.Errorf("invalid index URL %q", base)
	}
	client := f.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	clientCopy := *client
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return indexFeed{base: u, client: &clientCopy}, nil
}

func (f indexFeed) fetchRecords(ctx context.Context, cursor time.Time) ([]Record, error) {
	q := url.Values{"since": []string{cursor.Format(time.RFC3339Nano)}, "limit": []string{strconv.Itoa(recordsLimit)}}
	reqURL := f.base.JoinPath("index")
	reqURL.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create index request: %w", err)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch index: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("index returned %s", resp.Status)
	}
	return decodePage(resp.Body)
}

// recentVersions accumulates the earliest first-cached time per module version
// for records at or after cutoff.
type recentVersions struct {
	recent map[string]time.Time
	cutoff time.Time
}

// add merges one page of records and returns the timestamp of its last record at or after the cutoff,
// which becomes the cursor for the next page.
// Only the first page can contain pre-cutoff records, because the cursor starts one nanosecond early;
// if such a page held nothing but those,
// the returned zero timestamp makes the caller fail closed instead of advancing the cursor.
func (v recentVersions) add(records []Record) (time.Time, error) {
	var last time.Time
	for _, r := range records {
		if r.Path == "" || r.Version == "" || r.Timestamp.IsZero() {
			return time.Time{}, errors.New("invalid index record")
		}
		if r.Timestamp.Before(v.cutoff) {
			continue
		}
		key := availability.Key(module.Version{Path: r.Path, Version: r.Version})
		if old, ok := v.recent[key]; !ok || r.Timestamp.Before(old) {
			v.recent[key] = r.Timestamp
		}
		last = r.Timestamp
	}
	return last, nil
}

func decodePage(body interface {
	Read(p []byte) (n int, err error)
}) ([]Record, error) {
	s := bufio.NewScanner(body)
	// Index records are small, but do not make a silently small Scanner limit.
	s.Buffer(make([]byte, 4096), 1024*1024)
	var records []Record
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" {
			return nil, errors.New("invalid empty index record")
		}
		var r Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return nil, fmt.Errorf("decode index record: %w", err)
		}
		records = append(records, r)
	}
	if err := s.Err(); err != nil {
		return nil, fmt.Errorf("read index response: %w", err)
	}
	return records, nil
}
