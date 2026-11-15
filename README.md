# SanDisk PRO-BLADE Station exporter

A Linux Prometheus exporter for the **station controller**, written in Go. Reads fan RPM, the station temperature sensor, four slot temperature channels, slot state bits, controller status codes, firmware identity and all seven thermal profiles. No SMART subprocesses, LVM scans, disk reads or firmware updates. Telemetry-only is the default; an explicitly enabled cooling controller can set report-6 PWM.

## Run

```sh
go build -o bin/exporter ./cmd/sandisk-pro-blade-station-exporter
sudo bin/exporter --poll-interval=15s --workers=4
curl http://127.0.0.1:9908/metrics
```

Hardware collection is independent of HTTP requests. All attached stations are discovered through `/sys/class/hidraw` using USB identity `4791:2064`; paths are checked again against the opened device. Station IDs are hashes of the device serial (physical USB location is the fallback); raw serials are not exposed. Optional `--station-aliases='{"station-<hash>":"editing-desk"}'` provides human labels. One exporter per Linux node supports multiple stations with a bounded worker pool, sequential transactions per device, hotplug discovery every poll and cancellable two-second device deadlines.

The initial verified firmware is **2.0.1.2**. Unknown firmware exposes identity and `firmware_supported=0`, not incorrectly decoded temperature readings. The protocol was recovered from the official SanDisk utility and checked against live hardware; see [protocol documentation](docs/protocol.md). It is not an official SanDisk interface contract.

`--poll-interval` defaults to 15s, `--metadata-interval` to 1h and `--max-age` to 45s. A normal cycle sends one GET_STATUS per station; firmware and thermal profiles are refreshed only on discovery or metadata expiry. A failed status read removes sensor data immediately. A stale poller removes data at max-age even if the HTTP server remains responsive. Disconnected devices disappear on the next discovery; other stations continue.

The sensor channels are **not all SMART temperatures**. Channel 0 is the station sensor; channels 1–4 are the controller's slot channels, which may use a module sensor or a board-sensor fallback. An empty slot can still have a board temperature. Channel numbering does not assert the physical bay order. Thermal-table profiles are exported by numerical index; the exporter does not invent a fixed active profile or interpret undocumented status codes as an alarm.

## Prometheus and Kubernetes

Scrape port 9908 every 15s. `/healthz` checks the HTTP process; `/readyz` checks that background discovery is recent, allowing an otherwise healthy node with zero stations. Track discovery/scrape success and sample age separately.

The Helm chart lives in [`presemantic/charts/v2/sandisk-pro-blade-station-exporter`](https://github.com/presemantic/charts/tree/main/v2/sandisk-pro-blade-station-exporter). It runs a DaemonSet with read-only host `/sys` and `/dev` mounts for discovery/hotplug. Kubernetes device cgroup access requires privileged mode with this generic device-discovery deployment; no hostPID, hostNetwork, host root mount or service-account token is used. Telemetry uses allowlisted HID GET request packets. Optional cooling additionally permits report-6 PWM/automatic restore only; no HTTP route offers hardware control.

For a fleet of 100 stations, distribute them across the relevant Linux nodes or increase `workers` on a heavily populated node. Normal reads do not scale with the number of Prometheus clients. Tests cover discovery and independent collection of 100 simulated devices. This is not a claim of a 100-device hardware benchmark.

## Build and verification

GitHub Actions runs race tests, vet, a protocol fuzz smoke test and publishes Linux amd64/arm64 images to the existing private registry `europe-west1-docker.pkg.dev/prod-common-cicd/presemantic-docker/sandisk-pro-blade-station-exporter:<commit>` using the organization’s existing `GCP_SA_ARTIFACT_REGISTRY_KEY` secret. No local build is published. The chart's `appVersion` selects the immutable image version.

```sh
go test -race ./...
go vet ./...
go test ./internal/protocol -run=^$ -fuzz=FuzzDecode -fuzztime=10s
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build ./cmd/sandisk-pro-blade-station-exporter
```

The binary needs Linux only for hardware access; decoding, discovery fixtures and collection tests also run on macOS.

## Optional cooling control

`--control-config=/etc/problade/control.json` enables the reviewed host-side curve. It does not modify the station thermal tables or firmware. Both disk SMART and station-internal temperature thresholds apply; the higher requested PWM wins. Below both first thresholds the target is 0%. Upward changes are immediate; downshifts require both temperatures below the current step by `hysteresisC` continuously for `decreaseDelaySeconds`.

The Helm `control` section supplies JSON configuration, the read-only existing host `szew-host.prom` file, and explicit station hash -> SHA256 disk-serial mappings. Linux `nvmeN` enumeration can change; disk identity is matched from sysfs serial hashes, never a fixed device name. Missing disks, missing/stale/invalid SMART, invalid station readings, an unavailable watchdog, write failures or out-of-band RPM after the settling interval return that station to factory automatic control. Retry is delayed and revalidates inputs. Configurations and bindings are reloaded on process startup; ConfigMap checksums trigger a controlled Pod rollout. Devices are discovered every cycle, and the current target is reissued every cycle, including 0%, so station power cycling cannot silently discard the setting forever.

PWM metrics are **commands**, not hardware readback: the controller exposes desired PWM, last successful command, target level, actual RPM verification, input health, writes, failures and automatic fallbacks. RPM bounds validate a band rather than claiming an exact PWM/RPM mapping; the fan cannot sustain arbitrarily low RPM. Tune bands using observed hardware. The default curve is 20/37/50/100% at disk 50/55/60/65C or internal 55/60/65/70C, with a 3C/60s downshift hold. Only explicitly configured stations receive control; others remain telemetry-only.

A Kubernetes native watchdog sidecar starts first and stops last. Manual commands require its heartbeat and atomically renew a 45-second lease. If the exporter hangs/exits without cleanup, the watchdog restores factory automatic operation after lease expiry. The shared lease lock prevents races between renewal and restore. Normal shutdown restores automatically too. **A complete host/kernel failure, simultaneous loss of both processes, physical disconnection or power loss cannot be covered by a userspace watchdog.** The station has no confirmed hardware auto-restore timeout; power-up initializes its factory control, then the controller reapplies the configured curve when fresh telemetry is available.
