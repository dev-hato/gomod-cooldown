// Package availability determines whether a module version is old enough to
// participate in version discovery.
package availability

import (
	"fmt"
	"time"

	"golang.org/x/mod/module"
)

// Availability describes the timestamps used for a decision. FirstCached is
// nil when the complete recent index snapshot proves it predates the cutoff.
type Availability struct {
	CommitTime  time.Time
	FirstCached *time.Time
	AvailableAt time.Time
}

// Query identifies the module version to decide on, together with the commit timestamp reported by its .info endpoint.
type Query struct {
	Module     module.Version
	CommitTime time.Time
}

// Source supplies the availability time for a version.
// Every source answers from data it already holds, so it takes no context.
type Source interface {
	AvailableAt(query Query) (Availability, error)
}

// CommitTimeSource uses only the commit timestamp reported by .info.
type CommitTimeSource struct{}

// AvailableAt returns the supplied commit time as the availability time.
func (CommitTimeSource) AvailableAt(query Query) (Availability, error) {
	return Availability{CommitTime: query.CommitTime, AvailableAt: query.CommitTime}, nil
}

// GoIndexSource uses first-cached timestamps supplied by a complete
// index.golang.org snapshot. A missing version is known to predate Cutoff but
// has no exact timestamp, so it is represented by Cutoff for an inclusive
// cooldown decision.
type GoIndexSource struct {
	Recent map[string]time.Time
	Cutoff time.Time
}

// AvailableAt returns the first-cached availability time from the snapshot.
func (s GoIndexSource) AvailableAt(query Query) (Availability, error) {
	if cached, ok := s.Recent[Key(query.Module)]; ok {
		return Availability{FirstCached: &cached, AvailableAt: cached}, nil
	}
	if s.Cutoff.IsZero() {
		return Availability{}, fmt.Errorf("first-cached time for %s@%s is not in the snapshot", query.Module.Path, query.Module.Version)
	}
	return Availability{AvailableAt: s.Cutoff}, nil
}

// CombinedSource combines .info commit time with a complete index snapshot.
// The snapshot only contains records at or after its cutoff.
type CombinedSource struct {
	Recent map[string]time.Time
}

// Key returns an unambiguous map key for a module path and version.
func Key(mod module.Version) string { return mod.Path + "\x00" + mod.Version }

// AvailableAt returns the later of the commit and first-cached timestamps.
func (s CombinedSource) AvailableAt(query Query) (Availability, error) {
	a := Availability{CommitTime: query.CommitTime, AvailableAt: query.CommitTime}
	if cached, ok := s.Recent[Key(query.Module)]; ok {
		a.FirstCached = &cached
		if a.AvailableAt.Before(cached) {
			a.AvailableAt = cached
		}
	}
	return a, nil
}
