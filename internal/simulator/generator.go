// Package simulator generates explicitly simulated development observations.
// Production collectors never import or execute this package.
package simulator

import (
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"time"

	"InfraPulseAgent/internal/model"
)

var scenarios = []string{"healthy", "cpu-increase", "memory-pressure", "traffic-increase", "interface-errors", "interface-flap", "device-recovery", "topology-change", "log-burst", "audit-activity", "sip-latency", "sip-recovery", "all"}

type Options struct {
	Scenario string
	Duration time.Duration
	Interval time.Duration
	Seed     int64
	Devices  int
	Start    time.Time
}

func Scenarios() []string { return append([]string(nil), scenarios...) }

func (o Options) WithDefaults() Options {
	if o.Scenario == "" {
		o.Scenario = "healthy"
	}
	if o.Duration == 0 {
		o.Duration = 30 * time.Second
	}
	if o.Interval == 0 {
		o.Interval = time.Second
	}
	if o.Devices == 0 {
		o.Devices = 4
	}
	if o.Start.IsZero() {
		o.Start = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	return o
}
func (o Options) Validate() error {
	o = o.WithDefaults()
	known := false
	for _, scenario := range scenarios {
		if o.Scenario == scenario {
			known = true
		}
	}
	if !known || o.Duration < 10*time.Millisecond || o.Duration > 24*time.Hour || o.Interval < 10*time.Millisecond || o.Interval > time.Hour || o.Devices < 1 || o.Devices > 64 || o.Start.Year() < 1970 || o.Start.Year() > 9998 {
		return errors.New("invalid simulator options: known scenario, duration 10ms..24h, interval 10ms..1h, devices 1..64 and valid start are required")
	}
	return nil
}

type Generator struct{ options Options }

func NewGenerator(options Options) (*Generator, error) {
	options = options.WithDefaults()
	if err := options.Validate(); err != nil {
		return nil, err
	}
	return &Generator{options: options}, nil
}
func (g *Generator) Options() Options { return g.options }
func (g *Generator) Steps() uint64    { return uint64((g.options.Duration-1)/g.options.Interval) + 1 }
func (g *Generator) enabled(s string) bool {
	return g.options.Scenario == s || g.options.Scenario == "all"
}
func deviceID(index int) string { return fmt.Sprintf("simulated:device-%03d", index+1) }

func (g *Generator) source(index int, kind model.SourceType) model.SourceRef {
	return model.SourceRef{Type: kind, TargetID: fmt.Sprintf("simulated:target-%03d", index+1), Protocol: "simulator", InstanceID: g.options.Scenario, Provenance: model.ProvenanceSimulated}
}

func (g *Generator) fleetSource(kind model.SourceType, target string) model.SourceRef {
	return model.SourceRef{Type: kind, TargetID: target, Protocol: "simulator", InstanceID: g.options.Scenario, Provenance: model.ProvenanceSimulated}
}
func (g *Generator) uptime(step uint64) float64 {
	return 3600 + float64(step)*g.options.Interval.Seconds()
}

// Tick is pure: repeat calls with identical options and step return identical
// normalized payloads. Metadata such as transport/session UUIDs is assigned by
// the runner separately. Out-of-duration steps produce no observations.
func (g *Generator) Tick(step uint64) []model.Envelope {
	if step >= g.Steps() {
		return nil
	}
	now := g.options.Start.Add(time.Duration(step) * g.options.Interval).UTC()
	var envelopes []model.Envelope
	devices := make([]model.Device, 0, g.options.Devices)
	for i := 0; i < g.options.Devices; i++ {
		online := g.online(step, i)
		lastSeen := now
		if !online {
			lastSeen = g.options.Start.Add(g.options.Interval).UTC() // last online step 1
		}
		devices = append(devices, model.Device{ID: deviceID(i), Source: "simulated", Name: fmt.Sprintf("Simulated device %d", i+1), Hostname: fmt.Sprintf("sim-device-%03d", i+1), ManagementAddress: fmt.Sprintf("192.0.2.%d", i+1), Vendor: "simulated", Model: "development-simulator", Type: "switch", Driver: "simulated", OS: "simulated", Online: online, LastSeen: lastSeen, UptimeSeconds: g.uptime(step), UptimeSource: "simulated", Capabilities: []model.Capability{{Name: "inventory.read", Supported: true}, {Name: "telemetry.read", Supported: true}, {Name: "topology.lldp", Supported: true}}})
		devices[len(devices)-1] = model.ObserveDevice(devices[len(devices)-1], g.source(i, model.SourceSNMP), now, now)
		if online {
			envelopes = append(envelopes, model.ObserveEnvelope(model.Envelope{Telemetry: g.telemetry(step, i, now)}, g.source(i, model.SourceSNMP), now))
		}
	}
	inv := model.ObserveEnvelope(model.Envelope{Inventory: &model.InventoryUpdate{ObservedAt: now, Devices: devices}}, g.fleetSource(model.SourceLocal, "simulated:fleet"), now)
	envelopes = append([]model.Envelope{inv}, envelopes...)
	for i := 0; i < g.options.Devices; i++ {
		envelopes = append(envelopes, model.ObserveEnvelope(model.Envelope{Topology: g.topology(step, i, now)}, g.source(i, model.SourceSNMP), now))
	}
	if records := g.logs(step, now); len(records) > 0 {
		envelopes = append(envelopes, model.ObserveEnvelope(model.Envelope{Logs: &model.LogBatch{ObservedAt: now, Source: "simulated", Records: records}}, g.fleetSource(model.SourceSyslog, "simulated:logs"), now))
	}
	targets := make([]model.TelephonyHealth, 0, g.options.Devices)
	for i := 0; i < g.options.Devices; i++ {
		targets = append(targets, g.sip(step, i, now))
	}
	envelopes = append(envelopes, model.ObserveEnvelope(model.Envelope{Telephony: &model.TelephonyUpdate{ObservedAt: now, Source: "simulated", Targets: targets}}, g.fleetSource(model.SourceSIP, "simulated:sip-fleet"), now))
	if events := g.events(step, now); len(events) > 0 {
		envelopes = append(envelopes, model.ObserveEnvelope(model.Envelope{Events: events}, g.source(0, model.SourceSNMP), now))
	}
	return envelopes
}

func (g *Generator) variation(index int) uint64 {
	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "%d:%d", g.options.Seed, index)
	return h.Sum64() % 5
}
func level(step uint64, count int) int {
	if step >= uint64(count) {
		return count - 1
	}
	return int(step)
}

