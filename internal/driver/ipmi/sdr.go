package ipmi

import (
	"encoding/binary"
	"fmt"
	"sort"
	"time"

	"InfraPulseAgent/internal/driver"
	"github.com/bougou/go-ipmi/pkg/types"
)

type sensor struct {
	record                                                          uint16
	owner, channel, lun, number, entity, instance, typ, readingType uint8
	name, resource                                                  string
	readingMask                                                     uint16
	full                                                            *types.SDRFull
}
type sdrCache struct {
	sensors    []sensor
	frus       []uint8
	repository RepositoryInfo
	refreshed  time.Time
}

func addFRU(ids []uint8, id uint8, limit int) []uint8 {
	for _, existing := range ids {
		if existing == id {
			return ids
		}
	}
	if len(ids) < limit {
		ids = append(ids, id)
	}
	return ids
}

// Enumeration uses fixed bounded chunks. The 5-byte record header is validated
// before allocation, and next IDs are checked before the next protocol request.
func (p *poll) discoverSDR(force bool) (sdrCache, error) {
	if err := p.operation(); err != nil {
		return sdrCache{}, err
	}
	repo, err := p.client.RepositoryInfo(p.ctx)
	if err != nil {
		p.a.cache = sdrCache{}
		return sdrCache{}, err
	}
	if repo.Version != 0x51 {
		p.a.cache = sdrCache{}
		return sdrCache{}, driver.ErrMalformed
	}
	if int(repo.Records) > p.a.limits.MaxSDRRecords {
		p.a.cache = sdrCache{}
		return sdrCache{}, driver.ErrLimit
	}
	now := time.Now() // preserve the monotonic clock for cache expiry
	if !force && !p.a.cache.refreshed.IsZero() && now.Sub(p.a.cache.refreshed) < p.a.limits.SDRCacheTTL && p.a.cache.repository == repo {
		return p.a.cache, nil
	}
	// Expired/changed metadata is never reused indefinitely after a failed read.
	p.a.cache = sdrCache{}
	cache := sdrCache{repository: repo}
	if repo.Records == 0 {
		cache.refreshed = now
		p.a.cache = cache
		return cache, nil
	}
	reservation := uint16(0)
	if repo.ReserveSupported {
		if err := p.operation(); err != nil {
			return cache, err
		}
		reservation, err = p.client.ReserveSDR(p.ctx)
		if err != nil {
			return cache, err
		}
		if reservation == 0 {
			return cache, driver.ErrMalformed
		}
	}
	id := uint16(0)
	seen := make(map[uint16]bool, p.a.limits.MaxSDRRecords)
	sensorKeys := make(map[string]bool, p.a.limits.MaxSDRRecords)
	total := 0
	var failure error
	complete := true
	visited := 0
	for iteration := 0; iteration < p.a.limits.MaxSDRRecords; iteration++ {
		if seen[id] {
			failure = driver.ErrMalformed
			break
		}
		seen[id] = true
		next, record, e := p.sdrRecord(reservation, id)
		if e != nil {
			failure = e
			break
		}
		if len(record) > p.a.limits.MaxSDRBytes-total {
			failure = driver.ErrLimit
			break
		}
		total += len(record)
		actual := binary.LittleEndian.Uint16(record[:2])
		if actual == 0xffff || (id != 0 && actual != id) {
			failure = driver.ErrMalformed
			break
		}
		parsed, e := parseSDR(record, next)
		visited++
		if e != nil {
			complete = false
			p.record(e)
		} else {
			if parsed.FRUDeviceLocator != nil {
				fru := parsed.FRUDeviceLocator
				if fru.IsLogicalFRUDevice && fru.DeviceAccessAddress == 0x20 && fru.AccessLUN == 0 && fru.ChannelNumber == 0 && fru.FRUDeviceID_SlaveAddress != 0xff {
					before := len(cache.frus)
					cache.frus = addFRU(cache.frus, fru.FRUDeviceID_SlaveAddress, p.a.limits.MaxFRURecords)
					if before == len(cache.frus) {
						found := false
						for _, x := range cache.frus {
							found = found || x == fru.FRUDeviceID_SlaveAddress
						}
						if !found {
							p.record(driver.ErrLimit)
						}
					}
				} else {
					complete = false
					p.record(ErrUnsupportedAddress)
				}
			}
			for _, s := range sensors(parsed, record) {
				if len(cache.sensors) >= p.a.limits.MaxSDRRecords {
					complete = false
					p.record(driver.ErrLimit)
					break
				}
				if sensorKeys[s.resource] {
					complete = false
					p.record(driver.ErrMalformed)
					continue
				}
				sensorKeys[s.resource] = true
				cache.sensors = append(cache.sensors, s)
			}
		}
		if next == 0xffff {
			break
		}
		if next == id || seen[next] || iteration+1 == p.a.limits.MaxSDRRecords {
			failure = driver.ErrMalformed
			break
		}
		id = next
	}
	if failure != nil {
		p.record(failure)
	}
	if failure == nil && visited != int(repo.Records) {
		failure = driver.ErrMalformed
		p.record(failure)
	}
	// Repository timestamps/count must remain stable across the snapshot.
	if failure == nil {
		if e := p.operation(); e != nil {
			failure = e
		} else {
			last, e := p.client.RepositoryInfo(p.ctx)
			if e != nil {
				failure = e
			} else if last != repo {
				failure = driver.ErrMalformed
				p.record(failure)
			}
		}
	}
	sort.Slice(cache.sensors, func(i, j int) bool { return cache.sensors[i].resource < cache.sensors[j].resource })
	sort.Slice(cache.frus, func(i, j int) bool { return cache.frus[i] < cache.frus[j] })
	if failure == nil && complete {
		cache.refreshed = now
		p.a.cache = cache
	}
	return cache, failure
}

