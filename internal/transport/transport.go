// Package transport defines the outbound connection boundary.
package transport

import (
	"InfraPulseAgent/internal/model"
	"context"
)

type Transport interface {
	Connect(context.Context) (Session, error)
}
type Session interface {
	Send(model.Envelope) error
	Receive() (model.ControllerMessage, error)
	// Close must cancel and unblock concurrent Send and Receive calls. It is
	// idempotent; implementations must not wait for an unresponsive peer.
	Close() error
}
