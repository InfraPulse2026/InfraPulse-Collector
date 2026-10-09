package config

import (
	"InfraPulseAgent/internal/model"
	"time"
)

// Target adapts the existing devices YAML schema without changing configured
// IDs or introducing a second configuration model. Existing entries are enabled.
// A Target ID identifies the connection; it is not an observed Device ID.
func (d Device) Target(defaults Polling) model.Target {
	poll := d.Polling
	if d.Driver == "redfish" {
		// Hardware BMCs use their own slow polling budget. In particular, the
		// existing 3s/2s telemetry defaults are unsuitable for full discovery.
		poll.FastInterval, poll.TopologyInterval = 0, 0
		if poll.MediumInterval == 0 {
			poll.MediumInterval = 30 * time.Second
		}
		if poll.InventoryInterval == 0 {
			poll.InventoryInterval = 5 * time.Minute
		}
		if poll.Timeout == 0 {
			poll.Timeout = 30 * time.Second
		}
		defaults = Polling{}
	}
	if poll.FastInterval == 0 {
		poll.FastInterval = defaults.FastInterval
	}
	if poll.MediumInterval == 0 {
		poll.MediumInterval = defaults.MediumInterval
	}
	if poll.InventoryInterval == 0 {
		poll.InventoryInterval = defaults.InventoryInterval
	}
	if poll.TopologyInterval == 0 {
		poll.TopologyInterval = defaults.TopologyInterval
	}
	if poll.Timeout == 0 {
		poll.Timeout = defaults.Timeout
	}
	return model.Target{
		ID: "remote:" + d.ID, Name: d.Name, Type: model.SourceType(d.Driver), Protocol: d.Driver,
		Endpoint: d.Address, CredentialRef: d.CredentialRef, Enabled: true, Location: d.Location,
		Polling: model.TargetPolling{FastInterval: poll.FastInterval, MediumInterval: poll.MediumInterval,
			InventoryInterval: poll.InventoryInterval, TopologyInterval: poll.TopologyInterval, Timeout: poll.Timeout},
	}
}
