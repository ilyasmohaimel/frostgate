package history

import (
	"errors"
	"os"
	"sync"
	"time"

	"github.com/ilyasmohaimel/frostgate/internal/probe"
	"go.etcd.io/bbolt"
)

// ErrNoHistory is returned by OpenReadonly when no history database exists.
var ErrNoHistory = errors.New("no history database")

// OpenReadonly opens the history DB only if it already exists on disk. It
// never creates directories or files, so read-only commands are safe when
// history is disabled or has never been written.
func OpenReadonly() (*DB, error) {
	path := HistoryPath()
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNoHistory
		}
		return nil, err
	}
	bdb, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: 2 * time.Second, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	return &DB{db: bdb}, nil
}

// Recorder owns the history lifecycle for one process: enabled flag,
// retention, watch interval, short-lived writes and pruning. When disabled
// it must not create any file or directory.
type Recorder struct {
	mu            sync.Mutex
	enabled       bool
	retention     time.Duration
	watchInterval time.Duration
	profile       string
	lastWrite     time.Time
	lastPrune     time.Time
	commit        func(*DB, []Record, time.Duration, bool) error
}

// NewRecorder builds a Recorder from resolved config values.
func NewRecorder(enabled bool, retention, watchInterval time.Duration) *Recorder {
	return NewRecorderForProfile(enabled, retention, watchInterval, DefaultProfile)
}

func NewRecorderForProfile(enabled bool, retention, watchInterval time.Duration, profile string) *Recorder {
	if retention <= 0 {
		retention = 90 * 24 * time.Hour
	}
	if watchInterval <= 0 {
		watchInterval = 5 * time.Minute
	}
	if profile == "" {
		profile = DefaultProfile
	}
	return &Recorder{enabled: enabled, retention: retention, watchInterval: watchInterval, profile: profile, commit: func(db *DB, records []Record, retention time.Duration, prune bool) error {
		return db.RecordSnapshot(records, retention, prune)
	}}
}

// Enabled reports whether history recording is active.
func (r *Recorder) Enabled() bool { return r.enabled }

// WatchInterval returns the configured minimum spacing between history
// writes during continuous modes (watch/TUI).
func (r *Recorder) WatchInterval() time.Duration { return r.watchInterval }

// Retention returns the configured retention window.
func (r *Recorder) Retention() time.Duration { return r.retention }

// Record writes records unconditionally (subject to enabled) and prunes old
// entries at most once per watch interval. A disabled recorder is a no-op and
// never touches the filesystem.
func (r *Recorder) Record(recs []Record) error {
	_, err := r.write(recs, false)
	return err
}

// RecordThrottled drops the write entirely when the previous write happened
// less than watchInterval ago. Used by watch JSON and TUI snapshots so manual
// refreshes cannot flood the DB.
func (r *Recorder) RecordThrottled(recs []Record) (bool, error) {
	return r.write(recs, true)
}

func (r *Recorder) write(recs []Record, throttled bool) (bool, error) {
	if r == nil || !r.enabled || len(recs) == 0 {
		return false, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	if throttled && !r.lastWrite.IsZero() && now.Sub(r.lastWrite) < r.watchInterval {
		return false, nil
	}
	db, err := Open()
	if err != nil {
		return false, err
	}
	defer db.Close()
	prepared := make([]Record, len(recs))
	for i, rec := range recs {
		if rec.Profile == "" {
			rec.Profile = r.profile
		}
		prepared[i] = rec
	}
	shouldPrune := r.lastPrune.IsZero() || now.Sub(r.lastPrune) >= r.watchInterval
	if err := r.commit(db, prepared, r.retention, shouldPrune); err != nil {
		return false, err
	}
	r.lastWrite = now
	if shouldPrune {
		r.lastPrune = now
	}
	return true, nil
}

// Close is retained for API compatibility; writes no longer keep the DB open.
func (r *Recorder) Close() error {
	return nil
}

// FromProbeResults converts probe results into history records. downloadFor
// may be nil; route names come straight from endpoints (never secrets).
func FromProbeResults(results []probe.Result, downloadFor func(name string) float64, networkLabel string, profile ...string) []Record {
	profileID := ""
	if len(profile) > 0 {
		profileID = profile[0]
	}
	out := make([]Record, 0, len(results))
	for _, res := range results {
		var dl float64
		if downloadFor != nil {
			dl = downloadFor(res.Endpoint.Name)
		}
		out = append(out, Record{
			Route:        res.Endpoint.Name,
			Type:         string(res.Endpoint.Type),
			Status:       string(res.Status),
			Loss:         res.Metrics.LossPct,
			MedianMs:     res.Metrics.Median.Milliseconds(),
			JitterMs:     res.Metrics.Jitter.Milliseconds(),
			JitterKnown:  boolPtr(res.Metrics.JitterKnown),
			MinMs:        res.Metrics.Min.Milliseconds(),
			MaxMs:        res.Metrics.Max.Milliseconds(),
			MeanMs:       res.Metrics.Mean.Milliseconds(),
			DownloadMbps: dl,
			Network:      sanitizeNetworkLabel(networkLabel),
			Category:     res.Category,
			Profile:      profileID,
		})
	}
	return out
}

func boolPtr(v bool) *bool { return &v }