func (p *poll) sdrRecord(reservation, id uint16) (uint16, []byte, error) {
	if err := p.operation(); err != nil {
		return 0, nil, err
	}
	next, header, err := p.client.SDR(p.ctx, reservation, id, 0, 5)
	if err != nil {
		return 0, nil, err
	}
	if err = p.received(header); err != nil {
		return 0, nil, err
	}
	if len(header) != 5 || header[2] != 0x51 {
		return 0, nil, driver.ErrMalformed
	}
	length := 5 + int(header[4])
	if length > 260 {
		return 0, nil, driver.ErrLimit
	}
	data := make([]byte, 0, length)
	data = append(data, header...)
	for len(data) < length {
		if err := p.operation(); err != nil {
			return 0, nil, err
		}
		// Get SDR offsets are 8-bit. A record larger than the 255-byte offset
		// range is read with the final chunk starting before that boundary.
		count := min(32, length-len(data))
		offset := len(data)
		if offset > 255 {
			return 0, nil, driver.ErrMalformed
		}
		n, dataPart, e := p.client.SDR(p.ctx, reservation, id, uint8(offset), uint8(count))
		if e != nil {
			return 0, nil, e
		}
		if e = p.received(dataPart); e != nil {
			return 0, nil, e
		}
		if n != next || len(dataPart) != count {
			return 0, nil, driver.ErrMalformed
		}
		data = append(data, dataPart...)
	}
	return next, data, nil
}

