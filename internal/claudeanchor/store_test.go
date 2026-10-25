package claudeanchor

import (
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestStoreBoundsExpiryAndConcurrentRenewal(t *testing.T) {
	var s Store[int]
	now := time.Now()
	for i := range Limit + 1 {
		s.Put(strconv.Itoa(i), i, now)
	}
	if _, ok := s.Get("0", now); ok || len(s.items) != Limit {
		t.Fatal("store exceeded bound")
	}
	if _, ok := s.Get("1", now.Add(Lifetime)); ok {
		t.Fatal("expired anchor retained")
	}
	var wg sync.WaitGroup
	for n := range 8 {
		wg.Go(func() {
			for range 100 {
				s.Put("shared", n, now)
				s.Get("shared", now)
			}
		})
	}
	wg.Wait()
	if _, ok := s.Get("shared", now.Add(Lifetime-time.Second)); !ok {
		t.Fatal("renewed anchor lost")
	}
	s.Put("", 1, now)
	if _, ok := s.Get("", now); ok {
		t.Fatal("empty anchor accepted")
	}
}
