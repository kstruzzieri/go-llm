package compat

import (
	"container/list"
	"sync"
	"time"
)

// CompletionRecord holds the minimum metadata needed to attribute a feedback
// signal back to the provider/model that served the original completion.
type CompletionRecord struct {
	ID        string
	Provider  string
	Model     string
	UseCase   string
	FilePath  string
	RouteInfo string
	CreatedAt time.Time
}

// completionRecordStore is an in-memory, bounded, TTL-expiring LRU.
type completionRecordStore struct {
	mu    sync.Mutex
	ttl   time.Duration
	max   int
	list  *list.List
	index map[string]*list.Element
}

func newCompletionRecordStore(ttl time.Duration, max int) *completionRecordStore {
	return &completionRecordStore{
		ttl:   ttl,
		max:   max,
		list:  list.New(),
		index: make(map[string]*list.Element, max),
	}
}

func (s *completionRecordStore) put(rec CompletionRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now()
	}
	if elem, ok := s.index[rec.ID]; ok {
		elem.Value = rec
		s.list.MoveToFront(elem)
		return
	}
	s.index[rec.ID] = s.list.PushFront(rec)
	for s.list.Len() > s.max {
		oldest := s.list.Back()
		if oldest != nil {
			s.list.Remove(oldest)
			delete(s.index, oldest.Value.(CompletionRecord).ID)
		}
	}
}

func (s *completionRecordStore) get(id string) (CompletionRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	elem, ok := s.index[id]
	if !ok {
		return CompletionRecord{}, false
	}
	rec := elem.Value.(CompletionRecord)
	if time.Since(rec.CreatedAt) > s.ttl {
		s.list.Remove(elem)
		delete(s.index, id)
		return CompletionRecord{}, false
	}
	s.list.MoveToFront(elem)
	return rec, true
}
