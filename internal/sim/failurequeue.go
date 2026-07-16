package sim

import (
	"fmt"

	"github.com/vrushabh-dhone/cx-guardian/internal/engine"
)

// FailureQueue mimics orch-entity-failure-queue's record processor. It implements
// engine.Actuator so the Healer can drive it.
//
// Real-code references:
//   - whole-agent cleanup:  recordprocessor.go:54-91 + entityoperations.go:61-99
//   - "Agent record ttl set" log marker: entityoperations.go:76
type FailureQueue struct {
	store   *Store
	tracker *engine.Tracker
}

func NewFailureQueue(store *Store, tracker *engine.Tracker) *FailureQueue {
	return &FailureQueue{store: store, tracker: tracker}
}

// WholeAgentCleanup is the CURRENT, dangerous default: expire the agent record and every
// contact it is handling. This is what produces the 5-7x cascade amplification.
func (fq *FailureQueue) WholeAgentCleanup(seedContact int64, agentNo int32) engine.HealResult {
	related := fq.tracker.HealthyContactsForAgent(agentNo, seedContact)

	// Ordered expiry: related contacts first, then the agent (matches persistencechannelprocessor).
	wiped := []int64{seedContact}
	fq.store.SetTTL(KindContact, seedContact)
	for _, c := range related {
		fq.store.SetTTL(KindContact, c)
		wiped = append(wiped, c)
	}
	fq.store.SetTTL(KindAgent, int64(agentNo)) // "Agent record ttl set"

	return engine.HealResult{
		Action:         "WHOLE_AGENT_CLEANUP",
		Quarantined:    wiped,
		Preserved:      nil,
		AgentPreserved: false,
		Message: fmt.Sprintf("failure-queue: whole-agent cleanup — agent %d + %d contacts TTL'd (1 real failure -> %d wipes)",
			agentNo, len(wiped), len(wiped)),
	}
}

// CascadeCircuitBreak is Sentinel's fix: contact-only quarantine. Expire just the seed,
// preserve the agent record and every healthy contact.
func (fq *FailureQueue) CascadeCircuitBreak(seedContact int64, agentNo int32) engine.HealResult {
	preserved := fq.tracker.HealthyContactsForAgent(agentNo, seedContact)
	fq.store.SetTTL(KindContact, seedContact) // only the genuinely-bad contact

	return engine.HealResult{
		Action:         engine.ActionCascadeCircuitBreak,
		Quarantined:    []int64{seedContact},
		Preserved:      preserved,
		AgentPreserved: true,
		Message: fmt.Sprintf("circuit breaker: contact-only quarantine — agent %d preserved, %d healthy contacts saved",
			agentNo, len(preserved)),
	}
}

func (fq *FailureQueue) Requeue(contactNo int64) engine.HealResult {
	// Reflect recovery in the store: the stuck contact re-enters matching.
	fq.store.SetState(KindContact, contactNo, "QUEUING", true)
	return engine.HealResult{Action: engine.ActionRequeue, Preserved: []int64{contactNo}, Message: fmt.Sprintf("produced RequeueContact for %d (ROUTING->QUEUING) — contact recovered", contactNo)}
}

func (fq *FailureQueue) Sync(contactNo int64) engine.HealResult {
	fq.store.SetState(KindContact, contactNo, "REFINING", true)
	return engine.HealResult{Action: engine.ActionSync, Preserved: []int64{contactNo}, Message: fmt.Sprintf("produced SyncContactV2 for %d — re-synced from DynamoDB, re-entered matching", contactNo)}
}

func (fq *FailureQueue) Terminate(contactNo int64) engine.HealResult {
	fq.store.SetState(KindContact, contactNo, "ENDED", true)
	return engine.HealResult{Action: engine.ActionTerminate, Preserved: []int64{contactNo}, Message: fmt.Sprintf("produced TerminateContact for %d — contact released, agent freed", contactNo)}
}
