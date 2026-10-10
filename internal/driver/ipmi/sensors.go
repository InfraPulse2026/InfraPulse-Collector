package ipmi

import (
	"fmt"
	"math"
	"time"

	"InfraPulseAgent/internal/driver"
	"InfraPulseAgent/internal/model"
	"github.com/bougou/go-ipmi/pkg/types"
)

func (p *poll) sensorComponent(s sensor, parent string) model.InventoryComponent {
	uri := s.resource
	at := time.Now().UTC()
	kind := "Sensor"
	switch s.typ {
	case 4:
		kind = "Fan"
	case 7:
		kind = "ProcessorSensor"
	case 8:
		kind = "PowerSupply"
	case 0xc:
		kind = "MemorySensor"
	case 0xd:
		kind = "StorageSensor"
	}
	return model.InventoryComponent{ID: stableID(p.a.opts.ID, "Sensor", uri), ParentID: parent, Class: kind, Name: s.name,
		Description: fmt.Sprintf("SDR=%d sensor_type=0x%02x event_reading_type=0x%02x entity=%d instance=%d", s.record, s.typ, s.readingType, s.entity, s.instance),
		Hardware:    &model.HardwareInventory{ResourceType: kind, Protocol: "ipmi", Status: unknownStatus("unknown")}, Observation: p.observation(uri, at)}
}

func (p *poll) sensorReading(s sensor) (model.ComponentHealth, *model.Metric, error) {
	at := time.Now().UTC()
	health := model.ComponentHealth{ID: stableID(p.a.opts.ID, "Sensor", s.resource), ResourceID: s.resource, Name: s.name, Status: unknownStatus("unavailable"), Observation: p.observation(s.resource, at)}
	if s.owner != 0x20 || s.channel != 0 || s.lun > 3 {
		health.Status.State = "unsupported_owner"
		return health, nil, ErrUnsupportedAddress
	}
	if err := p.operation(); err != nil {
		return health, nil, err
	}
	reading, err := p.client.SensorReading(p.ctx, s.number, s.owner, s.lun, s.channel)
	if err != nil {
		return health, nil, err
	}
	at = time.Now().UTC()
	health.Observation = p.observation(s.resource, at)
	if reading.Unavailable {
		health.Status.State = "reading_unavailable"
		return health, nil, nil
	}
	if reading.ScanningDisabled {
		health.Status.State = "scanning_disabled"
		return health, nil, nil
	}
	if reading.StateBytes < 0 || reading.StateBytes > 2 {
		health.Status.State = "reading_invalid"
		return health, nil, driver.ErrMalformed
	}
	health.Status = sensorStatus(s, reading)
	if s.full == nil || !s.full.HasAnalogReading() {
		return health, nil, nil
	}
	name, unit, value, ok := sensorMetric(s, reading.Raw)
	if !ok {
		health.Status.State = "conversion_unavailable"
		return health, nil, nil
	}
	status := health.Status
	metric := &model.Metric{Name: name, Unit: unit, Value: value, ResourceID: s.resource, ComponentID: health.ID, Status: &status, Observation: model.CloneObservation(health.Observation)}
	return health, metric, nil
}

func sensorMetric(s sensor, raw uint8) (string, string, float64, bool) {
	if s.full == nil || !s.full.HasAnalogReading() || s.readingType != 1 || s.full.LinearizationFunc > types.LinearizationFunc_CBRT {
		return "", "", 0, false
	}
	units := s.full.SensorUnit
	if units.AnalogDataFormat == types.SensorAnalogUnitFormat_NotAnalog || units.RateUnit != types.SensorRateUnit_None || units.ModifierRelation != types.SensorModifierRelation_None || units.ModifierUnit != types.SensorUnitType_Unspecified {
		return "", "", 0, false
	}
	value := s.full.ConvertReading(raw)
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return "", "", 0, false
	}
	name, unit := "", ""
	if units.Percentage {
		if units.BaseUnit != types.SensorUnitType_Unspecified {
			return "", "", 0, false
		}
		name, unit = "hardware.sensor.percent", "%"
		if s.typ == 4 {
			name = "hardware.fan.percent"
		}
		if value < 0 || value > 100 {
			return "", "", 0, false
		}
	} else {
		switch units.BaseUnit {
		case types.SensorUnitType_DegreesC:
			if s.typ != 1 {
				return "", "", 0, false
			}
			name, unit = "hardware.temperature.celsius", "Cel"
		case types.SensorUnitType_DegreesF:
			if s.typ != 1 {
				return "", "", 0, false
			}
			name, unit = "hardware.temperature.celsius", "Cel"
			value = (value - 32) * 5 / 9
		case types.SensorUnitType_DegreesK:
			if s.typ != 1 {
				return "", "", 0, false
			}
			name, unit = "hardware.temperature.celsius", "Cel"
			value -= 273.15
		case types.SensorUnitType_RPM:
			if s.typ != 4 || value < 0 {
				return "", "", 0, false
			}
			name, unit = "hardware.fan.rpm", "RPM"
		case types.SensorUnitType_Volts:
			if s.typ != 2 {
				return "", "", 0, false
			}
			name, unit = "hardware.voltage.volts", "V"
		case types.SensorUnitType_Amps:
			if s.typ != 3 {
				return "", "", 0, false
			}
			name, unit = "hardware.current.amperes", "A"
		case types.SensorUnitType_Watts:
			if s.typ != 0xb && s.typ != 8 && s.typ != 9 {
				return "", "", 0, false
			}
			name, unit = "hardware.power.watts", "W"
		default:
			return "", "", 0, false
		}
	}
	if unit == "Cel" && value < -273.15 || math.IsNaN(value) || math.IsInf(value, 0) {
		return "", "", 0, false
	}
	return name, unit, value, true
}

