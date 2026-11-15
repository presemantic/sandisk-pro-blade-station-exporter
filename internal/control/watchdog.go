package control

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/presemantic/sandisk-pro-blade-station-exporter/internal/device"
)

// RunWatchdog must run in a separate process from the controller.
// On orderly sidecar shutdown it restores every armed target before exiting.
func RunWatchdog(ctx context.Context, store LeaseStore, sysRoot, devRoot string, open Open, log *slog.Logger) error {
	if e := os.MkdirAll(store.Dir, 0700); e != nil {
		return e
	}
	restore := func(id string) error {
		devices, e := device.Discover(sysRoot, devRoot)
		if e != nil {
			return e
		}
		for _, d := range devices {
			if d.ID != id {
				continue
			}
			f, e := open(d.Path)
			if e != nil {
				return e
			}
			defer f.Close()
			c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			e = f.SetFan(c, false, 0)
			if e == nil {
				log.Warn("watchdog restored firmware automatic mode", "station", id)
			}
			return e
		}
		// Keep the lease until the same physical station reconnects.
		return os.ErrNotExist
	}
	for {
		if e := store.Sweep(time.Now(), restore); e != nil && !errors.Is(e, os.ErrNotExist) {
			log.Error("watchdog sweep failed", "error", e)
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = os.Remove(filepath.Join(store.Dir, "watchdog.ready"))
			// Expire all leases only on exit, without depending on the main process.
			err := store.Sweep(time.Now().Add(24*time.Hour), restore)
			_ = os.Remove(filepath.Join(store.Dir, "watchdog.ready"))
			return err
		case <-timer.C:
		}
	}
}
