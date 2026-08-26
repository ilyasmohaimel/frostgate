package history

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/ilyasmohaimel/frostgate/internal/profile"
	"github.com/ilyasmohaimel/frostgate/internal/redact"
	"go.etcd.io/bbolt"
)

type Record struct {
	Timestamp time.Time `json:"ts"`
	Route     string    `json:"route"`
	Type      string    `json:"type"`
	Status    string    `json:"status"`
	Loss      float64   `json:"loss"`
	MedianMs  int64     `json:"median_ms"`
	JitterMs  int64     `json:"jitter_ms"`
	// JitterKnown is a pointer so an absent legacy field stays distinguishable
	// from a newly recorded false value. Legacy zero jitter is not evidence of
	// perfect jitter; legacy non-zero jitter is.
	JitterKnown  *bool   `json:"jitter_known,omitempty"`
	MinMs        int64   `json:"min_ms"`
	MaxMs        int64   `json:"max_ms"`
	MeanMs       int64   `json:"mean_ms"`
	DownloadMbps float64 `json:"download_mbps,omitempty"`
	Network      string  `json:"network,omitempty"`
	Category     string  `json:"category,omitempty"`
	// Profile scopes equal route names in independent Frostgate configs. The
	// empty value is a legacy record and is visible only to the default profile.
	Profile string `json:"profile,omitempty"`
}

const DefaultProfile = "default"

func ProfileID(configPath string, isDefault bool) string {
	if isDefault {
		return DefaultProfile
	}
	sum := sha256.Sum256([]byte(profile.CanonicalPath(configPath)))
	return "p-" + hex.EncodeToString(sum[:12])
}

func HistoryPath() string {
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "frostgate", "history.db")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "frostgate", "history.db")
}

func HistoryDir() string {
	return filepath.Dir(HistoryPath())
}

// StatDB reports whether a history database file exists on disk.
func StatDB() (os.FileInfo, error) {
	return os.Stat(HistoryPath())
}

type DB struct {
	db  *bbolt.DB
	put func(*bbolt.Bucket, []byte, []byte) error
}

type preparedRecord struct {
	record Record
	data   []byte
}

func Open() (*DB, error) {
	path := HistoryPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	bdb, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, err
	}
	err = bdb.Update(func(tx *bbolt.Tx) error {
		_, e := tx.CreateBucketIfNotExists([]byte("history"))
		return e
	})
	if err != nil {
		return nil, errors.Join(err, bdb.Close())
	}
	// Ensure perms 0600. A failure here would leave measurements readable by
	// other users, so do not continue as if the database were private.
	if err := os.Chmod(path, 0o600); err != nil {
		closeErr := bdb.Close()
		if closeErr != nil {
			return nil, fmt.Errorf("secure history permissions: %w; close history db: %v", err, closeErr)
		}
		return nil, fmt.Errorf("secure history permissions: %w", err)
	}
	return &DB{db: bdb}, nil
}

func (d *DB) Close() error {
	if d.db != nil {
		return d.db.Close()
	}
	return nil
}

func (d *DB) Add(r Record) error {
	return d.RecordSnapshot([]Record{r}, 0, false)
}