// Mapping is deliberately conservative. Availability and presence never imply
// failure; unknown generic asserted/OEM bits are retained as unknown health.
func sensorStatus(s sensor, r Reading) model.HardwareStatus {
	status := unknownStatus("status_unavailable")
	if r.StateBytes == 0 {
		return status
	}
	bits := r.States
	if r.StateBytes == 1 {
		bits &= 0xff
	} else {
		bits &= 0x7fff
	}
	status.State = fmt.Sprintf("event=0x%02x;states=0x%04x", s.readingType, bits)
	if s.readingType == 1 {
		bits &= 0x3f
		if bits & ^s.readingMask != 0 {
			return status
		}
		if bits&0x36 != 0 {
			status.Health = "critical"
		} else if bits&0x09 != 0 {
			status.Health = "warning"
		} else if s.readingMask&0x3f != 0 {
			status.Health = "ok"
		}
		return status
	}
	// A mask tells which present-state bits are actually implemented. An
	// asserted unadvertised bit cannot become an invented healthy observation.
	if bits & ^s.readingMask != 0 {
		return status
	}
	switch s.readingType {
	case 4, 5, 6: // predictive failure / limit exceeded / performance lags
		if bits&^uint16(3) != 0 || bits == 3 {
			return status
		}
		if bits&2 != 0 {
			status.Health = "warning"
		} else if bits&1 != 0 {
			status.Health = "ok"
		}
	case 7: // severity present-state offsets (§41.2 and Table 42-2)
		if bits & ^uint16(0x1ff) != 0 {
			return status
		}
		if bits&0x6c != 0 {
			status.Health = "critical"
		} else if bits&0x12 != 0 {
			status.Health = "warning"
		} else if bits&1 != 0 {
			status.Health = "ok"
		}
	case 8: // presence says nothing about health
		if bits == 1 {
			status.State = "Absent"
		} else if bits == 2 {
			status.State = "Present"
		}
	case 9:
		if bits == 1 {
			status.State = "Disabled"
		} else if bits == 2 {
			status.State = "Enabled"
		}
	case 0xb:
		if bits & ^uint16(0xff) != 0 {
			return status
		}
		if bits&0x20 != 0 {
			status.Health = "critical"
		} else if bits&0xde != 0 {
			status.Health = "warning"
		} else if bits == 1 {
			status.Health = "ok"
		}
	case 0x6f:
		critical, warning, presence := uint16(0), uint16(0), uint16(0)
		switch s.typ {
		case 7:
			critical, warning, presence = 0x087f, 0x1400, 0x0380
		case 8:
			critical, warning, presence = 0x001a, 0x0064, 0x0081
		case 9:
			critical, warning, presence = 0x0074, 0x0080, 0x000b
		case 0xc:
			critical, warning, presence = 0x048e, 0x0221, 0x0150
		case 0xd:
			critical, warning, presence = 0x0042, 0x0124, 0x0099
		default:
			return status
		}
		if bits&critical != 0 {
			status.Health = "critical"
		} else if bits&warning != 0 {
			status.Health = "warning"
		} else if bits & ^presence != 0 {
			return status
		}
		// Presence and standby are explicit states but do not prove OK.
		if s.typ == 8 && bits == 1 {
			status.State = "Present"
		}
		if s.typ == 8 && bits == 0x80 {
			status.State = "StandbyOffline"
		}
	}
	return status
}
