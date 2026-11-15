// Package control implements opt-in host-side cooling, separate from HID telemetry.
package control

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"
)

type Step struct {
	DiskC    int `json:"diskC"`
	StationC int `json:"stationC"`
	PWM      int `json:"pwm"`
	MinRPM   int `json:"minRPM"`
	MaxRPM   int `json:"maxRPM"`
}
type Config struct {
	Steps                []Step              `json:"steps"`
	Targets              map[string][]string `json:"targets"`
	HysteresisC          int                 `json:"hysteresisC"`
	DecreaseDelaySeconds int                 `json:"decreaseDelaySeconds"`
	SettleSeconds        int                 `json:"settleSeconds"`
	RetrySeconds         int                 `json:"retrySeconds"`
	LeaseSeconds         int                 `json:"leaseSeconds"`
	SMARTMaxAgeSeconds   int                 `json:"smartMaxAgeSeconds"`
	SMARTFile            string              `json:"smartFile"`
}

func Load(path string) (Config, error) {
	f, e := os.Open(path)
	if e != nil {
		return Config{}, e
	}
	defer f.Close()
	var c Config
	d := json.NewDecoder(io.LimitReader(f, 1<<20))
	d.DisallowUnknownFields()
	if e = d.Decode(&c); e != nil {
		return c, e
	}
	var extra any
	if e = d.Decode(&extra); e != io.EOF {
		return c, fmt.Errorf("control config must contain one JSON object")
	}
	return c, c.Validate()
}
func (c Config) Validate() error {
	if len(c.Steps) < 1 || len(c.Steps) > 10 || len(c.Targets) == 0 {
		return fmt.Errorf("control requires ordered steps and explicit station/disk targets")
	}
	prev := Step{}
	for i, s := range c.Steps {
		if s.PWM < 20 || s.PWM > 100 || s.DiskC < 30 || s.DiskC > 80 || s.StationC < 30 || s.StationC > 85 || s.MinRPM < 500 || s.MaxRPM <= s.MinRPM {
			return fmt.Errorf("invalid control step %d", i)
		}
		if i > 0 && (s.PWM <= prev.PWM || s.DiskC <= prev.DiskC || s.StationC <= prev.StationC) {
			return fmt.Errorf("control steps must increase")
		}
		prev = s
	}
	if prev.PWM != 100 {
		return fmt.Errorf("last step must request 100 percent")
	}
	for id, hashes := range c.Targets {
		if len(id) != 32 || id[:8] != "station-" || len(hashes) == 0 {
			return fmt.Errorf("invalid station target")
		}
		seen := map[string]bool{}
		for _, h := range hashes {
			if len(h) != 64 || seen[h] {
				return fmt.Errorf("invalid or duplicate disk hash")
			}
			for _, r := range h {
				if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
					return fmt.Errorf("disk hash must be lowercase SHA256")
				}
			}
			seen[h] = true
		}
	}
	if c.HysteresisC < 1 || c.HysteresisC > 10 || c.DecreaseDelaySeconds < 30 || c.SettleSeconds < 15 || c.RetrySeconds < 30 || c.LeaseSeconds < 30 || c.LeaseSeconds > 120 || c.SMARTMaxAgeSeconds < 30 || c.SMARTMaxAgeSeconds > 300 || c.SMARTFile == "" {
		return fmt.Errorf("invalid control timing or SMART source")
	}
	return nil
}

type Decision struct {
	Level      int
	LowerSince time.Time
}

func (c Config) Demand(disk, station float64) int {
	level := 0
	for i, s := range c.Steps {
		if disk >= float64(s.DiskC) || station >= float64(s.StationC) {
			level = i + 1
		}
	}
	return level
}
func (d *Decision) Update(c Config, disk, station float64, now time.Time) int {
	next := c.Demand(disk, station)
	if next > d.Level {
		d.Level = next
		d.LowerSince = time.Time{}
	}
	if next == d.Level {
		d.LowerSince = time.Time{}
	}
	if next < d.Level {
		s := c.Steps[d.Level-1]
		if disk > float64(s.DiskC-c.HysteresisC) || station > float64(s.StationC-c.HysteresisC) {
			d.LowerSince = time.Time{}
			return d.Level
		}
		if d.LowerSince.IsZero() {
			d.LowerSince = now
		}
		if now.Sub(d.LowerSince) >= time.Duration(c.DecreaseDelaySeconds)*time.Second {
			d.Level = next
			d.LowerSince = time.Time{}
		}
	}
	return d.Level
}
