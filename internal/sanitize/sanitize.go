// Package sanitize redacts known secret fields in free-text event/error payloads.
// It does not promise exhaustive anonymization or inspect arbitrary binary data.
package sanitize

import (
	"InfraPulseAgent/internal/model"
	"regexp"
)

type DataSanitizer interface {
	Sanitize(model.Envelope) model.Envelope
}
type KnownSecrets struct{}

var secret = regexp.MustCompile(`(?i)["']?\b(password|passwd|api[_-]?key|token|secret|community|enrollment[_-]?token|authorization|private[_-]?key)\b["']?\s*[:=]\s*(?:"[^"]*"|'[^']*'|[^\s,;]+)`)
var bearer = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9+/_.=~\-]+`)

func Text(s string) string {
	return secret.ReplaceAllString(bearer.ReplaceAllString(s, "$1 [REDACTED]"), "$1=[REDACTED]")
}
func (KnownSecrets) Sanitize(e model.Envelope) model.Envelope {
	if e.Error != nil {
		v := *e.Error
		v.Message = Text(v.Message)
		e.Error = &v
	}
	if e.CommandResult != nil {
		v := *e.CommandResult
		v.ErrorMessage = Text(v.ErrorMessage)
		e.CommandResult = &v
	}
	if len(e.Events) > 0 {
		events := append([]model.Event(nil), e.Events...)
		for i := range events {
			events[i].Message = Text(events[i].Message)
		}
		e.Events = events
	}
	return e
}
