package ipmi

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"

	"InfraPulseAgent/internal/driver"
	"InfraPulseAgent/internal/model"
	"github.com/bougou/go-ipmi/pkg/types"
)

func (p *poll) fru(id uint8, parent string) ([]model.InventoryComponent, error) {
	if err := p.operation(); err != nil {
		return nil, err
	}
	info, err := p.client.FRUInfo(p.ctx, id)
	if err != nil {
		return nil, err
	}
	if info.Bytes == 0 {
		return nil, nil
	}
	if int(info.Bytes) > p.a.limits.MaxFRUBytes {
		return nil, driver.ErrLimit
	}
	if info.Bytes < 8 || info.WordAccess && info.Bytes%2 != 0 {
		return nil, driver.ErrMalformed
	}
	data := make([]byte, 0, int(info.Bytes))
	var readError error
	for len(data) < int(info.Bytes) {
		if err := p.operation(); err != nil {
			readError = err
			break
		}
		offset := len(data)
		if info.WordAccess {
			offset /= 2
		}
		count := min(32, int(info.Bytes)-len(data))
		part, e := p.client.ReadFRU(p.ctx, id, uint16(offset), uint8(count))
		if e != nil {
			readError = e
			break
		}
		if e = p.received(part); e != nil {
			readError = e
			break
		}
		if len(part) == 0 || len(part) > count || info.WordAccess && len(part)%2 != 0 {
			readError = driver.ErrMalformed
			break
		}
		data = append(data, part...)
	}
	components, parseError := p.parseFRU(id, parent, data)
	return components, errors.Join(readError, parseError)
}

// The pinned library validates the common-header checksum, but its area Unpack
// methods do not validate area checksums or cross-area overlap. Validate these
// small framing constraints here, then reuse each typed library area decoder.
// Independent areas are decoded independently to preserve a valid sibling.
func (p *poll) parseFRU(id uint8, parent string, data []byte) ([]model.InventoryComponent, error) {
	if len(data) < 8 || data[0] != 1 || data[6] != 0 || !checksum(data[:8]) {
		return nil, driver.ErrMalformed
	}
	var components []model.InventoryComponent
	var errs []error
	for _, entry := range []struct {
		index int
		kind  string
	}{{2, "Chassis"}, {3, "Board"}, {4, "Product"}} {
		if data[entry.index] == 0 {
			continue
		}
		start := int(data[entry.index]) * 8
		if start < 8 || start+2 > len(data) {
			errs = append(errs, driver.ErrMalformed)
			continue
		}
		length := int(data[start+1]) * 8
		end := start + length
		valid := length >= 8 && end <= len(data) && data[start] == 1
		for index := 1; index <= 5; index++ {
			if index == entry.index || data[index] == 0 {
				continue
			}
			other := int(data[index]) * 8
			if other == start || other > start && other < end {
				valid = false
			}
		}
		if !valid || !checksum(data[start:end]) {
			errs = append(errs, driver.ErrMalformed)
			continue
		}
		area := data[start:end]
		fieldStart, required := 3, 2
		if entry.kind == "Board" {
			fieldStart, required = 6, 5
		}
		if entry.kind == "Product" {
			required = 7
		}
		if !validFields(area, fieldStart, required) {
			errs = append(errs, driver.ErrMalformed)
			continue
		}
		uri := fmt.Sprintf("/ipmi/fru/%d/%s", id, strings.ToLower(entry.kind))
		at := time.Now().UTC()
		component := model.InventoryComponent{ID: stableID(p.a.opts.ID, entry.kind, uri), ParentID: parent, Class: entry.kind, Hardware: &model.HardwareInventory{ResourceType: entry.kind, Protocol: "ipmi", Status: unknownStatus("unknown")}, Observation: p.observation(uri, at)}
		decode := func(tl types.TypeLength, b []byte, language uint8, english bool) string {
			value, e := decodeText(uint8(tl), b, language, english)
			if e != nil {
				errs = append(errs, e)
			}
			return value
		}
		switch entry.kind {
		case "Chassis":
			var v types.FRUChassisInfoArea
			if v.Unpack(area) != nil {
				errs = append(errs, driver.ErrMalformed)
				continue
			}
			component.Hardware.ChassisType = v.ChassisType.String()
			component.Hardware.PartNumber = decode(v.PartNumberTypeLength, v.PartNumber, 0, true)
			component.SerialNumber = decode(v.SerialNumberTypeLength, v.SerialNumber, 0, true)
			// Standard Chassis Info contains no manufacturer field.
		case "Board":
			var v types.FRUBoardInfoArea
			if v.Unpack(area) != nil {
				errs = append(errs, driver.ErrMalformed)
				continue
			}
			component.Manufacturer = decode(v.ManufacturerTypeLength, v.Manufacturer, v.LanguageCode, false)
			component.Model = decode(v.ProductNameTypeLength, v.ProductName, v.LanguageCode, false)
			component.SerialNumber = decode(v.SerialNumberTypeLength, v.SerialNumber, v.LanguageCode, true)
			component.Hardware.PartNumber = decode(v.PartNumberTypeLength, v.PartNumber, v.LanguageCode, true)
		case "Product":
			var v types.FRUProductInfoArea
			if v.Unpack(area) != nil {
				errs = append(errs, driver.ErrMalformed)
				continue
			}
			component.Manufacturer = decode(v.ManufacturerTypeLength, v.Manufacturer, v.LanguageCode, false)
			component.Model = decode(v.NameTypeLength, v.Name, v.LanguageCode, false)
			component.Name = component.Model
			component.Hardware.PartNumber = decode(v.PartModelTypeLength, v.PartModel, v.LanguageCode, false)
			component.HardwareRevision = decode(v.VersionTypeLength, v.Version, v.LanguageCode, false)
			component.SerialNumber = decode(v.SerialNumberTypeLength, v.SerialNumber, v.LanguageCode, true)
			component.Hardware.AssetTag = decode(v.AssetTagTypeLength, v.AssetTag, v.LanguageCode, false)
		}
		if component.SerialNumber != "" || component.Manufacturer != "" || component.Model != "" || component.Hardware.PartNumber != "" || component.Hardware.ChassisType != "" {
			components = append(components, component)
		}
	}
	return components, errors.Join(errs...)
}

