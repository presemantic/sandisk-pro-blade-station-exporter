package exporter

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/presemantic/sandisk-pro-blade-station-exporter/internal/control"
	"github.com/presemantic/sandisk-pro-blade-station-exporter/internal/device"
	"github.com/presemantic/sandisk-pro-blade-station-exporter/internal/protocol"
)

type Transport interface {
	Query(context.Context, protocol.Command, byte) ([]byte, error)
	Close() error
}
type Discover func() ([]device.Device, error)
type Open func(string) (Transport, error)
type Config struct {
	Interval, MetadataInterval, MaxAge time.Duration
	Workers                            int
	Aliases                            map[string]string
}
type Station struct {
	Device                    device.Device
	Alias                     string
	Firmware                  protocol.Firmware
	Tables                    [7]protocol.ThermalTable
	MetadataAt                time.Time
	Status                    protocol.Status
	At                        time.Time
	OK, Supported, MetadataOK bool
	Errors                    uint64
	Requests, RequestErrors   [3]uint64
	Duration                  time.Duration
	Control                   control.Result
}
type Snapshot struct {
	Stations        []Station
	At              time.Time
	DiscoveryOK     bool
	DiscoveryErrors uint64
	Cycles          uint64
	Duration        time.Duration
}
type Manager struct {
	config     Config
	discover   Discover
	open       Open
	logger     *slog.Logger
	mu         sync.RWMutex
	snapshot   Snapshot
	controller *control.Controller
}

// SetController must be called before Run. Telemetry-only operation is the default.
func (m *Manager) SetController(c *control.Controller) { m.controller = c }

func New(config Config, discover Discover, open Open, logger *slog.Logger) *Manager {
	return &Manager{config: config, discover: discover, open: open, logger: logger}
}
func (m *Manager) Snapshot() Snapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s := m.snapshot
	s.Stations = append([]Station(nil), s.Stations...)
	return s
}
func (m *Manager) MaxAge() time.Duration { return m.config.MaxAge }
func (m *Manager) Run(ctx context.Context) {
	for {
		started := time.Now()
		m.Poll(ctx)
		delay := m.config.Interval - time.Since(started)
		if delay < 0 {
			delay = 0
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
func (m *Manager) Poll(ctx context.Context) {
	start := time.Now()
	old := m.Snapshot()
	next := Snapshot{Cycles: old.Cycles + 1, DiscoveryErrors: old.DiscoveryErrors}
	devices, err := m.discover()
	if err != nil {
		next.DiscoveryErrors++
		next.Stations = old.Stations
		for i := range next.Stations {
			next.Stations[i].OK = false
		}
		m.logger.Error("station discovery failed", "error", err)
	} else {
		next.DiscoveryOK = true
		previous := map[string]Station{}
		for _, s := range old.Stations {
			previous[s.Device.ID] = s
		}
		next.Stations = make([]Station, len(devices))
		workers := min(m.config.Workers, len(devices))
		jobs := make(chan int, len(devices))
		var wg sync.WaitGroup
		// One cycle has a fixed budget. Unresponsive stations cannot queue forever.
		cycle, cancel := context.WithTimeout(ctx, m.config.Interval)
		defer cancel()
		for i := range devices {
			jobs <- i
		}
		close(jobs)
		for range workers {
			wg.Go(func() {
				for i := range jobs {
					s := previous[devices[i].ID]
					s.Device = devices[i]
					s.Alias = m.config.Aliases[s.Device.ID]
					next.Stations[i] = m.pollStation(cycle, s)
				}
			})
		}
		wg.Wait()
		sort.Slice(next.Stations, func(i, j int) bool { return next.Stations[i].Device.ID < next.Stations[j].Device.ID })
	}
	if m.controller != nil {
		for i := range next.Stations {
			s := &next.Stations[i]
			var status *protocol.Status
			if s.OK && next.DiscoveryOK {
				status = &s.Status
			}
			s.Control = m.controller.Reconcile(ctx, s.Device, s.Firmware.Version, status)
		}
	}
	next.At = time.Now()
	next.Duration = time.Since(start)
	m.mu.Lock()
	m.snapshot = next
	m.mu.Unlock()
}
func (m *Manager) pollStation(ctx context.Context, s Station) Station {
	start := time.Now()
	s.OK = false
	deferDuration := func() { s.Duration = time.Since(start) }
	if ctx.Err() != nil {
		s.Errors++
		deferDuration()
		return s
	}
	client, err := m.open(s.Device.Path)
	if err != nil {
		s.Errors++
		deferDuration()
		m.logger.Warn("station open failed", "station", s.Device.ID, "error", err)
		return s
	}
	defer client.Close()
	query := func(command protocol.Command, profile byte) ([]byte, error) {
		index := 0
		if command == protocol.GetStatus {
			index = 1
		}
		if command == protocol.GetThermalTable {
			index = 2
		}
		s.Requests[index]++
		data, e := client.Query(ctx, command, profile)
		if e != nil {
			s.RequestErrors[index]++
		}
		return data, e
	}
	needsMetadata := s.MetadataAt.IsZero() || time.Since(s.MetadataAt) >= m.config.MetadataInterval
	if needsMetadata {
		data, e := query(protocol.GetFirmware, 0)
		if e == nil {
			s.Firmware, e = protocol.DecodeFirmware(data)
		}
		if e != nil {
			s.Errors++
			s.MetadataOK = false
			s.Supported = false
			deferDuration()
			m.logger.Warn("firmware read failed", "station", s.Device.ID, "error", e)
			return s
		}
		s.Supported = s.Firmware.Version == protocol.SupportedFirmware
		if !s.Supported {
			s.MetadataOK = false
			s.MetadataAt = time.Now()
			s.Errors++
			deferDuration()
			m.logger.Warn("unsupported station firmware", "station", s.Device.ID, "firmware", s.Firmware.Version)
			return s
		}
		s.MetadataOK = false
		var tables [7]protocol.ThermalTable
		for profile := range byte(7) {
			raw, e := query(protocol.GetThermalTable, profile)
			if e == nil {
				tables[profile], e = protocol.DecodeThermalTable(raw)
			}
			if e != nil {
				err = e
				break
			}
		}
		if err == nil {
			s.Tables = tables
			s.MetadataAt = time.Now()
			s.MetadataOK = true
		} else {
			m.logger.Warn("thermal table read failed", "station", s.Device.ID, "error", err)
		}
	}
	if !s.Supported {
		deferDuration()
		return s
	}
	data, e := query(protocol.GetStatus, 0)
	if e == nil {
		s.Status, e = protocol.DecodeStatus(data)
	}
	if e != nil {
		s.Errors++
		deferDuration()
		m.logger.Warn("station status failed", "station", s.Device.ID, "error", e)
		return s
	}
	s.OK = true
	s.At = time.Now()
	deferDuration()
	return s
}
func (c Config) Validate() error {
	if c.Interval < time.Second || c.MetadataInterval < c.Interval || c.MaxAge < c.Interval {
		return fmt.Errorf("require interval>=1s, metadata interval>=interval, max age>=interval")
	}
	if c.Workers < 1 || c.Workers > 64 {
		return fmt.Errorf("workers must be between 1 and 64")
	}
	return nil
}