func parseSDR(data []byte, next uint16) (*types.SDR, error) {
	if len(data) < 5 || data[2] != 0x51 || len(data) != 5+int(data[4]) {
		return nil, driver.ErrMalformed
	}
	// The library decoder permits unused trailing bytes; enforce the source
	// type/length bound before calling it. Unsupported record types are skipped.
	nameIndex := 0
	switch data[3] {
	case 1:
		nameIndex = 47
	case 2:
		nameIndex = 31
	case 0x11:
		nameIndex = 15
	default:
		return &types.SDR{}, nil
	}
	if len(data) <= nameIndex {
		return nil, driver.ErrMalformed
	}
	typeLength := data[nameIndex]
	nameBytes := int(typeLength & 0x1f)
	if typeLength&0x20 != 0 || nameBytes > 16 || nameIndex+1+nameBytes > len(data) {
		return nil, driver.ErrMalformed
	}
	parsed, err := types.ParseSDR(data, next)
	if err != nil {
		return nil, driver.ErrMalformed
	}
	if parsed.Compact != nil {
		v := parsed.Compact
		count := max(1, int(v.ShareCount))
		if int(v.SensorNumber)+count > 0xff || v.EntityInstanceSharing && int(v.SensorEntityInstance)+count > 0x80 {
			return nil, driver.ErrMalformed
		}
	}
	return parsed, nil
}

func sensors(parsed *types.SDR, raw []byte) []sensor {
	var s sensor
	var generator types.GeneratorID
	var typeLength types.TypeLength
	var text []byte
	count := 1
	instanceSharing := false
	modifier, modifierType := uint8(0), uint8(0)
	if parsed.Full != nil {
		v := parsed.Full
		s = sensor{record: parsed.RecordHeader.RecordID, entity: uint8(v.SensorEntityID), instance: raw[9], typ: uint8(v.SensorType), readingType: uint8(v.SensorEventReadingType), full: v}
		generator = v.GeneratorID
		s.number = uint8(v.SensorNumber)
		typeLength = v.IDStringTypeLength
		text = v.IDStringBytes
	} else if parsed.Compact != nil {
		v := parsed.Compact
		s = sensor{record: parsed.RecordHeader.RecordID, entity: uint8(v.SensorEntityID), instance: raw[9], typ: uint8(v.SensorType), readingType: uint8(v.SensorEventReadingType)}
		generator = v.GeneratorID
		s.number = uint8(v.SensorNumber)
		typeLength = v.IDStringTypeLength
		text = v.IDStringBytes
		count = max(1, int(v.ShareCount))
		instanceSharing = v.EntityInstanceSharing
		modifier = v.IDStringInstanceModifierOffset
		modifierType = v.IDStringInstanceModifierType
	} else {
		return nil
	}
	s.owner = generator.OwnerID()
	s.channel = generator.ChannelNumber()
	s.lun = generator.LUN()
	s.readingMask = binary.LittleEndian.Uint16(raw[18:20])
	if s.readingType == 1 {
		// Table 43-1: comparison-status support lives in bits 12..14 of
		// the lower/upper event masks, not in the threshold-value mask.
		// The pinned library's Mask helpers reverse these byte positions.
		s.readingMask = uint16((raw[15]>>4)&7) | uint16((raw[17]>>4)&7)<<3
	}
	name, err := decodeText(uint8(typeLength), text, 0, false)
	if err != nil {
		name = ""
	}
	s.name = name
	var out []sensor
	for i := 0; i < count; i++ {
		if int(s.number)+i >= 0xff || (instanceSharing && int(s.instance&0x7f)+i > 0x7f) {
			break
		}
		copy := s
		copy.number += uint8(i)
		if instanceSharing {
			copy.instance = (s.instance & 0x80) | ((s.instance & 0x7f) + uint8(i))
		}
		if count > 1 {
			if modifierType == 0 {
				copy.name += fmt.Sprintf("%d", int(modifier)+i)
			} else if modifierType == 1 && int(modifier)+i < 26 {
				copy.name += string(rune('A' + int(modifier) + i))
			} else {
				copy.name = ""
			}
		}
		copy.resource = fmt.Sprintf("/ipmi/sensors/%02x/%d/%d/%d/%d/%d", copy.owner, copy.channel, copy.lun, copy.number, copy.entity, copy.instance)
		out = append(out, copy)
	}
	return out
}