var cpuTrend = []float64{20, 30, 45, 60, 80, 95}
var errorTrend = []uint64{0, 3, 12, 50, 200}
var sipTrend = []float64{20, 50, 120, 400}

func (g *Generator) online(step uint64, index int) bool {
	return !(g.enabled("device-recovery") && index == 0 && step >= 2 && step < 5)
}
func (g *Generator) interfaceUp(step uint64, index int) bool {
	return !(g.enabled("interface-flap") && index == 0 && (step == 2 || step == 4))
}
func (g *Generator) telemetry(step uint64, index int, now time.Time) *model.TelemetrySample {
	cpu := 20 + float64(g.variation(index))
	if g.enabled("cpu-increase") {
		cpu = cpuTrend[level(step, len(cpuTrend))]
	}
	total := uint64(512 << 20)
	percent := uint64(30 + g.variation(index))
	if g.enabled("memory-pressure") {
		percent = []uint64{30, 40, 55, 70, 85, 95}[level(step, 6)]
	}
	used := total * percent / 100
	rate := float64(100_000 + g.variation(index)*1000)
	if g.enabled("traffic-increase") {
		rate = []float64{100_000, 200_000, 400_000, 800_000, 1_600_000, 3_200_000}[level(step, 6)]
	}
	errors := uint64(0)
	if g.enabled("interface-errors") {
		errors = errorTrend[level(step, len(errorTrend))]
	}
	// Integrate a deterministic traffic trend so counters remain monotonic even
	// after the trend plateaus. Units are bytes and bytes per second.
	seconds := g.options.Interval.Seconds()
	bytes := float64(1_000_000) + rate*float64(step)*seconds
	if g.enabled("traffic-increase") {
		bytes = 1_000_000
		trend := []float64{100_000, 200_000, 400_000, 800_000, 1_600_000, 3_200_000}
		for i := uint64(1); i <= step && i < uint64(len(trend)); i++ {
			bytes += trend[i] * seconds
		}
		if step >= uint64(len(trend)) {
			bytes += trend[len(trend)-1] * float64(step-uint64(len(trend))+1) * seconds
		}
	}
	up := g.interfaceUp(step, index)
	state := "up"
	if !up {
		state = "down"
	}
	available := model.CounterAvailability{RXBytes: true, TXBytes: true, RXPackets: true, TXPackets: true, RXErrors: true, TXErrors: true, RXDropped: true, TXDropped: true}
	errorRate := float64(0)
	if step > 0 && g.enabled("interface-errors") {
		errorRate = float64(errors-errorTrend[level(step-1, len(errorTrend))]) / seconds
	}
	iface := model.Interface{ID: "simulated:if-1", Index: 1, Name: "sim-eth0", AdminUp: true, OperState: state, MTU: 1500, MAC: fmt.Sprintf("02:00:00:00:00:%02x", index+1), Addresses: []string{fmt.Sprintf("192.0.2.%d/24", index+1)}, Counters: model.Counters{RXBytes: uint64(bytes), TXBytes: uint64(bytes / 2), RXPackets: uint64(bytes / 800), TXPackets: uint64(bytes / 1600), RXErrors: errors, TXErrors: errors / 2}, Rates: &model.Rates{RXBytesPerSecond: rate, TXBytesPerSecond: rate / 2, RXPacketsPerSecond: rate / 800, TXPacketsPerSecond: rate / 1600, RXErrorsPerSecond: errorRate, TXErrorsPerSecond: errorRate / 2, Available: &available}, Metadata: &model.InterfaceMetadata{Index: true, MTU: true, AdminStatus: true}, CounterMetadata: &model.CounterMetadata{Available: available, Widths: model.CounterWidths{RXBytes: 64, TXBytes: 64, RXPackets: 64, TXPackets: 64, RXErrors: 64, TXErrors: 64, RXDropped: 64, TXDropped: 64}}}
	if !up {
		iface.Rates.RXBytesPerSecond = 0
		iface.Rates.TXBytesPerSecond = 0
		iface.Rates.RXPacketsPerSecond = 0
		iface.Rates.TXPacketsPerSecond = 0
	}
	pressure := float64(0)
	if g.enabled("memory-pressure") {
		pressure = math.Max(0, float64(percent)-70)
	}
	return &model.TelemetrySample{DeviceID: deviceID(index), ObservedAt: now, CollectedAt: now, Source: "simulated", BootID: "simulated:boot", UptimeSeconds: g.uptime(step), UptimeSource: "simulated", CPU: &model.CPUStats{LogicalCPUs: 2, Model: "simulated", UsagePercent: &cpu}, Memory: &model.MemoryStats{TotalBytes: total, UsedBytes: used, AvailableBytes: total - used, FreeBytes: total - used, PressureSomeAvg10: &pressure}, Interfaces: []model.Interface{iface}}
}

