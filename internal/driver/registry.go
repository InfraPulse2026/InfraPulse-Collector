package driver

import (
	"errors"
	"sync"
)

// Constructor keeps protocol-specific input at the composition root. Registry
// does not depend on YAML, credentials, transport or any concrete protocol.
type Constructor[T any] func(T) (Adapter, error)

// Registry selects statically registered constructors; it is not a dynamic
// plugin loader. Registration normally finishes before the runtime starts.
type Registry[T any] struct {
	mu           sync.RWMutex
	constructors map[string]Constructor[T]
}

func NewRegistry[T any]() *Registry[T] {
	return &Registry[T]{constructors: make(map[string]Constructor[T])}
}

func (r *Registry[T]) Register(protocol string, constructor Constructor[T]) error {
	if protocol == "" || len(protocol) > 128 || constructor == nil {
		return errors.New("invalid adapter registration")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.constructors[protocol]; exists {
		return errors.New("adapter already registered")
	}
	r.constructors[protocol] = constructor
	return nil
}

func (r *Registry[T]) Create(protocol string, input T) (Adapter, error) {
	r.mu.RLock()
	constructor, exists := r.constructors[protocol]
	r.mu.RUnlock()
	if !exists {
		return nil, errors.New("remote driver unsupported")
	}
	adapter, err := constructor(input)
	if err == nil && adapter == nil {
		return nil, errors.New("adapter constructor returned no adapter")
	}
	return adapter, err
}
