package agent

import (
	"context"
	"encoding/json"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"InfraPulseAgent/internal/config"
	"InfraPulseAgent/internal/credentials"
	"InfraPulseAgent/internal/mockcontroller"
	"InfraPulseAgent/internal/model"
	"github.com/gosnmp/gosnmp"
)

// This is a protocol fixture, not a substitute production driver. The real
// credential provider, UDP adapter, MIB driver, rate tracker, registry and gRPC
// mapping execute end to end against bounded test-only SNMP responses.
func TestAgentSNMPUDPTopologyRatesOfflineRecovery(t *testing.T) {
	const secret = "snmp-e2e-private-community"
	t.Setenv("INFRAPULSE_SNMP_E2E_COMMUNITY", secret)
	started := time.Now()
	var unavailable atomic.Bool
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		buf := make([]byte, 65535)
		decoder := &gosnmp.GoSNMP{Version: gosnmp.Version2c}
		for {
			n, peer, e := conn.ReadFrom(buf)
			if e != nil {
				return
			}
			if unavailable.Load() {
				continue
			}
			request, e := decoder.SnmpDecodePacket(buf[:n])
			if e != nil || request.Community != secret {
				continue
			}
			values := snmpE2EValues(started)
			response := &gosnmp.SnmpPacket{Version: request.Version, Community: request.Community, RequestID: request.RequestID, PDUType: gosnmp.GetResponse}
			if request.PDUType == gosnmp.GetRequest {
				for _, v := range request.Variables {
					found := false
					for _, candidate := range values {
						if candidate.Name == v.Name {
							response.Variables = append(response.Variables, candidate)
							found = true
							break
						}
					}
					if !found {
						response.Variables = append(response.Variables, gosnmp.SnmpPDU{Name: v.Name, Type: gosnmp.NoSuchObject})
					}
				}
			} else if request.PDUType == gosnmp.GetBulkRequest || request.PDUType == gosnmp.GetNextRequest {
				max := int(request.MaxRepetitions)
				if max < 1 {
					max = 1
				}
				if max > 100 {
					max = 100
				}
				cursor := request.Variables[0].Name
				for _, v := range values {
					if snmpE2EOIDCompare(v.Name, cursor) > 0 {
						response.Variables = append(response.Variables, v)
						if len(response.Variables) == max {
							break
						}
					}
				}
				if len(response.Variables) == 0 {
					response.Variables = []gosnmp.SnmpPDU{{Name: cursor, Type: gosnmp.EndOfMibView}}
				}
			} else {
				continue
			}
			wire, e := response.MarshalMsg()
			if e == nil {
				conn.WriteTo(wire, peer)
			}
		}
	}()
	t.Cleanup(func() { conn.Close(); <-serverDone })
	observed := make(chan model.Envelope, 512)
	controller := startTCPController(t, "127.0.0.1:0", mockcontroller.New(nil, nil, observeChannel(observed)))
	cfg := testConfig(t, controller.listener.Addr().String())
	cfg.Local.Enabled = false
	cfg.Queue.MaxMessages = 256
	cfg.Remote.Concurrency = 1
	cfg.Credentials = map[string]credentials.Reference{"snmp": {CommunityEnv: "INFRAPULSE_SNMP_E2E_COMMUNITY"}}
	_, portText, _ := net.SplitHostPort(conn.LocalAddr().String())
	port, _ := strconv.ParseUint(portText, 10, 16)
	cfg.Devices = []config.Device{{ID: "fixture-switch", Name: "Fixture Switch", Driver: "snmp", Address: "127.0.0.1", CredentialRef: "snmp", SNMP: config.SNMP{Port: uint16(port)}, Polling: config.Polling{FastInterval: 30 * time.Millisecond, MediumInterval: 80 * time.Millisecond, InventoryInterval: 100 * time.Millisecond, TopologyInterval: 150 * time.Millisecond, Timeout: 80 * time.Millisecond}}}
	a := newTestAgent(t, cfg, nil)
	stop := runTestAgent(t, a)
	var inventory, topology, rates bool
	waitEnvelope(t, observed, func(e model.Envelope) bool {
		wire, _ := json.Marshal(e)
		if strings.Contains(string(wire), secret) {
			t.Fatal("SNMP community entered wire")
		}
		if e.Inventory != nil {
			for _, d := range e.Inventory.Devices {
				if d.ID == "fixture-switch" && d.Model == "fixture-chassis" && d.SerialNumber == "fixture-serial" && d.Hostname == "fixture-snmp-host" {
					inventory = true
				}
			}
		}
		if update := e.Topology; update != nil && update.DeviceID == "fixture-switch" && update.Completeness == "complete" {
			if len(update.Observations) != 1 {
				t.Fatalf("wrong LLDP fixture: %+v", update)
			}
			n := update.Observations[0]
			if n.LocalPortNumber != 7 || n.LocalInterfaceID != "ifindex:42" || n.RemoteSystemName != "unregistered-peer" || n.Protocol != "lldp" || n.Confidence != "high" {
				t.Fatalf("wrong local port evidence: %+v", n)
			}
			topology = true
		}
		if sample := e.Telemetry; sample != nil && sample.DeviceID == "fixture-switch" && len(sample.Interfaces) == 1 {
			iface := sample.Interfaces[0]
			if sample.Source != "snmp" || iface.CounterMetadata == nil || iface.CounterMetadata.Widths.RXBytes != 64 || iface.Counters.RXBytes < 1<<42 {
				t.Fatalf("wrong SNMP normalization: %+v", sample)
			}
			if iface.Rates != nil && iface.Rates.Available != nil && iface.Rates.Available.RXBytes && iface.Rates.RXBytesPerSecond > 0 {
				rates = true
			}
		}
		return inventory && topology && rates
	})
	unavailable.Store(true)
	waitEnvelope(t, observed, func(e model.Envelope) bool {
		for _, event := range e.Events {
			if event.Type == "device.offline" && event.DeviceID == "fixture-switch" {
				return true
			}
		}
		return false
	})
	unavailable.Store(false)
	waitEnvelope(t, observed, func(e model.Envelope) bool {
		for _, event := range e.Events {
			if event.Type == "device.online" && event.DeviceID == "fixture-switch" {
				return true
			}
		}
		return false
	})
	waitEnvelope(t, observed, func(e model.Envelope) bool { return e.Telemetry != nil && e.Telemetry.DeviceID == "fixture-switch" })
	stop()
	snapshot, _ := json.Marshal(a.Snapshot())
	if strings.Contains(string(snapshot), secret) {
		t.Fatal("SNMP community entered diagnostics")
	}
	if len(a.Snapshot().RemoteDevices) != 1 || !a.Snapshot().RemoteDevices[0].Device.Online {
		t.Fatalf("remote recovery state missing: %+v", a.Snapshot().RemoteDevices)
	}
	// A separate probe exercises the public CLI wiring without Controller state.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	dev, err := ProbeDevice(ctx, cfg, "fixture-switch")
	if err != nil || dev.ID != "fixture-switch" || dev.Hostname != "fixture-snmp-host" {
		t.Fatalf("bounded probe: %+v %v", dev, err)
	}
	if dev.Observation == nil || dev.Observation.Source.TargetID != "remote:fixture-switch" || dev.Observation.Source.CollectorID != "" || dev.Observation.Source.Type != model.SourceSNMP || dev.Observation.Time.ClockOffset != nil {
		t.Fatalf("standalone probe source binding: %+v", dev.Observation)
	}
}

