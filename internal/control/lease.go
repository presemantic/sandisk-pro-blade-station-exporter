package control

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Lease struct {
	Station string  `json:"station"`
	Expires float64 `json:"expires"`
	Armed   bool    `json:"armed"`
}

func atomicJSON(path string, v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".state-")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if _, e = f.Write(b); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	return os.Rename(f.Name(), path)
}

type LeaseStore struct {
	Dir string
	TTL time.Duration
}

func (s LeaseStore) path(id string) string { return filepath.Join(s.Dir, id+".json") }
func (s LeaseStore) Ready(now time.Time) error {
	st, e := os.Stat(filepath.Join(s.Dir, "watchdog.ready"))
	if e != nil {
		return fmt.Errorf("watchdog unavailable: %w", e)
	}
	if age := now.Sub(st.ModTime()); age < -5*time.Second || age > 5*time.Second {
		return fmt.Errorf("watchdog heartbeat stale")
	}
	return nil
}
func (s LeaseStore) Apply(id string, now time.Time, send func() error) error {
	return withLock(s.Dir, func() error {
		if e := s.Ready(now); e != nil {
			return e
		}
		if e := atomicJSON(s.path(id), Lease{id, float64(now.Add(s.TTL).UnixNano()) / 1e9, true}); e != nil {
			return e
		}
		return send()
	})
}
func (s LeaseStore) Release(id string, send func() error) error {
	return withLock(s.Dir, func() error {
		if e := send(); e != nil {
			return e
		}
		if e := os.Remove(s.path(id)); e != nil && !os.IsNotExist(e) {
			return e
		}
		return nil
	})
}

// Sweep is independent of the exporter process. The lock prevents stale restores
// racing a renewed lease and a successfully issued manual command.
func (s LeaseStore) Sweep(now time.Time, restore func(string) error) error {
	return withLock(s.Dir, func() error {
		if e := os.WriteFile(filepath.Join(s.Dir, "watchdog.ready"), []byte("ready\n"), 0600); e != nil {
			return e
		}
		if e := os.Chtimes(filepath.Join(s.Dir, "watchdog.ready"), now, now); e != nil {
			return e
		}
		entries, e := os.ReadDir(s.Dir)
		if e != nil {
			return e
		}
		var first error
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), "station-") || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			path := filepath.Join(s.Dir, entry.Name())
			b, e := os.ReadFile(path)
			if e != nil {
				if first == nil {
					first = e
				}
				continue
			}
			var lease Lease
			if e = json.Unmarshal(b, &lease); e != nil {
				if first == nil {
					first = e
				}
				continue
			}
			if lease.Station+".json" != entry.Name() {
				if first == nil {
					first = fmt.Errorf("invalid lease identity")
				}
				continue
			}
			if lease.Armed && (math.IsNaN(lease.Expires) || math.IsInf(lease.Expires, 0) || float64(now.UnixNano())/1e9 > lease.Expires) {
				if e = restore(lease.Station); e != nil {
					if first == nil {
						first = e
					}
					continue
				}
				if e = os.Remove(path); e != nil && first == nil {
					first = e
				}
			}
		}
		return first
	})
}
