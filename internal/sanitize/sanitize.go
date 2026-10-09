// Package sanitize redacts known secret fields in free-text event/error payloads.
// It does not promise exhaustive anonymization or inspect arbitrary binary data.
package sanitize

import (
	"InfraPulseAgent/internal/model"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

type DataSanitizer interface {
	Sanitize(model.Envelope) model.Envelope
}
type KnownSecrets struct {
	// Values are explicitly configured credential values, never printed or
	// serialized. They supplement field-name patterns in arbitrary log text.
	Values []string
}

var secret = regexp.MustCompile(`(?i)["']?\b(password|passwd|api[_-]?key|token|secret|community|enrollment[_-]?token|authorization|private[_-]?key)\b["']?\s*[:=]\s*(?:"[^"]*"|'[^']*'|[^\s,;]+)`)
var bearer = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9+/_.=~\-]+`)
var privateKey = regexp.MustCompile(`(?is)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?(?:-----END [A-Z0-9 ]*PRIVATE KEY-----|$)`)

func Text(s string) string {
	return secret.ReplaceAllString(bearer.ReplaceAllString(privateKey.ReplaceAllString(s, "[REDACTED PRIVATE KEY]"), "$1 [REDACTED]"), "$1=[REDACTED]")
}

// TextWithSecrets applies known patterns before one-pass literal replacement.
// Replacement text is never processed again by another configured value. It
// intentionally does not promise discovery
// of unknown secrets or exhaustive anonymization of arbitrary text.
func TextWithSecrets(s string, values []string) string {
	s = Text(s)
	if len(values) == 0 {
		return s
	}
	secrets := append([]string(nil), values...)
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	marker := literalMarker(secrets)
	if marker == "" {
		// No safe marker is available for this unusual credential set. Empty
		// text cannot retain a nonempty configured value or recreate one by
		// joining text on opposite sides of a removed value.
		return ""
	}
	type replacement struct{ old, new string }
	replacements := make([]replacement, 0, len(secrets)+len(redactionMarkers))
	previous := ""
	for _, value := range secrets {
		if value != "" && value != previous && len(value) <= len(s) {
			replacements = append(replacements, replacement{value, marker})
		}
		previous = value
	}
	for _, known := range redactionMarkers {
		if markerSafe(known, secrets) {
			replacements = append(replacements, replacement{known, known})
		} else {
			replacements = append(replacements, replacement{known, marker})
		}
	}
	// Full credentials win over a shorter marker prefix. This also redacts
	// credentials equal to the marker or containing one of its substrings.
	sort.SliceStable(replacements, func(i, j int) bool { return len(replacements[i].old) > len(replacements[j].old) })
	pairs := make([]string, 0, 2*len(replacements))
	for _, replacement := range replacements {
		pairs = append(pairs, replacement.old, replacement.new)
	}
	return strings.NewReplacer(pairs...).Replace(s)
}

var redactionMarkers = []string{
	"[REDACTED]",
	"[REDACTED PRIVATE KEY]",
	"[OVERSIZED LOG LINE REDACTED]",
	"[LOG RECORD EXCEEDS BATCH LIMIT; REDACTED]",
	"[SANITIZED LOG LINE EXCEEDS LIMIT; REDACTED]",
}

func markerSafe(marker string, values []string) bool {
	// A marker uses only characters absent from every configured value. A
	// literal cannot reappear inside it or across its boundary with source
	// text. Single-character markers cover short/overlapping credentials.
	for _, value := range values {
		if strings.ContainsAny(value, marker) {
			return false
		}
	}
	return true
}

func literalMarker(values []string) string {
	for _, marker := range []string{"[REDACTED]", "█", "■", "●", "�", "~", "#", "!"} {
		if markerSafe(marker, values) {
			return marker
		}
	}
	return ""
}

func (k KnownSecrets) Sanitize(e model.Envelope) model.Envelope {
	text := func(s string) string { return TextWithSecrets(s, k.Values) }
	e = sanitizeHardware(e, text)
	if e.Error != nil {
		v := *e.Error
		v.Message = text(v.Message)
		e.Error = &v
	}
	if e.CommandResult != nil {
		v := *e.CommandResult
		v.ErrorMessage = text(v.ErrorMessage)
		e.CommandResult = &v
	}
	if len(e.Events) > 0 {
		events := append([]model.Event(nil), e.Events...)
		for i := range events {
			events[i].Message = text(events[i].Message)
		}
		e.Events = events
	}
	if e.Logs != nil {
		batch := model.CloneLogBatch(*e.Logs)
		for i := range batch.Records {
			r := &batch.Records[i]
			r.Message, r.Raw = text(r.Message), text(r.Raw)
			r.Host, r.Process, r.ProcessID = text(r.Host), text(r.Process), text(r.ProcessID)
			if r.Syslog != nil {
				s := r.Syslog
				s.MessageID, s.StructuredData, s.RawTimestamp = text(s.MessageID), text(s.StructuredData), text(s.RawTimestamp)
				if len(s.RawPayload) > 0 {
					cleaned := text(string(s.RawPayload))
					if utf8.ValidString(cleaned) {
						s.RawPayload = nil
					} else {
						s.RawPayload = []byte(cleaned)
					}
				}
			}
			if r.Audit != nil {
				audit := *r.Audit
				audit.Executable, audit.Result, audit.Host = text(audit.Executable), text(audit.Result), text(audit.Host)
				r.Audit = &audit
			}
		}
		e.Logs = &batch
	}
	return e
}
