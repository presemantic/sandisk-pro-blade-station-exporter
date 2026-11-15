package control

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/presemantic/sandisk-pro-blade-station-exporter/internal/device"
	"github.com/presemantic/sandisk-pro-blade-station-exporter/internal/protocol"
)

const stationID = "station-000000000000000000000001"

func configForTest() Config {
	return Config{Steps: []Step{{50, 55, 20, 900, 2500}, {55, 60, 37, 2500, 4500}, {60, 65, 50, 3500, 6500}, {65, 70, 100, 5000, 16000}}, Targets: map[string][]string{stationID: {fmt.Sprintf("%064d", 1)}}, HysteresisC: 3, DecreaseDelaySeconds: 60, SettleSeconds: 30, RetrySeconds: 60, LeaseSeconds: 45, SMARTMaxAgeSeconds: 120, SMARTFile: "/metrics"}
}
func TestCurveAndHysteresis(t *testing.T) {
	c := configForTest()
	if e := c.Validate(); e != nil {
		t.Fatal(e)
	}
	for _, tt := range []struct {
		disk, station float64
		level         int
	}{{49, 54, 0}, {50, 54, 1}, {49, 55, 1}, {55, 54, 2}, {50, 60, 2}, {60, 60, 3}, {55, 70, 4}} {
		if got := c.Demand(tt.disk, tt.station); got != tt.level {
			t.Fatalf("%+v got %d", tt, got)
		}
	}
	var d Decision
	n := time.Now()
	if d.Update(c, 55, 55, n) != 2 {
		t.Fatal("upward must be immediate")
	}
	if d.Update(c, 53, 55, n.Add(time.Minute)) != 2 {
		t.Fatal("must reach 3 degree hysteresis")
	}
	if d.Update(c, 52, 55, n.Add(2*time.Minute)) != 2 {
		t.Fatal("must hold for minute")
	}
	if d.Update(c, 52, 55, n.Add(3*time.Minute)) != 1 {
		t.Fatal("must decrease after minute")
	}
	if d.Update(c, 70, 55, n.Add(3*time.Minute)) != 4 {
		t.Fatal("emergency must increase immediately")
	}
}

type fakeTemps struct {
	v   float64
	err error
}

func (f *fakeTemps) Temperature(string, time.Time) (float64, error) { return f.v, f.err }

type fakeFan struct{ commands []int }