func snmpE2EValues(started time.Time) []gosnmp.SnmpPDU {
	var values []gosnmp.SnmpPDU
	add := func(oid string, kind gosnmp.Asn1BER, value any) {
		values = append(values, gosnmp.SnmpPDU{Name: oid, Type: kind, Value: value})
	}
	const system = ".1.3.6.1.2.1.1"
	add(system+".1.0", gosnmp.OctetString, []byte("Generic fixture SNMP device"))
	add(system+".2.0", gosnmp.ObjectIdentifier, ".1.3.6.1.4.1.99999.1")
	add(system+".3.0", gosnmp.TimeTicks, uint32(100000+time.Since(started)/(10*time.Millisecond)))
	add(system+".5.0", gosnmp.OctetString, []byte("fixture-snmp-host"))
	const base = ".1.3.6.1.2.1.2.2.1"
	for column, n := range map[int]int{1: 42, 3: 6, 4: 1500, 7: 1, 8: 1} {
		add(base+"."+strconv.Itoa(column)+".42", gosnmp.Integer, n)
	}
	add(base+".2.42", gosnmp.OctetString, []byte("port 42"))
	add(base+".5.42", gosnmp.Gauge32, uint32(1_000_000_000))
	add(base+".6.42", gosnmp.OctetString, []byte{2, 0, 0, 0, 0, 42})
	for _, column := range []int{10, 11, 12, 13, 14, 16, 17, 18, 19, 20} {
		add(base+"."+strconv.Itoa(column)+".42", gosnmp.Counter32, uint32(0))
	}
	const extended = ".1.3.6.1.2.1.31.1.1.1"
	add(extended+".1.42", gosnmp.OctetString, []byte("ether42"))
	add(extended+".15.42", gosnmp.Gauge32, uint32(1000))
	add(extended+".19.42", gosnmp.TimeTicks, uint32(0))
	counter := uint64(1<<42) + uint64(time.Since(started).Milliseconds())*1000
	for _, column := range []int{6, 7, 8, 9, 10, 11, 12, 13} {
		add(extended+"."+strconv.Itoa(column)+".42", gosnmp.Counter64, counter)
	}
	const entity = ".1.3.6.1.2.1.47.1.1.1.1"
	add(entity+".5.1", gosnmp.Integer, 3)
	add(entity+".13.1", gosnmp.OctetString, []byte("fixture-chassis"))
	add(entity+".11.1", gosnmp.OctetString, []byte("fixture-serial"))
	const local = ".1.0.8802.1.1.2.1.3.7.1"
	add(local+".2.7", gosnmp.Integer, 5)
	add(local+".3.7", gosnmp.OctetString, []byte("ether42"))
	add(local+".4.7", gosnmp.OctetString, []byte("port 42"))
	add(".1.0.8802.1.1.2.1.2.1.0", gosnmp.TimeTicks, uint32(10))
	const remote = ".1.0.8802.1.1.2.1.4.1.1"
	add(remote+".4.100.7.1", gosnmp.Integer, 4)
	add(remote+".5.100.7.1", gosnmp.OctetString, []byte{2, 0, 0, 0, 0, 99})
	add(remote+".6.100.7.1", gosnmp.Integer, 5)
	add(remote+".7.100.7.1", gosnmp.OctetString, []byte("uplink"))
	add(remote+".9.100.7.1", gosnmp.OctetString, []byte("unregistered-peer"))
	sort.Slice(values, func(i, j int) bool { return snmpE2EOIDCompare(values[i].Name, values[j].Name) < 0 })
	return values
}
func snmpE2EOIDCompare(a, b string) int {
	x, y := strings.Split(strings.Trim(a, "."), "."), strings.Split(strings.Trim(b, "."), ".")
	for i := 0; i < len(x) && i < len(y); i++ {
		m, _ := strconv.ParseUint(x[i], 10, 32)
		n, _ := strconv.ParseUint(y[i], 10, 32)
		if m < n {
			return -1
		}
		if m > n {
			return 1
		}
	}
	if len(x) < len(y) {
		return -1
	}
	if len(x) > len(y) {
		return 1
	}
	return 0
}
