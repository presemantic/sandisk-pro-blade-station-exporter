package exporter

import (
	"context"
	"errors"
	"fmt"
	"github.com/presemantic/sandisk-pro-blade-station-exporter/internal/device"
	"github.com/presemantic/sandisk-pro-blade-station-exporter/internal/protocol"
	"github.com/prometheus/client_golang/prometheus"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

type fake struct {
	failed      bool
	firmware    byte
	active, max *atomic.Int32
}

func (f *fake) Close() error {
	if f.active != nil {
		f.active.Add(-1)
	}
	return nil
}
func (f *fake) Query(ctx context.Context, c protocol.Command, p byte) ([]byte, error) {
	if f.failed {
		return nil, errors.New("unplugged")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b := make([]byte, 64)
	b[0] = 8
	b[3] = 1
	b[4] = byte(c)
	switch c {
	case protocol.GetFirmware:
		b[1] = 10
		copy(b[5:], []byte{2, 0, 1, f.firmware})
	case protocol.GetStatus:
		b[1] = 17
		b[7] = 50
		b[9] = 48
		b[11] = 49
		b[13] = 40
		b[15] = 41
		b[19] = 51
	case protocol.GetThermalTable:
		b[1] = 23
	}
	for _, v := range b[3 : 3+int(b[1])] {
		b[2] += v
	}
	return b, nil
}
func config() Config {
	return Config{Interval: time.Second, MetadataInterval: time.Hour, MaxAge: 3 * time.Second, Workers: 4}
}
func logger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
func TestFailureDropsSensorsAndHotplug(t *testing.T) {
	devices := []device.Device{{ID: "a", Path: "/dev/hidraw1"}}
	fail := false
	m := New(config(), func() ([]device.Device, error) { return devices, nil }, func(string) (Transport, error) { return &fake{failed: fail, firmware: 2}, nil }, logger())
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(NewCollector(m))
	m.Poll(context.Background())
	first := m.Snapshot().Stations[0]
	if !first.OK || !first.MetadataOK {
		t.Fatal(first)
	}
	metrics, e := reg.Gather()
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, f := range metrics {
		if f.GetName() == prefix+"temperature_celsius" {
			found = true
		}
	}
	if !found {
		t.Fatal("no sensors")
	}
	fail = true
	m.Poll(context.Background())
	metrics, e = reg.Gather()
	if e != nil {
		t.Fatal(e)
	}
	for _, f := range metrics {
		if f.GetName() == prefix+"temperature_celsius" {
			t.Fatal("stale sensor value emitted")
		}
	}
	devices = nil
	m.Poll(context.Background())
	if len(m.Snapshot().Stations) != 0 {
		t.Fatal("removed station retained")
	}
}
func TestHundredStationsBoundedConcurrency(t *testing.T) {
	devices := make([]device.Device, 100)
	for i := range devices {
		devices[i] = device.Device{ID: fmt.Sprint(i), Path: fmt.Sprint(i)}
	}
	var active, max atomic.Int32
	m := New(config(), func() ([]device.Device, error) { return devices, nil }, func(string) (Transport, error) {
		n := active.Add(1)
		for old := max.Load(); n > old && !max.CompareAndSwap(old, n); old = max.Load() {
		}
		return &fake{firmware: 2, active: &active}, nil
	}, logger())
	m.Poll(context.Background())
	if len(m.Snapshot().Stations) != 100 || max.Load() > 4 || active.Load() != 0 {
		t.Fatal("fleet concurrency incorrect", max.Load(), active.Load())
	}
	for _, s := range m.Snapshot().Stations {
		if !s.OK {
			t.Fatal("station not read")
		}
	}
}
func TestMetadataCachedAndUnsupportedFirmware(t *testing.T) {
	m := New(config(), func() ([]device.Device, error) { return []device.Device{{ID: "a"}}, nil }, func(string) (Transport, error) { return &fake{firmware: 2}, nil }, logger())
	m.Poll(context.Background())
	m.Poll(context.Background())
	s := m.Snapshot().Stations[0]
	if s.Requests != [3]uint64{1, 2, 7} {
		t.Fatal("metadata not cached", s.Requests)
	}
	m = New(config(), func() ([]device.Device, error) { return []device.Device{{ID: "a"}}, nil }, func(string) (Transport, error) { return &fake{firmware: 99}, nil }, logger())
	m.Poll(context.Background())
	s = m.Snapshot().Stations[0]
	if s.Supported || s.OK || s.Requests[1] != 0 {
		t.Fatal("unknown firmware read as known")
	}
}
func TestExpiredSensorNotExposed(t *testing.T) {
	m := New(config(), func() ([]device.Device, error) { return nil, nil }, nil, logger())
	m.snapshot = Snapshot{DiscoveryOK: true, Stations: []Station{{Device: device.Device{ID: "stale"}, OK: true, At: time.Now().Add(-time.Hour)}}}
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(NewCollector(m))
	ms, e := reg.Gather()
	if e != nil {
		t.Fatal(e)
	}
	for _, f := range ms {
		if f.GetName() == prefix+"temperature_celsius" {
			t.Fatal("expired readings visible")
		}
	}
}
