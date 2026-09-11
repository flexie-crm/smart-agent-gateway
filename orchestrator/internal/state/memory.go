package state

import (
	"context"
	"sync"
	"time"
)

// Memory is the store this process keeps for itself: right for one gateway,
// and behind the interface so a shared one can replace it without anything
// above noticing.
//
// Expiry is LAZY. A value past its time is not there when it is asked for, and
// what is left behind is swept when the store grows past what anybody would
// keep. A goroutine sweeping on a clock would be a second thing to shut down
// for a store that holds a few hundred small facts.
type Memory struct {
	mu     sync.Mutex
	values map[string]held
	keys   int              // the most it holds before expired ones are swept
	now    func() time.Time // moved by tests
}

type held struct {
	value []byte
	until time.Time // zero means it stays
}

func NewMemory() *Memory {
	return &Memory{values: map[string]held{}, keys: 10_000, now: time.Now}
}

func (m *Memory) Get(_ context.Context, key string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	value, found := m.values[key]
	if !found || (!value.until.IsZero() && m.now().After(value.until)) {
		delete(m.values, key)
		return nil, false, nil
	}
	// A copy, because what is handed out must not be changed under the store.
	out := make([]byte, len(value.value))
	copy(out, value.value)
	return out, true, nil
}

func (m *Memory) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	stored := held{value: value}
	if ttl > 0 {
		stored.until = m.now().Add(ttl)
	}
	m.values[key] = stored
	if len(m.values) > m.keys {
		m.sweep()
	}
	return nil
}

func (m *Memory) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.values, key)
	return nil
}

// sweep drops what has expired. Only that: a value with no time on it is
// somebody's, and this is not the place to decide it has been there too long.
func (m *Memory) sweep() {
	for key, value := range m.values {
		if !value.until.IsZero() && m.now().After(value.until) {
			delete(m.values, key)
		}
	}
}
