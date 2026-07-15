// Package sim is the in-memory simulation that stands in for the real EM runtime during
// the hackathon demo: a mock DynamoDB (with TTL semantics) plus the failure-queue cleanup
// logic. Swap this package for real Kafka + DynamoDB and the sentinel engine is unchanged.
package sim

import (
	"sync"
)

// RecordKind distinguishes agent records from contact records in the mock table.
type RecordKind string

const (
	KindAgent   RecordKind = "AGENT"
	KindContact RecordKind = "CONTACT"
)

// Record is one row in the mock DynamoDB table.
type Record struct {
	Kind      RecordKind
	ID        int64 // contactNo or agentNo
	AgentNo   int32
	State     string
	TTLSet    bool // true once a TTL delta has been applied (== "about to be deleted")
	Wiped     bool // true once the TTL has elapsed and the row is gone
	Stuck     bool // true while a contact is detected stuck (e.g. ROUTING past ring timeout)
	Recovered bool // true once a stuck contact was healed (e.g. requeued)
}

// SetState updates a record's FSM state and clears the stuck flag (used by remediation).
func (s *Store) SetState(kind RecordKind, id int64, state string, recovered bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.records[key(kind, id)]; ok {
		r.State = state
		r.Stuck = false
		r.Recovered = recovered
	}
}

// Store is a tiny thread-safe stand-in for the EM DynamoDB table.
type Store struct {
	mu      sync.RWMutex
	records map[string]*Record
}

func NewStore() *Store {
	return &Store{records: map[string]*Record{}}
}

func key(kind RecordKind, id int64) string {
	return string(kind) + "#" + itoa(id)
}

func (s *Store) Put(r *Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[key(r.Kind, r.ID)] = r
}

func (s *Store) Get(kind RecordKind, id int64) (*Record, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.records[key(kind, id)]
	return r, ok
}

// SetTTL marks a record for expiry (mirrors SetPersistenceRecordTimeToLiveDelta with delta=1s).
func (s *Store) SetTTL(kind RecordKind, id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.records[key(kind, id)]; ok {
		r.TTLSet = true
	}
}

// ExpireDue simulates the TTL sweeper deleting every row whose TTL has elapsed.
func (s *Store) ExpireDue() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.records {
		if r.TTLSet {
			r.Wiped = true
		}
	}
}

// Snapshot returns a copy of all records for rendering in the UI.
func (s *Store) Snapshot() []Record {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Record, 0, len(s.records))
	for _, r := range s.records {
		out = append(out, *r)
	}
	return out
}

func itoa(i int64) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}