func (f *fakeFan) SetFan(_ context.Context, m bool, d byte) error {
	v := int(d)
	if !m {
		v = -1
	}
	f.commands = append(f.commands, v)
	return nil
}
func (f *fakeFan) Close() error { return nil }
func TestReapplyRestoreAndRestart(t *testing.T) {
	cfg := configForTest()
	n := time.Now()
	ls := LeaseStore{t.TempDir(), 45 * time.Second}
	if e := ls.Sweep(n, func(string) error { return nil }); e != nil {
		t.Fatal(e)
	}
	temps := &fakeTemps{v: 51}
	fan := &fakeFan{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := New(cfg, temps, ls, func(string) (Fan, error) { return fan, nil }, log)
	c.Now = func() time.Time { return n }
	dev := device.Device{ID: stationID, Path: "fake"}
	status := &protocol.Status{FanRPM: 1620, Temperatures: [5]int16{56, 50, 50, 40, 40}}
	a := c.Reconcile(context.Background(), dev, protocol.SupportedFirmware, status)
	if !a.Manual || a.AppliedPWM != 20 || fan.commands[0] != 51 {
		t.Fatal(a, fan.commands)
	}
	n = n.Add(time.Second)
	a = c.Reconcile(context.Background(), dev, protocol.SupportedFirmware, status)
	if a.Writes != 2 {
		t.Fatal("must reapply even unchanged PWM")
	}
	n = n.Add(31 * time.Second)
	_ = ls.Sweep(n, func(string) error { return nil })
	a = c.Reconcile(context.Background(), dev, protocol.SupportedFirmware, status)
	if !a.RPMValid {
		t.Fatal("stable measured RPM not accepted", a)
	}
	status.FanRPM = 0
	a = c.Reconcile(context.Background(), dev, protocol.SupportedFirmware, status)
	if a.Manual || a.Healthy || fan.commands[len(fan.commands)-1] != -1 {
		t.Fatal("stall did not restore auto", a, fan.commands)
	}
	// Process restart does not rely on prior in-memory state.
	status.FanRPM = 1620
	n = n.Add(time.Minute)
	_ = ls.Sweep(n, func(string) error { return nil })
	c = New(cfg, temps, ls, func(string) (Fan, error) { return fan, nil }, log)
	c.Now = func() time.Time { return n }
	a = c.Reconcile(context.Background(), dev, protocol.SupportedFirmware, status)
	if !a.Manual || a.AppliedPWM != 20 {
		t.Fatal("restart did not reapply", a)
	}
	temps.err = fmt.Errorf("missing SMART")
	a = c.Reconcile(context.Background(), dev, protocol.SupportedFirmware, status)
	if a.Manual || a.Healthy {
		t.Fatal("missing SMART did not fail safe")
	}
}
func TestWatchdogLease(t *testing.T) {
	ls := LeaseStore{t.TempDir(), 45 * time.Second}
	now := time.Now()
	restored := 0
	restore := func(string) error { restored++; return nil }
	if e := ls.Apply(stationID, now, func() error { return nil }); e == nil {
		t.Fatal("must require watchdog")
	}
	_ = ls.Sweep(now, restore)
	if e := ls.Apply(stationID, now, func() error { return nil }); e != nil {
		t.Fatal(e)
	}
	_ = ls.Sweep(now.Add(44*time.Second), restore)
	if restored != 0 {
		t.Fatal("restored before expiry")
	}
	_ = ls.Sweep(now.Add(46*time.Second), restore)
	if restored != 1 {
		t.Fatal("lost heartbeat did not restore")
	}
	if _, e := os.Stat(ls.path(stationID)); !os.IsNotExist(e) {
		t.Fatal("lease not removed")
	}
}
func TestSMARTIdentityAndFreshness(t *testing.T) {
	dir := t.TempDir()
	sys := filepath.Join(dir, "sys")
	serial := filepath.Join(sys, "class/block/nvme7n1/device")
	_ = os.MkdirAll(serial, 0755)
	_ = os.WriteFile(filepath.Join(serial, "serial"), []byte("test-serial  \n"), 0600)
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte("test-serial")))
	file := filepath.Join(dir, "metrics")
	n := time.Now()
	source := SMARTSource{file, sys, 120 * time.Second, map[string][]string{stationID: {hash}}}
	write := func(stamp float64, available int) {
		t.Helper()
		s := fmt.Sprintf("szew_host_collector_success 1\nszew_host_collector_timestamp_seconds %f\nszew_disk_smart_available{device=\"nvme7n1\"} %d\nszew_disk_temperature_celsius{device=\"nvme7n1\"} 51\n", stamp, available)
		if e := os.WriteFile(file, []byte(s), 0600); e != nil {
			t.Fatal(e)
		}
	}
	write(float64(n.Unix()), 1)
	if v, e := source.Temperature(stationID, n); e != nil || v != 51 {
		t.Fatal(v, e)
	}
	write(float64(n.Add(-121*time.Second).Unix()), 1)
	if _, e := source.Temperature(stationID, n); e == nil {
		t.Fatal("stale data accepted")
	}
	write(float64(n.Unix()), 0)
	if _, e := source.Temperature(stationID, n); e == nil {
		t.Fatal("failed SMART accepted")
	}
	write(float64(n.Unix()), 1)
	_ = os.Remove(filepath.Join(serial, "serial"))
	if _, e := source.Temperature(stationID, n); e == nil {
		t.Fatal("missing disk accepted")
	}
}
