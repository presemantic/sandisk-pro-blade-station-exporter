package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/presemantic/sandisk-pro-blade-station-exporter/internal/control"
	"github.com/presemantic/sandisk-pro-blade-station-exporter/internal/device"
	"github.com/presemantic/sandisk-pro-blade-station-exporter/internal/exporter"
	"github.com/presemantic/sandisk-pro-blade-station-exporter/internal/hid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		slog.Error("exporter stopped", "error", err)
		os.Exit(1)
	}
}
func run() error {
	address := flag.String("web.listen-address", ":9908", "HTTP metrics address")
	interval := flag.Duration("poll-interval", 15*time.Second, "Interval between hardware polling cycles")
	metadata := flag.Duration("metadata-interval", time.Hour, "Refresh period for firmware and thermal tables")
	timeout := flag.Duration("device-timeout", 2*time.Second, "Maximum wait per HID request")
	age := flag.Duration("max-age", 45*time.Second, "Stop exposing sensor values after this age")
	workers := flag.Int("workers", 4, "Concurrent stations per polling cycle (1..64)")
	sys := flag.String("sysfs-path", "/sys", "Sysfs root for HID discovery")
	dev := flag.String("device-path", "/dev", "Directory containing hidraw devices")
	aliases := flag.String("station-aliases", "{}", "JSON object mapping hashed station IDs to display names")
	controlFile := flag.String("control-config", "", "Opt-in fan-control JSON configuration")
	controlState := flag.String("control-state-path", "/run/problade-control", "Shared control/watchdog lease directory")
	watchdog := flag.Bool("control-watchdog", false, "Run only the independent cooling watchdog")
	watchdogHealth := flag.Bool("control-watchdog-health", false, "Check the independent watchdog heartbeat and exit")
	showVersion := flag.Bool("version", false, "Print build version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return nil
	}
	if *watchdogHealth {
		return (control.LeaseStore{Dir: *controlState}).Ready(time.Now())
	}
	if *timeout <= 0 || *timeout > *interval {
		return fmt.Errorf("device-timeout must be positive and <= poll-interval")
	}
	config := exporter.Config{Interval: *interval, MetadataInterval: *metadata, MaxAge: *age, Workers: *workers, Aliases: map[string]string{}}
	if err := json.Unmarshal([]byte(*aliases), &config.Aliases); err != nil {
		return fmt.Errorf("station-aliases: %w", err)
	}
	if err := config.Validate(); err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	openFan := func(path string) (control.Fan, error) { return hid.Open(path, *timeout) }
	if *watchdog {
		return control.RunWatchdog(ctx, control.LeaseStore{Dir: *controlState}, *sys, *dev, openFan, logger)
	}
	manager := exporter.New(config, func() ([]device.Device, error) { return device.Discover(*sys, *dev) }, func(path string) (exporter.Transport, error) { return hid.Open(path, *timeout) }, logger)
	var controller *control.Controller
	if *controlFile != "" {
		cfg, err := control.Load(*controlFile)
		if err != nil {
			return fmt.Errorf("control configuration: %w", err)
		}
		if *interval > time.Duration(cfg.LeaseSeconds/2)*time.Second {
			return fmt.Errorf("poll interval must be at most half the control lease")
		}
		source := control.SMARTSource{File: cfg.SMARTFile, SysRoot: *sys, MaxAge: time.Duration(cfg.SMARTMaxAgeSeconds) * time.Second, Targets: cfg.Targets}
		controller = control.New(cfg, source, control.LeaseStore{Dir: *controlState, TTL: time.Duration(cfg.LeaseSeconds) * time.Second}, openFan, logger)
		manager.SetController(controller)
	}
	registry := prometheus.NewRegistry()
	registry.MustRegister(exporter.NewCollector(manager), collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "sandisk_pro_blade_station_build_info", Help: "Exporter build version.", ConstLabels: prometheus.Labels{"version": version}}, func() float64 { return 1 }))
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{MaxRequestsInFlight: 4, Timeout: 5 * time.Second}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK); fmt.Fprintln(w, "ok") })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if controller != nil {
			if e := controller.Leases.Ready(time.Now()); e != nil {
				http.Error(w, "cooling watchdog unavailable", http.StatusServiceUnavailable)
				return
			}
		}
		s := manager.Snapshot()
		if s.At.IsZero() || time.Since(s.At) > config.MaxAge {
			http.Error(w, "no recent collection cycle", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ready")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintln(w, "SanDisk PRO-BLADE Station exporter: /metrics /healthz /readyz")
	})
	server := &http.Server{Addr: *address, Handler: mux, ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192}
	done := make(chan struct{})
	go func() { defer close(done); manager.Run(ctx) }()
	errors := make(chan error, 1)
	go func() { errors <- server.ListenAndServe() }()
	logger.Info("starting exporter", "version", version, "address", *address, "interval", interval.String(), "workers", *workers)
	var serveErr error
	select {
	case err := <-errors:
		if err != http.ErrServerClosed {
			serveErr = err
		}
	case <-ctx.Done():
	}
	cancel()
	<-done
	if controller != nil {
		restore, finish := context.WithTimeout(context.Background(), 5*time.Second)
		controller.Stop(restore)
		finish()
	}
	shutdown, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShutdown()
	if err := server.Shutdown(shutdown); err != nil {
		return err
	}
	return serveErr
}
