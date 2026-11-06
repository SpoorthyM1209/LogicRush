package engine

import "sync"

// IndexEntry stores the on-disk location and length of a value
type IndexEntry struct {
	Offset int64
	Length uint32
}

// Index is the thread-safe in-memory hash index
type Index struct {
	mu      sync.RWMutex
	entries map[string]IndexEntry
}

// NewIndex initializes an index with pre-allocated capacity to avoid rehashing overhead
func NewIndex(capacity int) *Index {
	if capacity < 16 {
		capacity = 16
	}
	return &Index{
		entries: make(map[string]IndexEntry, capacity),
	}
}

// Put updates or inserts an entry
func (idx *Index) Put(key string, entry IndexEntry) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.entries[key] = entry
}

// Get looks up an entry
func (idx *Index) Get(key string) (IndexEntry, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	entry, found := idx.entries[key]
	return entry, found
}

// Delete removes an entry
func (idx *Index) Delete(key string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	delete(idx.entries, key)
}

// Count returns the number of active keys
func (idx *Index) Count() int {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return len(idx.entries)
}

// Keys returns all active keys
func (idx *Index) Keys() []string {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	keys := make([]string, 0, len(idx.entries))
	for k := range idx.entries {
		keys = append(keys, k)
	}
	return keys
}
