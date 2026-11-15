package exporter

import (
	"fmt"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const prefix = "sandisk_pro_blade_station_"

var stationLabels = []string{"station", "name"}

type Collector struct {
	manager *Manager
	descs   map[string]*prometheus.Desc
}

func NewCollector(m *Manager) *Collector {
	c := &Collector{manager: m, descs: map[string]*prometheus.Desc{}}
	for _, x := range []struct {
		name, help string
		labels     []string
	}{
		{"discovery_success", "Whether station discovery completed successfully.", nil},
		{"discovery_errors_total", "Failed discovery cycles since exporter startup.", nil},
		{"discovered_count", "Stations discovered on this node.", nil},
		{"collection_cycles_total", "Completed collection cycles since exporter startup.", nil},
		{"collection_duration_seconds", "Wall-clock duration of the last collection cycle.", nil},
		{"present", "Station was present during the most recent successful discovery.", stationLabels},
		{"scrape_success", "Latest station protocol collection succeeded and is fresh.", stationLabels},
		{"scrape_errors_total", "Failed station collection cycles.", stationLabels},
		{"scrape_duration_seconds", "Duration of the last station collection.", stationLabels},
		{"last_success_timestamp_seconds", "Time of last successful station status read.", stationLabels},
		{"sample_age_seconds", "Age of the last successful station status read.", stationLabels},
		{"metadata_success", "Firmware and all seven thermal profiles were read successfully.", stationLabels},
		{"metadata_age_seconds", "Age of the firmware and thermal-table snapshot.", stationLabels},
		{"firmware_supported", "Firmware matches the explicitly validated protocol revision.", stationLabels},
		{"info", "Station identity with firmware version. Serial number is never exposed.", appendLabels("firmware")},
		{"request_total", "Hardware GET requests by kind since exporter startup.", appendLabels("command")},
		{"request_errors_total", "Failed hardware GET requests by kind since exporter startup.", appendLabels("command")},
		{"fan_speed_rpm", "Fan tachometer speed reported by the station controller.", stationLabels},
		{"fan_running", "One when the measured fan tachometer is above zero.", stationLabels},
		{"temperature_celsius", "Station sensor (0) and four slot sensor channels (1..4). Physical bay order is unverified.", appendLabels("sensor")},
		{"control_input_state", "Controller GPIO-derived input state; not a decoded cooling-mode enum.", stationLabels},
		{"enclosure_state_code", "Controller enclosure-state code; raw firmware enum.", stationLabels},
		{"slot_state_mask", "Controller slot state bitmask.", stationLabels},
		{"slot_present", "Slot presence bit from the controller, indexed in firmware channel order.", appendLabels("slot")},
		{"slot_status_flag", "Additional slot status flag; raw bit, not a drive failure assertion.", appendLabels("slot")},
		{"status_byte", "Raw numerical status payload bytes for protocol diagnostics.", appendLabels("offset")},
		{"firmware_byte", "Raw firmware GET payload bytes for protocol diagnostics.", appendLabels("offset")},
		{"thermal_threshold_celsius", "Thermal profile thresholds by profile, stage and sensor; profile selection depends on internal controller state.", appendLabels("profile", "stage", "sensor")},
		{"control_enabled", "Custom cooling configured for this station.", stationLabels},
		{"control_manual", "Last confirmed fan command enabled custom manual PWM.", stationLabels},
		{"control_healthy", "Latest control cycle used fresh inputs and issued its target successfully.", stationLabels},
		{"control_desired_pwm_percent", "Desired PWM percent from the configured curve; not measured hardware readback.", stationLabels},
		{"control_applied_pwm_percent", "Last successfully written PWM command; not measured hardware readback.", stationLabels},
		{"control_rpm_valid", "Measured tachometer is inside the configured target band after settling.", stationLabels},
		{"control_level", "Current curve level, zero for stopped.", stationLabels},
		{"control_disk_temperature_celsius", "Maximum fresh SMART temperature of disks explicitly assigned to this station.", stationLabels},
		{"control_writes_total", "Successful custom PWM command writes.", stationLabels},
		{"control_errors_total", "Failed control or input validation cycles.", stationLabels},
		{"control_fallbacks_total", "Successful returns to firmware automatic mode by the controller.", stationLabels},
	} {
		c.descs[x.name] = prometheus.NewDesc(prefix+x.name, x.help, x.labels, nil)
	}
	return c
}
func appendLabels(extra ...string) []string {
	return append(append([]string{}, stationLabels...), extra...)
}
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range c.descs {
		ch <- d
	}
}
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	snapshot := c.manager.Snapshot()
	now := time.Now()
	emit := func(name string, kind prometheus.ValueType, value float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(c.descs[name], kind, value, labels...)
	}
	gauge := func(name string, value float64, labels ...string) {
		emit(name, prometheus.GaugeValue, value, labels...)
	}
	gauge("discovery_success", boolValue(snapshot.DiscoveryOK))
	gauge("discovered_count", float64(len(snapshot.Stations)))
	gauge("collection_duration_seconds", snapshot.Duration.Seconds())
	emit("discovery_errors_total", prometheus.CounterValue, float64(snapshot.DiscoveryErrors))
	emit("collection_cycles_total", prometheus.CounterValue, float64(snapshot.Cycles))
	for _, s := range snapshot.Stations {
		labels := []string{s.Device.ID, s.Alias}
		age := now.Sub(s.At)
		fresh := s.OK && !s.At.IsZero() && age <= c.manager.MaxAge() && snapshot.DiscoveryOK
		gauge("present", boolValue(snapshot.DiscoveryOK), labels...)
		gauge("scrape_success", boolValue(fresh), labels...)
		gauge("scrape_duration_seconds", s.Duration.Seconds(), labels...)
		emit("scrape_errors_total", prometheus.CounterValue, float64(s.Errors), labels...)
		gauge("firmware_supported", boolValue(s.Supported), labels...)
		gauge("metadata_success", boolValue(s.MetadataOK), labels...)
		ctrl := s.Control
		gauge("control_enabled", boolValue(ctrl.Enabled), labels...)
		if ctrl.Enabled {
			gauge("control_manual", boolValue(ctrl.Manual), labels...)
			gauge("control_healthy", boolValue(ctrl.Healthy && fresh), labels...)
			gauge("control_desired_pwm_percent", float64(ctrl.DesiredPWM), labels...)
			gauge("control_applied_pwm_percent", float64(ctrl.AppliedPWM), labels...)
			gauge("control_rpm_valid", boolValue(ctrl.RPMValid && fresh), labels...)
			gauge("control_level", float64(ctrl.Level), labels...)
			if ctrl.Healthy && fresh {
				gauge("control_disk_temperature_celsius", ctrl.DiskC, labels...)
			}
			emit("control_writes_total", prometheus.CounterValue, float64(ctrl.Writes), labels...)
			emit("control_errors_total", prometheus.CounterValue, float64(ctrl.Errors), labels...)
			emit("control_fallbacks_total", prometheus.CounterValue, float64(ctrl.Fallbacks), labels...)
		}
		if !s.At.IsZero() {
			gauge("last_success_timestamp_seconds", float64(s.At.UnixNano())/1e9, labels...)
			gauge("sample_age_seconds", max(0, age.Seconds()), labels...)
		}
		if s.Firmware.Version != "" {
			gauge("info", 1, append(labels, s.Firmware.Version)...)
			for i, v := range s.Firmware.Raw {
				gauge("firmware_byte", float64(v), append(labels, strconv.Itoa(i))...)
			}
		}
		for i, name := range []string{"firmware", "status", "thermal_table"} {
			l := append(labels, name)
			emit("request_total", prometheus.CounterValue, float64(s.Requests[i]), l...)
			emit("request_errors_total", prometheus.CounterValue, float64(s.RequestErrors[i]), l...)
		}
		if !s.MetadataAt.IsZero() {
			gauge("metadata_age_seconds", max(0, now.Sub(s.MetadataAt).Seconds()), labels...)
		}
		if s.MetadataOK && snapshot.DiscoveryOK {
			for profile, table := range s.Tables {
				for stage, row := range table {
					for sensor, value := range row {
						gauge("thermal_threshold_celsius", float64(value), append(labels, strconv.Itoa(profile), strconv.Itoa(stage), strconv.Itoa(sensor))...)
					}
				}
			}
		}
		if !fresh {
			continue
		}
		gauge("fan_speed_rpm", float64(s.Status.FanRPM), labels...)
		gauge("fan_running", boolValue(s.Status.FanRPM > 0), labels...)
		for sensor, t := range s.Status.Temperatures {
			if t >= -40 && t <= 150 {
				gauge("temperature_celsius", float64(t), append(labels, strconv.Itoa(sensor))...)
			}
		}
		gauge("control_input_state", float64(s.Status.ControlInput), labels...)
		gauge("enclosure_state_code", float64(s.Status.EnclosureState), labels...)
		gauge("slot_state_mask", float64(s.Status.SlotState), labels...)
		for slot := range 4 {
			l := append(labels, strconv.Itoa(slot+1))
			gauge("slot_present", boolValue(s.Status.SlotState&(1<<uint(slot+4)) != 0), l...)
			gauge("slot_status_flag", boolValue(s.Status.SlotState&(1<<uint(slot)) != 0), l...)
		}
		for i, v := range s.Status.Raw {
			gauge("status_byte", float64(v), append(labels, fmt.Sprint(i))...)
		}
	}
}
func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