// RecordSnapshot atomically writes one logical probe snapshot and, when
// requested, its retention maintenance. A returned error therefore never
// misrepresents a partially committed snapshot as failed.
func (d *DB) RecordSnapshot(records []Record, retention time.Duration, prune bool) error {
	prepared := make([]preparedRecord, 0, len(records))
	now := time.Now()
	for _, r := range records {
		if r.Timestamp.IsZero() {
			r.Timestamp = now
		}
		r.Route = redact.String(r.Route)
		r.Network = sanitizeNetworkLabel(r.Network)
		r.Category = redact.String(r.Category)
		data, err := json.Marshal(r)
		if err != nil {
			return err
		}
		prepared = append(prepared, preparedRecord{record: r, data: data})
	}
	if retention == 0 {
		retention = 90 * 24 * time.Hour
	}
	return d.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte("history"))
		if b == nil {
			return fmt.Errorf("history bucket missing")
		}
		for _, record := range prepared {
			sequence, err := b.NextSequence()
			if err != nil {
				return err
			}
			// The route name is never used raw as a bbolt key. Its hash keeps
			// keys deterministic/safe while the sequence makes concurrent writes
			// collision-free inside the transaction.
			hash := sha256.Sum256([]byte(record.record.Route))
			key := fmt.Sprintf("%020d-%020d-%x", record.record.Timestamp.UnixNano(), sequence, hash[:8])
			put := d.put
			if put == nil {
				put = func(bucket *bbolt.Bucket, key, value []byte) error { return bucket.Put(key, value) }
			}
			if err := put(b, []byte(key), record.data); err != nil {
				return err
			}
		}
		if prune {
			return pruneBucket(b, retention, now)
		}
		return nil
	})
}

func (d *DB) Query(since time.Duration, route, network string) ([]Record, error) {
	return d.QueryProfile(since, route, network, "", false)
}

// QueryProfile returns only records for a selected profile. Legacy unscoped
// records are kept readable for the default profile but never leak into an
// explicit custom profile.
func (d *DB) QueryProfile(since time.Duration, route, network, profile string, includeLegacy bool) ([]Record, error) {
	var out []Record
	network = sanitizeNetworkLabel(network)
	cutoff := time.Now().Add(-since)
	if since == 0 {
		cutoff = time.Time{}
	}
	err := d.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte("history"))
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			var r Record
			if err := json.Unmarshal(v, &r); err != nil {
				return fmt.Errorf("decode history record %q: %w", string(k), err)
			}
			if !cutoff.IsZero() && r.Timestamp.Before(cutoff) {
				return nil
			}
			r.Route = redact.String(r.Route)
			r.Network = sanitizeNetworkLabel(r.Network)
			r.Category = redact.String(r.Category)
			if route != "" && r.Route != route {
				return nil
			}
			if network != "" && r.Network != network {
				return nil
			}
			if profile != "" && r.Profile != profile && !(includeLegacy && r.Profile == "") {
				return nil
			}
			out = append(out, r)
			return nil
		})
	})
	return out, err
}

func sanitizeNetworkLabel(label string) string {
	if label == "" {
		return ""
	}
	return redact.String(label)
}

func (d *DB) Prune(retention time.Duration) error {
	if retention == 0 {
		retention = 90 * 24 * time.Hour
	}
	return d.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte("history"))
		if b == nil {
			return nil
		}
		return pruneBucket(b, retention, time.Now())
	})
}

func pruneBucket(b *bbolt.Bucket, retention time.Duration, now time.Time) error {
	cutoff := now.Add(-retention)
	var toDelete [][]byte
	if err := b.ForEach(func(k, v []byte) error {
		var r Record
		if err := json.Unmarshal(v, &r); err != nil {
			toDelete = append(toDelete, append([]byte(nil), k...))
			return nil
		}
		if r.Timestamp.Before(cutoff) {
			toDelete = append(toDelete, append([]byte(nil), k...))
		}
		return nil
	}); err != nil {
		return err
	}
	for _, k := range toDelete {
		if err := b.Delete(k); err != nil {
			return err
		}
	}
	return nil
}

// Summary holds aggregated stats for a route
type Summary struct {
	Route        string
	Type         string // probe type of the route, e.g. singbox/http
	Count        int
	Availability float64
	Median       time.Duration
	Jitter       time.Duration
	JitterKnown  bool
	Failures     int
	Trend        string
	Download     float64
}

