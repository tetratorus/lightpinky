package screen

import (
	"strconv"
	"sync"
)

// Refs assigns small integer refs to the elements of one device's screens.
// A ref stays the same while an element keeps its role, label, id, ancestry
// and position among identical siblings. Use one Refs per device.
type Refs struct {
	mu    sync.Mutex
	next  int
	gen   int
	byKey map[string]*refEntry
}

type refEntry struct {
	ref  int
	seen int
}

// pruneAfter is how many screens a key may go unseen before it is dropped.
const pruneAfter = 200

// NewRefs returns an empty assigner whose first ref is 1.
func NewRefs() *Refs { return &Refs{next: 1, byKey: map[string]*refEntry{}} }

// Assign sets Ref on every tree element of s. OCR elements get no ref.
func (r *Refs) Assign(s *Screen) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gen++
	dups := map[string]int{}
	for i := range s.Elements {
		e := &s.Elements[i]
		if e.Source == SourceOCR {
			e.Ref = 0
			continue
		}
		base := e.Role + "\x00" + e.Label + "\x00" + e.ID + "\x00" + e.Path
		n := dups[base]
		dups[base] = n + 1
		k := base + "\x00" + strconv.Itoa(n)
		ent, ok := r.byKey[k]
		if !ok {
			ent = &refEntry{ref: r.next}
			r.next++
			r.byKey[k] = ent
		}
		ent.seen = r.gen
		e.Ref = ent.ref
	}
	if len(r.byKey) > 4096 {
		for k, ent := range r.byKey {
			if r.gen-ent.seen > pruneAfter {
				delete(r.byKey, k)
			}
		}
	}
}
