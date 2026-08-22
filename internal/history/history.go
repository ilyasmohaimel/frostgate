package history

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
		// Sort by timestamp for trend
		for i := 0; i < len(recs); i++ {
			for j := i + 1; j < len(recs); j++ {
				if recs[j].Timestamp.Before(recs[i].Timestamp) {
					recs[i], recs[j] = recs[j], recs[i]
				}
			}
		}
		up := 0
		var sumJitter, sumDownload int64
		var sumDownloadCount int
		failures := 0
		var medians []int64
		for _, r := range recs {
			if r.Status != "down" {
				up++
			} else {
				failures++
			}
			if r.Status != "down" && r.MedianMs > 0 {
				medians = append(medians, r.MedianMs)
			}
			sumJitter += r.JitterMs
			if r.DownloadMbps > 0 {
				sumDownload += int64(r.DownloadMbps * 100)
				sumDownloadCount++
			}
		}
		avail := float64(up) / float64(len(recs)) * 100
		median := time.Duration(0)
		if len(medians) > 0 {
			for i := 0; i < len(medians); i++ {
				for j := i + 1; j < len(medians); j++ {
					if medians[j] < medians[i] {
						medians[i], medians[j] = medians[j], medians[i]
					}
				}
			}
			mid := len(medians) / 2
			if len(medians)%2 == 1 {
				median = time.Duration(medians[mid]) * time.Millisecond
			} else {
				median = time.Duration((medians[mid-1]+medians[mid])/2) * time.Millisecond
			}
		}
		jitter := time.Duration(0)
		if len(recs) > 0 {
			jitter = time.Duration(sumJitter/int64(len(recs))) * time.Millisecond
		}
		avgDownload := 0.0
		if sumDownloadCount > 0 {
			avgDownload = float64(sumDownload) / float64(sumDownloadCount) / 100
		}
		trend := "stable"
		if len(recs) >= 4 {
			mid := len(recs) / 2
			var firstSum, secondSum int64
			for i := 0; i < mid; i++ {
				firstSum += recs[i].MedianMs
			}
			for i := mid; i < len(recs); i++ {
				secondSum += recs[i].MedianMs
			}
			firstAvg := firstSum / int64(mid)
			secondAvg := secondSum / int64(len(recs)-mid)
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
	return out
}
