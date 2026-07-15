package sentinel

import (
	"sync"
	"time"
)

// ContactView is Sentinel's in-memory picture of one contact's current FSM position.
type ContactView struct {
	ContactNo int64
	AgentNo   int32
	State     ContactState
	Since     time.Time
	Healthy   bool // true until a terminal/failure event says otherwise
}

// AgentView tracks which contacts an agent is currently handling.
type AgentView struct {
	AgentNo  int32
	Contacts map[int64]struct{}
}

// Tracker maintains live FSM state for every contact and agent it has seen.
// In production this is fed by the orch-entity-streams GenericHandler; here it is
// fed by the simulator. The logic is identical.
type Tracker struct {
	mu       sync.RWMutex
	contacts map[int64]*ContactView
	agents   map[int32]*AgentView
}

func NewTracker() *Tracker {
	return &Tracker{
		contacts: map[int64]*ContactView{},
		agents:   map[int32]*AgentView{},
	}
}

// OnContactStateChange ingests a ContactStateChangeV2-equivalent event.
func (t *Tracker) OnContactStateChange(e ContactStateChange) {
	t.mu.Lock()
	defer t.mu.Unlock()

	cv, ok := t.contacts[e.ContactNo]
	if !ok {
		cv = &ContactView{ContactNo: e.ContactNo, Healthy: true}
		t.contacts[e.ContactNo] = cv
	}
	if cv.State != e.State {
		cv.State = e.State
		cv.Since = e.Timestamp
	}
	cv.AgentNo = e.AgentNo

	if e.AgentNo != 0 {
		ag, ok := t.agents[e.AgentNo]
		if !ok {
			ag = &AgentView{AgentNo: e.AgentNo, Contacts: map[int64]struct{}{}}
			t.agents[e.AgentNo] = ag
		}
		if e.State == StateEnded {
			delete(ag.Contacts, e.ContactNo)
			cv.Healthy = false
		} else {
			ag.Contacts[e.ContactNo] = struct{}{}
		}
	}
}

// Contact returns the current view for a contact (copy), if known.
func (t *Tracker) Contact(no int64) (ContactView, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	cv, ok := t.contacts[no]
	if !ok {
		return ContactView{}, false
	}
	return *cv, true
}

// HealthyContactsForAgent returns the healthy contacts an agent is handling,
// excluding the given seed contact. These are the contacts that would become
// cascade victims if the whole-agent cleanup fires.
func (t *Tracker) HealthyContactsForAgent(agentNo int32, exclude int64) []int64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	ag, ok := t.agents[agentNo]
	if !ok {
		return nil
	}
	var out []int64
	for c := range ag.Contacts {
		if c == exclude {
			continue
		}
		if cv, ok := t.contacts[c]; ok && cv.Healthy && cv.State != StateEnded {
			out = append(out, c)
		}
	}
	return out
}

// DwellExceeded reports contacts that have been in a non-terminal state longer
// than the given threshold (the basis for the stuck-in-ROUTING / stuck-in-QUEUING signals).
func (t *Tracker) DwellExceeded(state ContactState, threshold time.Duration, now time.Time) []ContactView {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var out []ContactView
	for _, cv := range t.contacts {
		if cv.State == state && cv.Healthy && now.Sub(cv.Since) > threshold {
			out = append(out, *cv)
		}
	}
	return out
}