func Summarize(records []Record) []Summary {
	groups := map[string][]Record{}
	for _, r := range records {
		groups[r.Route] = append(groups[r.Route], r)
	}
	var out []Summary
	for route, recs := range groups {
		if len(recs) == 0 {
			continue
		}
		routeType := recs[0].Type
		sort.Slice(recs, func(i, j int) bool { return recs[i].Timestamp.Before(recs[j].Timestamp) })
		up := 0
		var sumDownload float64
		var validLatency []Record
		var jitters []int64
		var sumDownloadCount int
		failures := 0
		var medians []int64
		for _, r := range recs {
			usable := r.Status == "up" || r.Status == "degraded"
			if usable {
				up++
			} else {
				failures++
			}
			if usable && r.MedianMs >= 0 {
				medians = append(medians, r.MedianMs)
				validLatency = append(validLatency, r)
			}
			if usable && recordJitterKnown(r) && r.JitterMs >= 0 {
				jitters = append(jitters, r.JitterMs)
			}
			if usable && r.DownloadMbps > 0 && !math.IsNaN(r.DownloadMbps) && !math.IsInf(r.DownloadMbps, 0) {
				sumDownload += r.DownloadMbps
				sumDownloadCount++
			}
		}
		avail := float64(up) / float64(len(recs)) * 100
		median := time.Duration(0)
		if len(medians) > 0 {
			sort.Slice(medians, func(i, j int) bool { return medians[i] < medians[j] })
			mid := len(medians) / 2
			if len(medians)%2 == 1 {
				median = durationFromMilliseconds(medians[mid])
			} else {
				median = durationFromMilliseconds(medians[mid-1] + (medians[mid]-medians[mid-1])/2)
			}
		}
		jitter := time.Duration(0)
		if len(jitters) > 0 {
			jitter = durationFromMilliseconds(meanNonNegativeInt64(jitters))
		}
		avgDownload := 0.0
		if sumDownloadCount > 0 {
			avgDownload = sumDownload / float64(sumDownloadCount)
		}
		trend := "stable"
		if len(validLatency) >= 4 {
			mid := len(validLatency) / 2
			first := make([]int64, 0, mid)
			second := make([]int64, 0, len(validLatency)-mid)
			for i := 0; i < mid; i++ {
				first = append(first, validLatency[i].MedianMs)
			}
			for i := mid; i < len(validLatency); i++ {
				second = append(second, validLatency[i].MedianMs)
			}
			firstAvg := meanNonNegativeInt64(first)
			secondAvg := meanNonNegativeInt64(second)
			if secondAvg > saturatingAddMilliseconds(firstAvg, 20) {
				trend = "getting worse"
			} else if firstAvg > saturatingAddMilliseconds(secondAvg, 20) {
				trend = "improving"
			}
		}
		out = append(out, Summary{
			Route:        route,
			Type:         routeType,
			Count:        len(recs),
			Availability: avail,
			Median:       median,
			Jitter:       jitter,
			JitterKnown:  len(jitters) > 0,
			Failures:     failures,
			Trend:        trend,
			Download:     avgDownload,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Route < out[j].Route })
	return out
}

func saturatingAddMilliseconds(value, increment int64) int64 {
	if increment > 0 && value > math.MaxInt64-increment {
		return math.MaxInt64
	}
	return value + increment
}

func durationFromMilliseconds(ms int64) time.Duration {
	if ms <= 0 {
		return 0
	}
	if ms > math.MaxInt64/int64(time.Millisecond) {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(ms) * time.Millisecond
}

// meanNonNegativeInt64 avoids overflow when summarizing a user-owned legacy
// database. Callers have filtered negative values first.
func meanNonNegativeInt64(values []int64) int64 {
	if len(values) == 0 {
		return 0
	}
	var quotient int64
	var remainder uint64
	n := int64(len(values))
	denominator := uint64(n)
	for _, value := range values {
		quotient += value / n
		rest := uint64(value % n)
		if remainder >= denominator-rest {
			quotient++
			remainder -= denominator - rest
		} else {
			remainder += rest
		}
	}
	return quotient
}

func recordJitterKnown(r Record) bool {
	if r.JitterKnown != nil {
		return *r.JitterKnown
	}
	// Before jitter evidence was stored, non-zero jitter must have come from
	// at least two samples. A legacy zero may mean either perfect jitter or
	// insufficient samples, so preserve that uncertainty.
	return r.JitterMs != 0
}
