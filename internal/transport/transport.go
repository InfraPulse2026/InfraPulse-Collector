// Package transport defines the outbound connection boundary.
package transport

import (
	"InfraPulseAgent/internal/model"
	"context"
)

type Transport interface {
	Connect(context.Context) (Session, error)
}

// EnvelopeValidator prevents an unsendable normalized observation from entering
// a durable outbox. Protocol-specific encoding and size checks stay in transport.
type EnvelopeValidator interface {
	ValidateEnvelope(model.Envelope) error
}
type Session interface {
	Send(model.Envelope) error
	Receive() (model.ControllerMessage, error)
	// Close must cancel and unblock concurrent Send and Receive calls. It is
	// idempotent; implementations must not wait for an unresponsive peer.
	Close() error
}

// SendCloser optionally finishes the outbound stream while leaving Receive
// active. The single sender calls it after its final Send. A peer EOF can then
// confirm graceful stream completion without an application delivery ACK.
type SendCloser interface {
	CloseSend() error
}
