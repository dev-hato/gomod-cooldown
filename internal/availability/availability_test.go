package availability

import (
	"testing"
	"time"

	"golang.org/x/mod/module"
)

func TestGoIndexSource(t *testing.T) {
	cutoff := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cached := module.Version{Path: "example.com/m", Version: "v1.0.0"}
	s := GoIndexSource{Cutoff: cutoff, Recent: map[string]time.Time{Key(cached): cutoff.Add(time.Hour)}}
	a, err := s.AvailableAt(Query{Module: cached})
	if err != nil || a.FirstCached == nil || !a.AvailableAt.Equal(cutoff.Add(time.Hour)) {
		t.Fatal(a, err)
	}
	a, err = s.AvailableAt(Query{Module: module.Version{Path: "example.com/m", Version: "v0.9.0"}})
	if err != nil || !a.AvailableAt.Equal(cutoff) || a.FirstCached != nil {
		t.Fatal(a, err)
	}
}