func (g *Generator) topology(step uint64, index int, now time.Time) *model.TopologyUpdate {
	snapshot := &model.TopologyUpdate{DeviceID: deviceID(index), ObservedAt: now, CollectedAt: now, Source: "simulated", Completeness: "complete", TTLSeconds: 30, ExpiryAssumption: "simulated scenario lifetime"}
	windowStart, windowEnd := uint64(2), uint64(5)
	if g.options.Scenario == "all" {
		windowStart, windowEnd = 5, 8 // recovery precedes physical-link evidence
	}
	if g.options.Devices < 2 || !g.online(step, index) || g.enabled("topology-change") && (step < windowStart || step >= windowEnd) {
		return snapshot
	}
	next := (index + 1) % g.options.Devices
	if !g.online(step, next) {
		return snapshot
	}
	snapshot.Observations = []model.Neighbor{{LocalDeviceID: deviceID(index), LocalInterfaceID: "simulated:if-1", LocalInterfaceName: "sim-eth0", LocalPortNumber: 1, LocalPortID: []byte("sim-eth0"), LocalPortSubtype: 5, RemoteChassisID: []byte(fmt.Sprintf("sim-device-%03d", next+1)), RemoteChassisSubtype: 7, RemotePortID: []byte("sim-eth0"), RemotePortSubtype: 5, RemoteSystemName: fmt.Sprintf("sim-device-%03d", next+1), Protocol: "lldp", Source: "simulated", ObservedAt: now, ExpiresAt: now.Add(30 * time.Second), Confidence: "high", Evidence: []model.Evidence{{Source: "simulated", Value: "deterministic development topology", Confidence: "high"}}}}
	return snapshot
}

