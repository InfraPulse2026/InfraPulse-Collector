package logs

import (
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"InfraPulseAgent/internal/model"
)

var (
	bsdHeader       = regexp.MustCompile(`^(Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec) [ 0-9][0-9] [0-9]{2}:[0-9]{2}:[0-9]{2} ([^ ]+) (.*)$`)
	tag             = regexp.MustCompile(`^([A-Za-z0-9_./@-]+)(?:\[([0-9]+)\])?: ?(.*)$`)
	auditHead       = regexp.MustCompile(`(?:^| )type=([A-Z0-9_]+) msg=audit\(([0-9]+)(?:\.([0-9]{1,9}))?:([0-9]+)\):(?: |$)`)
	auditKV         = regexp.MustCompile(`(?:^| )([a-z_]+)=("[^"\r\n]*"|'[^'\r\n]*'|[^ \r\n]+)`)
	remoteAuditHead = regexp.MustCompile(`^(?:node=[^ \r\n]+ )?type=[A-Z0-9_]+ msg=audit\(`)
	// RFC5424 narrows RFC3339: uppercase T/Z, seconds below 60, at most six
	// fractional digits, and an offset whose hour/minute are valid clock fields.
	rfc5424Time = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T(?:[01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9](?:\.[0-9]{1,6})?(?:Z|[+-](?:[01][0-9]|2[0-3]):[0-5][0-9])$`)
)

// Parse preserves the source line and adds only fields established by a known
// grammar. RFC3164 omits the year and timezone, so no absolute timestamp is
// invented for that format. Audit syscall numbers remain numbers: decoding
// them without the architecture would produce incorrect names.
func Parse(line, format string, observed time.Time) model.LogRecord {
	r := model.LogRecord{ObservedAt: observed, Message: line, Raw: line}
	if format == "auditd" {
		parseAudit(&r, line)
		return r
	}
	if format == "raw" {
		return r
	}
	r.Syslog = &model.SyslogMetadata{Format: "unknown", ParseStatus: "fallback"}
	body := line
	var priority *uint32
	if strings.HasPrefix(body, "<") {
		end := strings.IndexByte(body, '>')
		if end >= 2 && end <= 4 {
			value, err := strconv.ParseUint(body[1:end], 10, 32)
			if err == nil && decimalDigits(body[1:end]) && value <= 191 && (body[1] != '0' || end == 2) {
				v := uint32(value)
				priority = &v
				body = body[end+1:]
			}
		}
	}
	if priority != nil {
		severity, facility := *priority%8, *priority/8
		r.Severity, r.Facility = &severity, &facility
	}
	if priority != nil && strings.HasPrefix(body, "1 ") {
		r.Syslog.Format = "rfc5424"
		parts := strings.SplitN(body, " ", 7)
		if len(parts) != 7 || parts[0] != "1" || !headerToken(parts[2], 255) || !headerToken(parts[3], 48) || !headerToken(parts[4], 128) || !headerToken(parts[5], 32) {
			return r
		}
		var ts *time.Time
		if parts[1] != "-" {
			v, err := time.Parse(time.RFC3339Nano, parts[1])
			if err != nil || v.Year() < 1 || !rfc5424Time.MatchString(parts[1]) {
				return r
			}
			tu := v.UTC()
			ts = &tu
		}
		structuredData, message, ok := structuredMessage(parts[6])
		if !ok {
			return r
		}
		r.OriginalTimestamp, r.Host, r.Process, r.ProcessID, r.Message = ts, nilValue(parts[2]), nilValue(parts[3]), nilValue(parts[4]), message
		r.Syslog.Version, r.Syslog.MessageID, r.Syslog.StructuredData, r.Syslog.RawTimestamp, r.Syslog.ParseStatus = 1, nilValue(parts[5]), structuredData, parts[1], "parsed"
		return r
	}
	if match := bsdHeader.FindStringSubmatch(body); match != nil {
		r.Syslog.Format = "rfc3164"
		// Validate clock/calendar syntax, without interpreting the absent year.
		if _, err := time.Parse("Jan _2 15:04:05", body[:15]); err != nil {
			return r
		}
		r.Message = match[3]
		if headerToken(match[2], 255) {
			r.Host = match[2]
		}
		parseTag(&r)
		r.Syslog.RawTimestamp, r.Syslog.ParseStatus = body[:15], "parsed"
		return r
	}
	// rsyslog's RSYSLOG_FileFormat uses an RFC3339 timestamp followed by
	// hostname and the conventional process tag. Custom templates fall back.
	parts := strings.SplitN(body, " ", 3)
	if len(parts) == 3 && headerToken(parts[1], 255) {
		if ts, err := time.Parse(time.RFC3339Nano, parts[0]); err == nil && ts.Year() >= 1 {
			tu := ts.UTC()
			r.OriginalTimestamp, r.Host, r.Message = &tu, parts[1], parts[2]
			parseTag(&r)
			r.Syslog.RawTimestamp, r.Syslog.ParseStatus = parts[0], "parsed"
		}
	}
	return r
}

func decimalDigits(v string) bool {
	if v == "" {
		return false
	}
	for i := range len(v) {
		if v[i] < '0' || v[i] > '9' {
			return false
		}
	}
	return true
}

func parseTag(r *model.LogRecord) {
	if m := tag.FindStringSubmatch(r.Message); m != nil && len(m[1]) <= 1024 && len(m[2]) <= 128 {
		r.Process, r.ProcessID, r.Message = m[1], m[2], m[3]
	}
}

func nilValue(v string) string {
	if v == "-" {
		return ""
	}
	return v
}

func headerToken(v string, limit int) bool {
	if len(v) < 1 || len(v) > limit {
		return false
	}
	for _, ch := range v {
		if ch < 33 || ch > 126 {
			return false
		}
	}
	return true
}

// structuredMessage validates SD boundaries without decoding or rewriting its
// content. Unknown backslash escapes remain literal, as required by RFC5424.
// The message is opaque octets: BOM, spaces, and embedded newlines survive.
func structuredMessage(v string) (structuredData, message string, ok bool) {
	if v == "-" {
		return "-", "", true
	}
	if strings.HasPrefix(v, "- ") {
		return "-", v[2:], true
	}
	if !strings.HasPrefix(v, "[") {
		return "", "", false
	}
	ids := make(map[string]bool)
	for i := 0; i < len(v) && v[i] == '['; {
		i++
		start := i
		for i < len(v) && sdNameByte(v[i]) {
			i++
		}
		if i-start < 1 || i-start > 32 || ids[v[start:i]] {
			return "", "", false
		}
		ids[v[start:i]] = true
		for i < len(v) && v[i] == ' ' {
			i++
			start = i
			for i < len(v) && sdNameByte(v[i]) {
				i++
			}
			if i-start < 1 || i-start > 32 || i+1 >= len(v) || v[i] != '=' || v[i+1] != '"' {
				return "", "", false
			}
			i += 2
			start = i
			for i < len(v) && v[i] != '"' {
				if v[i] == ']' {
					return "", "", false
				}
				if v[i] == '\\' && i+1 < len(v) && (v[i+1] == '"' || v[i+1] == '\\' || v[i+1] == ']') {
					i += 2
				} else {
					i++
				}
			}
			if i >= len(v) || !utf8.ValidString(v[start:i]) {
				return "", "", false
			}
			i++ // closing quote; another parameter or the element end follows
		}
		if i >= len(v) || v[i] != ']' {
			return "", "", false
		}
		i++
		if i == len(v) {
			return v, "", true
		}
		if v[i] == ' ' {
			return v[:i], v[i+1:], true
		}
		if v[i] != '[' {
			return "", "", false
		}
	}
	return "", "", false
}

func sdNameByte(b byte) bool {
	return b >= 33 && b <= 126 && b != '=' && b != ']' && b != '"'
}

// AttachAudit is used only after the caller establishes a configured audit
// plugin context. The kernel grammar is required in addition to that context;
// ordinary syslog text containing "type=" never enables audit detection. The
// enclosing syslog timestamp and HOSTNAME remain independent sender claims.
func AttachAudit(r *model.LogRecord) bool {
	if r == nil {
		return false
	}
	var parsed model.LogRecord
	message := strings.TrimPrefix(r.Message, "\ufeff")
	if !remoteAuditHead.MatchString(message) {
		return false
	}
	parseAudit(&parsed, message)
	if parsed.Audit == nil {
		return false
	}
	r.Audit = parsed.Audit
	return true
}

func parseAudit(r *model.LogRecord, line string) {
	m := auditHead.FindStringSubmatch(line)
	if m == nil || len(m[1]) > 64 {
		return
	}
	if _, err := strconv.ParseUint(m[4], 10, 64); err != nil {
		return
	}
	seconds, err := strconv.ParseInt(m[2], 10, 64)
	if err != nil || seconds < 0 || seconds > 253402300799 {
		return
	}
	fraction := m[3]
	var ns int64
	if fraction != "" {
		ns, err = strconv.ParseInt(fraction+strings.Repeat("0", 9-len(fraction)), 10, 64)
		if err != nil {
			return
		}
	}
	timestamp := time.Unix(seconds, ns).UTC()
	r.OriginalTimestamp = &timestamp
	eventTimestamp := m[2]
	if fraction != "" {
		eventTimestamp += "." + fraction
	}
	a := &model.AuditRecord{EventID: eventTimestamp + ":" + m[4], RecordType: m[1]}
	// Conflicting duplicate fields are omitted. Fields nested inside a quoted
	// msg value are not treated as independent top-level audit attributes.
	values := map[string]string{}
	duplicates := map[string]bool{}
	for _, kv := range auditKV.FindAllStringSubmatch(line, -1) {
		if old, ok := values[kv[1]]; ok && old != kv[2] {
			duplicates[kv[1]] = true
		}
		values[kv[1]] = kv[2]
	}
	get := func(key string) string {
		if duplicates[key] {
			return ""
		}
		v := values[key]
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
		return v
	}
	integer := func(key string) *uint32 {
		v, err := strconv.ParseUint(get(key), 10, 32)
		if err != nil {
			return nil
		}
		n := uint32(v)
		return &n
	}
	a.UID, a.AUID, a.PID = integer("uid"), integer("auid"), integer("pid")
	// Quoted executable names are known strings. Hex-encoded executable data
	// is left in Raw rather than guessed to be text.
	if v := values["exe"]; !duplicates["exe"] && len(v) <= 4098 && strings.HasPrefix(v, "\"") && strings.HasSuffix(v, "\"") {
		a.Executable = get("exe")
	}
	if v := get("syscall"); v != "" {
		if _, err := strconv.ParseUint(v, 10, 32); err == nil {
			a.Syscall = v
		}
	}
	if v := get("success"); v == "yes" || v == "no" {
		success := v == "yes"
		a.Success = &success
	}
	if v := get("exit"); v != "" {
		if _, err := strconv.ParseInt(v, 10, 64); err == nil {
			a.Result = v
		}
	}
	if v := get("node"); headerToken(v, 255) {
		a.Host, r.Host = v, v
	}
	r.Audit = a
}

// correlateAuditBatch groups records using only the kernel's exact
// timestamp/serial identifier plus source and node. Its memory is bounded by
// the batch, and records remain separate. Groups can be partial or span more
// than one batch; no completeness or cross-restart correlation is invented.
func correlateAuditBatch(records []model.LogRecord) []model.LogRecord {
	indices := make(map[string]int, len(records))
	groups := make([][]model.LogRecord, 0, len(records))
	for _, record := range records {
		key := ""
		if record.Audit != nil && record.Audit.EventID != "" {
			key = record.DeviceID + "\x00" + record.SourceID + "\x00" + record.Audit.Host + "\x00" + record.Audit.EventID
		}
		index, found := indices[key]
		if key == "" || !found {
			index = len(groups)
			groups = append(groups, nil)
			if key != "" {
				indices[key] = index
			}
		}
		groups[index] = append(groups[index], record)
	}
	result := make([]model.LogRecord, 0, len(records))
	for _, group := range groups {
		result = append(result, group...)
	}
	return result
}
