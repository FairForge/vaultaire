package engine

import (
	"reflect"
	"sort"
)

// StoreIdentifier is an optional driver interface: the store the driver
// writes into, as one string two drivers can be compared by — an S3 endpoint
// plus bucket, a directory, a fleet of accounts. Two registered names that
// resolve to the same store break an invariant every stale-copy path relies
// on (WP-R13-2): "the backend the key does not route to" is then the same
// bucket under another name, and deleting a stale copy there deletes the
// live object. The boot check (WP-R7-5) reports it; nothing in the engine
// acts on it.
type StoreIdentifier interface {
	StoreID() string
}

// SharedStore is one store that more than one registered name writes into.
type SharedStore struct {
	// Store is the StoreID the drivers reported, or "<same driver value>"
	// when the same driver instance was registered twice.
	Store string
	// Backends are the registered names, sorted.
	Backends []string
}

// SharedStores reports every store that two or more registered backends
// share: the same driver value under two names, or two drivers whose StoreID
// is equal. A driver that does not implement StoreIdentifier is compared by
// identity only. Sorted by store.
func (e *CoreEngine) SharedStores() []SharedStore {
	e.mu.RLock()
	defer e.mu.RUnlock()

	byStore := map[string][]string{}
	for name, d := range e.drivers {
		key := "<same driver value> " + driverIdentity(d)
		if si, ok := d.(StoreIdentifier); ok {
			if id := si.StoreID(); id != "" {
				key = id
			}
		}
		byStore[key] = append(byStore[key], name)
	}
	var out []SharedStore
	for store, names := range byStore {
		if len(names) < 2 {
			continue
		}
		sort.Strings(names)
		out = append(out, SharedStore{Store: store, Backends: names})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Store < out[j].Store })
	return out
}

// driverIdentity is a stable string for a driver value: its pointer for a
// pointer type, else its type name (a non-pointer driver compares as its
// type, which is the best "same value" we can say without comparing fields).
func driverIdentity(d Driver) string {
	v := reflect.ValueOf(d)
	if v.Kind() == reflect.Pointer && !v.IsNil() {
		return v.Type().String() + "@" + ptrString(v.Pointer())
	}
	return v.Type().String()
}

func ptrString(p uintptr) string {
	const digits = "0123456789abcdef"
	if p == 0 {
		return "0"
	}
	var b [16]byte
	i := len(b)
	for p > 0 {
		i--
		b[i] = digits[p&0xf]
		p >>= 4
	}
	return string(b[i:])
}
