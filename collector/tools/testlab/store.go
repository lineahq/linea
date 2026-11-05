package main

import (
	"encoding/json"
	"sync"
	"time"
)

const maxEntries = 500
const maxBufferBytes = 8 << 20
const maxEntryBytes = 512 << 10

type entry struct {
	ID         uint64          `json:"id"`
	ReceivedAt time.Time       `json:"received_at"`
	Signal     string          `json:"signal"`
	SessionID  string          `json:"session_id"`
	Point      string          `json:"point"`
	Pipeline   string          `json:"pipeline"`
	Service    string          `json:"service"`
	Summary    string          `json:"summary"`
	Records    int             `json:"records"`
	OTLP       json.RawMessage `json:"otlp"`
}

type captureStore struct {
	mu                          sync.Mutex
	entries                     []entry
	bytes                       int
	next                        uint64
	received, evicted, rejected uint64
}

type bufferStats struct {
	Entries    int    `json:"entries"`
	Bytes      int    `json:"bytes"`
	Received   uint64 `json:"received"`
	Evicted    uint64 `json:"evicted"`
	Rejected   uint64 `json:"rejected"`
	MaxEntries int    `json:"max_entries"`
	MaxBytes   int    `json:"max_bytes"`
}

func (s *captureStore) add(item entry) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(item.OTLP) > maxEntryBytes {
		s.rejected += uint64(item.Records)
		return false
	}
	s.next++
	item.ID, item.ReceivedAt = s.next, time.Now().UTC()
	s.received += uint64(item.Records)
	s.entries = append(s.entries, item)
	s.bytes += len(item.OTLP)
	for len(s.entries) > maxEntries || s.bytes > maxBufferBytes {
		s.bytes -= len(s.entries[0].OTLP)
		s.evicted += uint64(s.entries[0].Records)
		s.entries[0] = entry{}
		s.entries = s.entries[1:]
	}
	return true
}
func (s *captureStore) reject(records int) { s.mu.Lock(); s.rejected += uint64(records); s.mu.Unlock() }
func (s *captureStore) stats() bufferStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return bufferStats{len(s.entries), s.bytes, s.received, s.evicted, s.rejected, maxEntries, maxBufferBytes}
}
func (s *captureStore) clear() { s.mu.Lock(); s.entries = nil; s.bytes = 0; s.mu.Unlock() }

type eventPage struct {
	Entries   []entry `json:"entries"`
	NextAfter uint64  `json:"next_after"`
	Missed    bool    `json:"missed"`
}

func (s *captureStore) page(after uint64) eventPage {
	s.mu.Lock()
	defer s.mu.Unlock()
	page := eventPage{Entries: make([]entry, 0), NextAfter: after}
	if len(s.entries) > 0 && after > 0 && after < s.entries[0].ID-1 {
		page.Missed = true
	}
	for _, item := range s.entries {
		if item.ID > after {
			page.Entries = append(page.Entries, item)
			page.NextAfter = item.ID
			if len(page.Entries) == 200 {
				break
			}
		}
	}
	return page
}
