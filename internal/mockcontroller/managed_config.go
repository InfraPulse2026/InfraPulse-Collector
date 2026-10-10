package mockcontroller

import (
	"errors"
	"fmt"
	"strings"

	pb "InfraPulseAgent/api/gen/infrapulse/v1"
	"InfraPulseAgent/internal/model"
	transportgrpc "InfraPulseAgent/internal/transport/grpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type configSession struct {
	collectorID, sessionID string
	capable                bool
	pending                chan model.DesiredCollectorConfig
}

// SetDesiredConfigs installs a development scenario. Existing sessions retain
// their independent bounded delivery inbox; all payloads own their values.
func (c *Controller) SetDesiredConfigs(configs []model.DesiredCollectorConfig) error {
	if len(configs) > 128 {
		return errors.New("mock configuration scenario exceeds 128 snapshots")
	}
	owned := make([]model.DesiredCollectorConfig, len(configs))
	for i, s := range configs {
		if _, err := transportgrpc.EncodeDesiredConfig(s); err != nil {
			return err
		}
		owned[i] = model.CloneDesiredCollectorConfig(s)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.DesiredConfigs = owned
	c.configProgress = make(map[string]int)
	c.configRepeats = make(map[string]bool)
	return nil
}

// SendConfig queues a typed snapshot for one connected Collector. The existing
// stream handler sends it after the next envelope (normally a heartbeat), so
// there is still exactly one stream sender and receiver, and no extra worker.
// An explicit wrong collector/session claim is preserved for negative tests.
func (c *Controller) SendConfig(collectorID string, config model.DesiredCollectorConfig) error {
	if _, err := transportgrpc.EncodeDesiredConfig(config); err != nil {
		return err
	}
	config = model.CloneDesiredCollectorConfig(config)
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.configSessions[collectorID]
	if s == nil {
		return errors.New("mock collector is not connected")
	}
	if !s.capable {
		return errors.New("collector did not negotiate managed_config_v1")
	}
	select {
	case s.pending <- config:
		return nil
	default:
		return errors.New("mock configuration inbox is full")
	}
}

func (c *Controller) openConfigSession(h *model.AgentHello) *configSession {
	s := &configSession{collectorID: h.AgentID, sessionID: h.SessionID, capable: h.ManagedConfigV1, pending: make(chan model.DesiredCollectorConfig, 8)}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.configSessions == nil {
		c.configSessions = make(map[string]*configSession)
		c.configProgress = make(map[string]int)
		c.configRepeats = make(map[string]bool)
		c.configDisconnects = make(map[string]bool)
		c.configStatusCounts = make(map[string]int)
	}
	// Stream count already bounds these maps. Persisted mock scenario cursors
	// are limited independently: development fixtures cannot grow them forever.
	if len(c.configProgress) >= 128 {
		for id := range c.configProgress {
			if c.configSessions[id] == nil {
				delete(c.configProgress, id)
				delete(c.configDisconnects, id)
				delete(c.configStatusCounts, id)
				for key := range c.configRepeats {
					if strings.HasPrefix(key, id+":") {
						delete(c.configRepeats, key)
					}
				}
			}
		}
	}
	c.configSessions[h.AgentID] = s
	return s
}
func (c *Controller) closeConfigSession(s *configSession) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.configSessions[s.collectorID] == s {
		delete(c.configSessions, s.collectorID)
	}
}

func (c *Controller) sendInitialConfig(stream grpc.BidiStreamingServer[pb.AgentEnvelope, pb.ControllerEnvelope], s *configSession) error {
	if !s.capable {
		return nil
	}
	c.mu.Lock()
	index := c.configProgress[s.collectorID]
	var config *model.DesiredCollectorConfig
	if len(c.DesiredConfigs) > 0 {
		if index >= len(c.DesiredConfigs) {
			index = len(c.DesiredConfigs) - 1
		}
		v := model.CloneDesiredCollectorConfig(c.DesiredConfigs[index])
		config = &v
	}
	c.mu.Unlock()
	if config != nil {
		return sendManagedConfig(stream, s, *config)
	}
	return nil
}

func (c *Controller) handleConfigStatus(stream grpc.BidiStreamingServer[pb.AgentEnvelope, pb.ControllerEnvelope], s *configSession, result *model.ConfigApplyResult) error {
	if result == nil || !s.capable || result.CollectorID != s.collectorID {
		return nil
	}
	if result.Status != model.ConfigStatusApplied && result.Status != model.ConfigStatusRejected && result.Status != model.ConfigStatusFailed {
		return nil
	}
	c.mu.Lock()
	c.configStatusCounts[s.collectorID]++
	disconnect := c.DisconnectAfterConfigStatus > 0 && c.configStatusCounts[s.collectorID] >= c.DisconnectAfterConfigStatus && !c.configDisconnects[s.collectorID]
	if disconnect {
		c.configDisconnects[s.collectorID] = true
	}
	index := c.configProgress[s.collectorID]
	var next *model.DesiredCollectorConfig
	if index < len(c.DesiredConfigs) {
		current := c.DesiredConfigs[index]
		if current.Revision == result.Revision && current.ConfigurationDigest == result.ConfigurationDigest {
			key := fmt.Sprintf("%s:%d", s.collectorID, index)
			if c.RepeatConfig && !c.configRepeats[key] {
				c.configRepeats[key] = true
				v := model.CloneDesiredCollectorConfig(current)
				next = &v
			} else {
				delete(c.configRepeats, key)
				index++
				c.configProgress[s.collectorID] = index
				if index < len(c.DesiredConfigs) {
					v := model.CloneDesiredCollectorConfig(c.DesiredConfigs[index])
					next = &v
				}
			}
		}
	}
	c.mu.Unlock()
	if disconnect {
		return status.Error(codes.Unavailable, "development mock configuration disconnect")
	}
	if next != nil {
		return sendManagedConfig(stream, s, *next)
	}
	return nil
}

func (c *Controller) sendPendingConfigs(stream grpc.BidiStreamingServer[pb.AgentEnvelope, pb.ControllerEnvelope], s *configSession) error {
	for range 8 {
		select {
		case config := <-s.pending:
			if err := sendManagedConfig(stream, s, config); err != nil {
				return err
			}
		default:
			return nil
		}
	}
	return nil
}
func sendManagedConfig(stream grpc.BidiStreamingServer[pb.AgentEnvelope, pb.ControllerEnvelope], s *configSession, config model.DesiredCollectorConfig) error {
	if config.CollectorID == "" {
		config.CollectorID = s.collectorID
	}
	if config.SessionID == "" {
		config.SessionID = s.sessionID
	}
	w, err := transportgrpc.EncodeDesiredConfig(config)
	if err != nil {
		return status.Error(codes.InvalidArgument, "invalid mock desired configuration")
	}
	return stream.Send(&pb.ControllerEnvelope{Payload: &pb.ControllerEnvelope_DesiredConfig{DesiredConfig: w}})
}
