package control

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

// SMARTSource consumes the existing atomic textfile; it never opens a disk.
type SMARTSource struct {
	File, SysRoot string
	MaxAge        time.Duration
	Targets       map[string][]string
}

func sampleValue(m *dto.Metric) float64 {
	if m.Gauge != nil {
		return m.Gauge.GetValue()
	}
	if m.Untyped != nil {
		return m.Untyped.GetValue()
	}
	return math.NaN()
}

func (s SMARTSource) Temperature(id string, now time.Time) (float64, error) {
	f, e := os.Open(s.File)
	if e != nil {
		return 0, e
	}
	defer f.Close()
	p := expfmt.NewTextParser(model.UTF8Validation)
	families, e := p.TextToMetricFamilies(io.LimitReader(f, 4<<20))
	if e != nil {
		return 0, e
	}
	success := families["szew_host_collector_success"]
	stamp := families["szew_host_collector_timestamp_seconds"]
	if success == nil || len(success.Metric) != 1 || sampleValue(success.Metric[0]) != 1 || stamp == nil || len(stamp.Metric) != 1 {
		return 0, fmt.Errorf("SMART collector not healthy")
	}
	ts := sampleValue(stamp.Metric[0])
	age := now.Sub(time.Unix(0, int64(ts*1e9)))
	if math.IsNaN(ts) || math.IsInf(ts, 0) || age > s.MaxAge || age < -5*time.Second {
		return 0, fmt.Errorf("SMART collector timestamp stale or invalid")
	}
	wanted := map[string]bool{}
	for _, h := range s.Targets[id] {
		wanted[h] = true
	}
	if len(wanted) == 0 {
		return 0, fmt.Errorf("no disks configured for station")
	}
	devices := map[string]string{}
	entries, e := os.ReadDir(filepath.Join(s.SysRoot, "class/block"))
	if e != nil {
		return 0, e
	}
	for _, entry := range entries {
		raw, e := os.ReadFile(filepath.Join(s.SysRoot, "class/block", entry.Name(), "device/serial"))
		if e != nil {
			continue
		}
		h := sha256.Sum256([]byte(strings.TrimSpace(string(raw))))
		key := hex.EncodeToString(h[:])
		if wanted[key] {
			if _, exists := devices[key]; exists {
				return 0, fmt.Errorf("duplicate disk identity")
			}
			devices[key] = entry.Name()
		}
	}
	maximum := -math.MaxFloat64
	for hash := range wanted {
		dev, ok := devices[hash]
		if !ok {
			return 0, fmt.Errorf("configured disk missing")
		}
		temp, found := float64(0), false
		available := false
		for _, name := range []string{"szew_disk_temperature_celsius", "szew_disk_smart_available"} {
			family := families[name]
			if family == nil {
				continue
			}
			for _, m := range family.Metric {
				match := false
				for _, l := range m.Label {
					if l.GetName() == "device" && l.GetValue() == dev {
						match = true
					}
				}
				if !match {
					continue
				}
				v := sampleValue(m)
				if name == "szew_disk_temperature_celsius" {
					if found {
						return 0, fmt.Errorf("duplicate disk temperature")
					}
					temp = v
					found = true
				} else {
					available = v == 1
				}
			}
		}
		if !available || !found || math.IsNaN(temp) || math.IsInf(temp, 0) || temp < -20 || temp > 110 {
			return 0, fmt.Errorf("configured disk SMART temperature unavailable")
		}
		maximum = max(maximum, temp)
	}
	return maximum, nil
}
