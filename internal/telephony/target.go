package telephony

import (
	"net"
	"strconv"

	"InfraPulseAgent/internal/model"
)

// Target projects the existing SIP endpoint configuration into the Collector
// domain without changing its historical wire target_id or probe options.
func (e Endpoint) Target() model.Target {
	e = e.WithDefaults()
	return model.Target{ID: "sip:" + e.ID, Type: model.SourceSIP, Protocol: "sip", Endpoint: net.JoinHostPort(e.Address, strconv.Itoa(int(e.Port))), Enabled: true, Polling: model.TargetPolling{FastInterval: e.Interval, Timeout: e.Timeout}}
}
