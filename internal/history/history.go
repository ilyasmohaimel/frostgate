package history

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"go.etcd.io/bbolt"
)

type Record struct {
	Timestamp    time.Time `json:"ts"`
	Route        string    `json:"route"`
	Type         string    `json:"type"`
	Status       string    `json:"status"`
	Loss         float64   `json:"loss"`
	MedianMs     int64     `json:"median_ms"`
	JitterMs     int64     `json:"jitter_ms"`
	MinMs        int64     `json:"min_ms"`
	MaxMs        int64     `json:"max_ms"`
	MeanMs       int64     `json:"mean_ms"`
	DownloadMbps float64   `json:"download_mbps,omitempty"`
	Network      string    `json:"network,omitempty"`
	Category     string    `json:"category,omitempty"`
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
	db *bbolt.DB
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
		bdb.Close()
		return nil, err
	}
	// Ensure perms 0600
	os.Chmod(path, 0o600)
	return &DB{db: bdb}, nil
}

func (d *DB) Close() error {
	if d.db != nil {
		return d.db.Close()
	}
	return nil
}

func (d *DB) Add(r Record) error {
	if r.Timestamp.IsZero() {
		r.Timestamp = time.Now()
	}
	// Never store secrets: ensure no URL/password in record (we only store route name)
	// Prune old before adding? Do lazy
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	key := fmt.Sprintf("%d-%s-%d", r.Timestamp.UnixNano(), r.Route, time.Now().UnixNano())
	return d.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte("history"))
		return b.Put([]byte(key), data)
	})
}

func (d *DB) Query(since time.Duration, route, network string) ([]Record, error) {
	var out []Record
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
				return nil
			}
			if !cutoff.IsZero() && r.Timestamp.Before(cutoff) {
				return nil
			}
			if route != "" && r.Route != route {
				return nil
			}
			if network != "" && r.Network != network {
				return nil
			}
			out = append(out, r)
			return nil
		})
	})
	return out, err
}

func (d *DB) Prune(retention time.Duration) error {
	if retention == 0 {
		retention = 90 * 24 * time.Hour
	}
	cutoff := time.Now().Add(-retention)
	return d.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte("history"))
		if b == nil {
			return nil
		}
		var toDelete [][]byte
		b.ForEach(func(k, v []byte) error {
			var r Record
			if err := json.Unmarshal(v, &r); err != nil {
				toDelete = append(toDelete, append([]byte(nil), k...))
				return nil
			}
			if r.Timestamp.Before(cutoff) {
				toDelete = append(toDelete, append([]byte(nil), k...))
			}
			return nil
		})
		for _, k := range toDelete {
			b.Delete(k)
		}
		return nil
	})
}

// Summary holds aggregated stats for a route
type Summary struct {
	Route        string
	Type         string // probe type of the route, e.g. singbox/http
	Count        int
	Availability float64
	Median       time.Duration
	Jitter       time.Duration
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
		var sumJitter int64
		var sumDownload float64
		var validLatency []Record
		var validJitterCount int
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
			if usable && r.MedianMs > 0 {
				medians = append(medians, r.MedianMs)
				validLatency = append(validLatency, r)
				sumJitter += r.JitterMs
				validJitterCount++
			}
			if usable && r.DownloadMbps > 0 {
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
				median = time.Duration(medians[mid]) * time.Millisecond
			} else {
				median = time.Duration((medians[mid-1]+medians[mid])/2) * time.Millisecond
			}
		}
		jitter := time.Duration(0)
		if validJitterCount > 0 {
			jitter = time.Duration(sumJitter/int64(validJitterCount)) * time.Millisecond
		}
		avgDownload := 0.0
		if sumDownloadCount > 0 {
			avgDownload = sumDownload / float64(sumDownloadCount)
		}
		trend := "stable"
		if len(validLatency) >= 4 {
			mid := len(validLatency) / 2
			var firstSum, secondSum int64
			for i := 0; i < mid; i++ {
				firstSum += validLatency[i].MedianMs
			}
			for i := mid; i < len(validLatency); i++ {
				secondSum += validLatency[i].MedianMs
			}
			firstAvg := firstSum / int64(mid)
			secondAvg := secondSum / int64(len(validLatency)-mid)
			if secondAvg > firstAvg+20 {
				trend = "getting worse"
			} else if secondAvg < firstAvg-20 {
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
			Failures:     failures,
			Trend:        trend,
			Download:     avgDownload,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Route < out[j].Route })
	return out
}
