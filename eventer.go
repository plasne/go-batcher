package batcher

import (
	"sync"

	"github.com/google/uuid"
)

type eventer struct {
	listenerMutex sync.RWMutex
	listeners     map[uuid.UUID]func(event string, val int, msg *string)
}

type ieventer interface {
	AddListener(fn func(event string, val int, msg *string)) uuid.UUID
	RemoveListener(id uuid.UUID)
	emit(event string, val int, msg *string)
}

func (r *eventer) AddListener(fn func(event string, val int, msg *string)) uuid.UUID {

	// lock
	r.listenerMutex.Lock()
	defer r.listenerMutex.Unlock()

	// allocate
	if r.listeners == nil {
		r.listeners = make(map[uuid.UUID]func(event string, val int, msg *string))
	}

	// add a new listener
	id := uuid.New()
	r.listeners[id] = fn

	return id
}

func (r *eventer) RemoveListener(id uuid.UUID) {

	// lock
	r.listenerMutex.Lock()
	defer r.listenerMutex.Unlock()

	// remove
	delete(r.listeners, id)

}

func (r *eventer) emit(event string, val int, msg *string) {

	// lock
	r.listenerMutex.RLock()
	defer r.listenerMutex.RUnlock()

	// emit
	for _, fn := range r.listeners {
		fn(event, val, msg)
	}

}
