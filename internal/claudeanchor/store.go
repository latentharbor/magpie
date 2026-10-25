// Package claudeanchor keeps bounded, process-local ownership metadata for
// native Claude anchors. It never stores credentials or conversation content.
package claudeanchor

import (
	"container/list"
	"sync"
	"time"
)

const Limit = 8192
const Lifetime = 24 * time.Hour

type entry[T any] struct {
	key   string
	value T
	at    time.Time
}

// Store's zero value is ready to use. Reads do not prolong an anchor's life:
// only an upstream success can renew one. Lifetime is a local memory bound,
// not a promise about the vendor's Thread or prompt-cache lifetime.
type Store[T any] struct {
	mu    sync.Mutex
	items map[string]*list.Element
	order list.List
}

func (s *Store[T]) Put(key string, value T, now time.Time) {
	if key == "" || len(key) > 1024 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.items == nil {
		s.items = make(map[string]*list.Element)
	}
	if e := s.items[key]; e != nil {
		e.Value = entry[T]{key, value, now}
		s.order.MoveToBack(e)
		return
	}
	for s.order.Len() >= Limit {
		e := s.order.Front()
		delete(s.items, e.Value.(entry[T]).key)
		s.order.Remove(e)
	}
	s.items[key] = s.order.PushBack(entry[T]{key, value, now})
}

func (s *Store[T]) Get(key string, now time.Time) (T, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var zero T
	e := s.items[key]
	if e == nil {
		return zero, false
	}
	v := e.Value.(entry[T])
	if now.Sub(v.at) >= Lifetime {
		delete(s.items, key)
		s.order.Remove(e)
		return zero, false
	}
	return v.value, true
}