func validFields(area []byte, start, required int) bool {
	fields := 0
	for cursor := start; cursor < len(area)-1; {
		tl := area[cursor]
		cursor++
		if tl == 0xc1 {
			if fields < required {
				return false
			}
			for _, pad := range area[cursor : len(area)-1] {
				if pad != 0 {
					return false
				}
			}
			return true
		}
		length := int(tl & 63)
		if length > len(area)-1-cursor {
			return false
		}
		cursor += length
		fields++
	}
	return false
}

// decodeText handles only specified FRU/SDR encodings. Binary/unspecified
// fields have no reliable textual interpretation and stay absent.
func decodeText(tl uint8, data []byte, language uint8, english bool) (string, error) {
	if len(data) != int(tl&63) {
		return "", driver.ErrMalformed
	}
	if len(data) == 0 {
		return "", nil
	}
	var text []rune
	switch tl >> 6 {
	case 0:
		return "", driver.ErrUnavailable
	case 1:
		alphabet := "0123456789 -."
		for _, b := range data {
			for _, n := range []uint8{b >> 4, b & 15} {
				if n > 12 {
					return "", driver.ErrMalformed
				}
				text = append(text, rune(alphabet[n]))
			}
		}
	case 2:
		var bits uint32
		var available uint
		for _, b := range data {
			bits |= uint32(b) << available
			available += 8
			for available >= 6 {
				text = append(text, rune((bits&63)+32))
				bits >>= 6
				available -= 6
			}
		}
		if bits != 0 {
			return "", driver.ErrMalformed
		}
	case 3:
		if english || language == 0 || language == 25 {
			for _, b := range data {
				text = append(text, rune(b))
			}
		} else {
			if language > 136 || len(data)%2 != 0 {
				return "", driver.ErrMalformed
			}
			values := make([]uint16, len(data)/2)
			for i := range values {
				values[i] = binary.LittleEndian.Uint16(data[2*i:])
			}
			for i := 0; i < len(values); i++ {
				v := values[i]
				if v >= 0xd800 && v <= 0xdbff {
					if i+1 >= len(values) || values[i+1] < 0xdc00 || values[i+1] > 0xdfff {
						return "", driver.ErrMalformed
					}
					text = append(text, utf16.DecodeRune(rune(v), rune(values[i+1])))
					i++
				} else if v >= 0xdc00 && v <= 0xdfff {
					return "", driver.ErrMalformed
				} else {
					text = append(text, rune(v))
				}
			}
		}
	}
	for _, c := range text {
		if unicode.IsControl(c) || c == unicode.ReplacementChar {
			return "", driver.ErrMalformed
		}
	}
	return strings.TrimSpace(string(text)), nil
}