func (g *Generator) logs(step uint64, now time.Time) []model.LogRecord {
	count := 1
	if g.enabled("log-burst") && step >= 2 && step <= 4 {
		count = 128
	}
	records := make([]model.LogRecord, 0, count+1)
	severity := uint32(6)
	facility := uint32(1)
	for i := 0; i < count; i++ {
		records = append(records, model.LogRecord{ID: fmt.Sprintf("simulated:log:%d:%d", step, i), DeviceID: deviceID(i % g.options.Devices), Source: "simulated", SourceID: "simulated:system-log", ObservedAt: now, OriginalTimestamp: &now, Severity: &severity, Facility: &facility, Host: fmt.Sprintf("sim-device-%03d", i%g.options.Devices+1), Process: "simulator", Message: fmt.Sprintf("Simulated system log step=%d record=%d seed=%d", step, i, g.options.Seed), Offset: int64(step)*128 + int64(i)})
	}
	if g.enabled("audit-activity") && step >= 2 && step <= 4 {
		uid := uint32(1000)
		pid := uint32(500 + step)
		success := false
		records = append(records, model.LogRecord{ID: fmt.Sprintf("simulated:audit:%d", step), DeviceID: deviceID(0), Source: "simulated", SourceID: "simulated:audit", ObservedAt: now, OriginalTimestamp: &now, Message: "Simulated denied execution for audit analysis fixture", Raw: fmt.Sprintf("type=SYSCALL msg=audit(%d.000:%d): uid=1000 auid=1000 pid=%d syscall=59 success=no exe=\"/simulated/denied\"", now.Unix(), step+1, pid), Audit: &model.AuditRecord{EventID: fmt.Sprintf("%d.000:%d", now.Unix(), step+1), RecordType: "SYSCALL", UID: &uid, AUID: &uid, PID: &pid, Executable: "/simulated/denied", Syscall: "59", Success: &success, Result: "denied"}})
	}
	return records
}

func (g *Generator) sip(step uint64, index int, now time.Time) model.TelephonyHealth {
	code := uint32(200)
	latency := 20 + float64(g.variation(index))
	state := "up"
	category := ""
	reachable := true
	failures := uint32(0)
	if g.enabled("sip-latency") {
		latency = sipTrend[level(step, len(sipTrend))]
		if step >= 4 && (g.options.Scenario != "all" || step < 7) {
			reachable = false
			state = "degraded"
			category = "timeout"
			failures = uint32(min(step-3, 100))
			if failures >= 3 {
				state = "down"
			}
		} else if g.options.Scenario == "all" && step >= 7 {
			latency = 20
		} else if latency > 250 {
			state = "degraded"
			category = "high_latency"
		}
	}
	if g.options.Scenario == "sip-recovery" && step >= 2 && step < 5 {
		reachable = false
		state = "degraded"
		category = "timeout"
		failures = uint32(step - 1)
		if failures >= 3 {
			state = "down"
		}
	}
	h := model.TelephonyHealth{TargetID: fmt.Sprintf("simulated:sip-%03d", index+1), Address: fmt.Sprintf("192.0.2.%d:5060", index+1), Transport: "udp", Reachable: reachable, ObservedAt: now, ConsecutiveFailures: failures, State: state, ErrorCategory: category, Source: "simulated"}
	h.Observation = model.NewObservation(g.fleetSource(model.SourceSIP, h.TargetID), h.Source, now, now, nil)
	if reachable {
		h.ResponseCode = &code
		h.LatencyMillis = &latency
	}
	return h
}

func (g *Generator) events(step uint64, now time.Time) []model.Event {
	var events []model.Event
	add := func(kind, severity, message string) {
		events = append(events, model.Event{ID: fmt.Sprintf("simulated:event:%s:%d", kind, step), Type: kind, DeviceID: deviceID(0), Timestamp: now, Severity: severity, Message: message, Source: "simulated"})
	}
	if step == 0 {
		add("simulator.scenario.started", "info", "Simulated scenario "+g.options.Scenario+" started")
		return events
	}
	if g.online(step, 0) != g.online(step-1, 0) {
		if g.online(step, 0) {
			add("device.online", "info", "Simulated device recovered")
		} else {
			add("device.offline", "warning", "Simulated device lost connectivity")
		}
	}
	if g.interfaceUp(step, 0) != g.interfaceUp(step-1, 0) {
		if g.interfaceUp(step, 0) {
			add("interface.up", "info", "Simulated interface is up")
		} else {
			add("interface.down", "warning", "Simulated interface is down")
		}
	}
	currentTopology := g.topology(step, 0, now)
	previousTopology := g.topology(step-1, 0, now)
	if len(currentTopology.Observations) > 0 && len(previousTopology.Observations) == 0 {
		add("topology.link.appeared", "info", "Simulated topology link appeared")
	} else if len(currentTopology.Observations) == 0 && len(previousTopology.Observations) > 0 {
		add("topology.link.disappeared", "warning", "Simulated topology link disappeared")
	}
	current, previous := g.sip(step, 0, now), g.sip(step-1, 0, now)
	if current.State != previous.State {
		severity := "warning"
		if current.State == "up" {
			severity = "info"
		}
		if current.State == "down" {
			severity = "error"
		}
		add("telephony."+current.State, severity, "Simulated SIP target simulated:sip-001 state changed to "+current.State)
		events[len(events)-1].DeviceID = ""
		events[len(events)-1].Observation = model.CloneObservation(current.Observation)
	}
	return events
}
