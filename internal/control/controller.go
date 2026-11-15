package control

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/presemantic/sandisk-pro-blade-station-exporter/internal/device"
	"github.com/presemantic/sandisk-pro-blade-station-exporter/internal/protocol"
)

type Fan interface {
	SetFan(context.Context, bool, byte) error
	Close() error
}
type Open func(string) (Fan, error)
type Temperatures interface {
	Temperature(string, time.Time) (float64, error)
}
type Result struct {
	Enabled, Manual, Healthy, RPMValid bool
	DesiredPWM, AppliedPWM, Level      int
	DiskC                              float64
	AppliedAt                          time.Time
	Writes, Errors, Fallbacks          uint64
}
type state struct {
	mu       sync.Mutex
	decision Decision
	result   Result
	retryAt  time.Time
	dev      device.Device
}
type Controller struct {
	Config Config
	Source Temperatures
	Leases LeaseStore
	Open   Open
	Log    *slog.Logger
	Now    func() time.Time
	mu     sync.Mutex
	states map[string]*state
}

func New(c Config, source Temperatures, leases LeaseStore, open Open, log *slog.Logger) *Controller {
	return &Controller{Config: c, Source: source, Leases: leases, Open: open, Log: log, Now: time.Now, states: map[string]*state{}}
}
func (c *Controller) Reconcile(ctx context.Context, dev device.Device, firmware string, status *protocol.Status) Result {
	if _, ok := c.Config.Targets[dev.ID]; !ok {
		return Result{}
	}
	c.mu.Lock()
	s := c.states[dev.ID]
	if s == nil {
		s = &state{}
		c.states[dev.ID] = s
	}
	c.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dev = dev
	s.result.Enabled = true
	now := c.Now()
	fail := func(e error) Result {
		s.result.Healthy = false
		s.result.RPMValid = false
		s.result.Errors++
		s.retryAt = now.Add(time.Duration(c.Config.RetrySeconds) * time.Second)
		c.Log.Warn("cooling fallback to firmware automatic", "station", dev.ID, "error", e)
		if firmware == protocol.SupportedFirmware || s.result.Manual {
			if e = c.restore(ctx, s); e != nil {
				c.Log.Error("cooling automatic restore failed", "station", dev.ID, "error", e)
			}
		}
		return s.result
	}
	if now.Before(s.retryAt) {
		s.result.Healthy = false
		if s.result.Manual {
			_ = c.restore(ctx, s)
		}
		return s.result
	}
	if firmware != protocol.SupportedFirmware || status == nil {
		return fail(fmt.Errorf("station telemetry unavailable or unsupported"))
	}
	if e := c.Leases.Ready(now); e != nil {
		return fail(e)
	}
	disk, e := c.Source.Temperature(dev.ID, now)
	if e != nil {
		return fail(e)
	}
	s.result.DiskC = disk
	for _, t := range status.Temperatures {
		if t < -20 || t > 110 {
			return fail(fmt.Errorf("invalid station sensor"))
		}
	}
	level := s.decision.Update(c.Config, disk, float64(status.Temperatures[0]), now)
	pwm := 0
	if level > 0 {
		pwm = c.Config.Steps[level-1].PWM
	}
	s.result.DesiredPWM = pwm
	s.result.Level = level
	changed := !s.result.Manual || s.result.AppliedPWM != pwm
	if changed {
		s.result.AppliedAt = now
		s.result.RPMValid = false
	}
	settled := now.Sub(s.result.AppliedAt) >= time.Duration(c.Config.SettleSeconds)*time.Second
	if settled {
		lo, hi := 0, 300
		if level > 0 {
			lo = c.Config.Steps[level-1].MinRPM
			hi = c.Config.Steps[level-1].MaxRPM
		}
		s.result.RPMValid = int(status.FanRPM) >= lo && int(status.FanRPM) <= hi
		if !s.result.RPMValid {
			return fail(fmt.Errorf("fan tachometer outside configured range: rpm=%d expected=%d..%d", status.FanRPM, lo, hi))
		}
	}
	fan, e := c.Open(dev.Path)
	if e != nil {
		return fail(e)
	}
	e = c.Leases.Apply(dev.ID, now, func() error { return fan.SetFan(ctx, true, byte((pwm*255+50)/100)) })
	fan.Close()
	if e != nil {
		return fail(e)
	}
	s.result.Manual = true
	s.result.AppliedPWM = pwm
	s.result.Healthy = true
	s.result.Writes++
	if changed {
		c.Log.Info("cooling target applied", "station", dev.ID, "pwm_percent", pwm, "disk_celsius", disk, "station_celsius", status.Temperatures[0])
	}
	return s.result
}
func (c *Controller) restore(ctx context.Context, s *state) error {
	// Restore also when a lease was armed just before an uncertain write.
	fan, e := c.Open(s.dev.Path)
	if e != nil {
		return e
	}
	defer fan.Close()
	e = c.Leases.Release(s.dev.ID, func() error { return fan.SetFan(ctx, false, 0) })
	if e == nil {
		s.result.Manual = false
		s.result.AppliedPWM = 0
		s.result.Fallbacks++
		s.decision = Decision{}
	}
	return e
}
func (c *Controller) Stop(ctx context.Context) {
	c.mu.Lock()
	states := make([]*state, 0, len(c.states))
	for _, s := range c.states {
		states = append(states, s)
	}
	c.mu.Unlock()
	for _, s := range states {
		s.mu.Lock()
		if e := c.restore(ctx, s); e != nil {
			c.Log.Error("cooling shutdown restore failed", "station", s.dev.ID, "error", e)
		}
		s.mu.Unlock()
	}
}
