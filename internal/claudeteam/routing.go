package claudeteam

import (
	"sync"
	"time"
)

// Weight is the neutral observation passed across the host boundary. The
// Team module does not import the router, which itself depends on providers.
type Weight struct {
	Key                    string
	Account, Known, Learns bool
	Used, Pace             float64
	Weight                 int
	Soon                   []time.Time
}

var ordering struct {
	sync.RWMutex
	f func(string, string, []Weight) []int
}

// RouteWith attaches the host's ordinary strategies. It is installed once by
// the gateway; other hosts can supply their own without an inference backend.
func RouteWith(f func(string, string, []Weight) []int) func() {
	ordering.Lock()
	old := ordering.f
	ordering.f = f
	ordering.Unlock()
	return func() { ordering.Lock(); ordering.f = old; ordering.Unlock() }
}

func routeOrder(policy, pool string, weights []Weight) []int {
	ordering.RLock()
	f := ordering.f
	ordering.RUnlock()
	if f != nil {
		order := f(policy, pool, weights)
		seen := make([]bool, len(weights))
		valid := len(order) == len(weights)
		for _, i := range order {
			if i < 0 || i >= len(seen) || seen[i] {
				valid = false
				break
			}
			seen[i] = true
		}
		if valid {
			return order
		}
	}
	order := make([]int, len(weights))
	for i := range order {
		order[i] = i
	}
	return order
}
