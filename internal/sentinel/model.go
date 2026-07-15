// Package sentinel contains the core detect -> diagnose -> heal loop of EM Sentinel.
//
// The types here mirror the real Entity Management event contracts so that, during
// the hackathon, the in-memory simulation can be swapped for a real orch-entity-streams
// Kafka consumer with minimal change:
//
//   - ContactState        <-> cxone.Common.UnifiedContactState
//   - AgentContactState    <-> cxone.Common.UnifiedAgentContactState
//   - ContactStateChange   <-> ContactStateChangeV2 (orch-entity-event-contracts)
//   - AgentContactChange   <-> AgentContactStateChangeV2
package sentinel

import "time"

// ContactState is the Unified Contact FSM state (9 states). See kb-articles/orch-entity-fsm.md.
type ContactState string

const (
	StateCreated   ContactState = "CREATED"
	StateQueuing   ContactState = "QUEUING"
	StateRefining  ContactState = "REFINING"
	StateRouting   ContactState = "ROUTING"
	StatePreviewing ContactState = "PREVIEWING"
	StateConnecting ContactState = "CONNECTING"
	StateWithAgent  ContactState = "WITH_AGENT"
	StateIdle       ContactState = "IDLE"
	StateEnded      ContactState = "ENDED"
)

// AgentContactState is the Agent Contact FSM state (6 states).
type AgentContactState string

const (
	AgentCreated         AgentContactState = "CREATED"
	AgentPreview         AgentContactState = "PREVIEW"
	AgentConnecting      AgentContactState = "CONNECTING"
	AgentContactWork     AgentContactState = "CONTACT_WORK"
	AgentAfterContactWork AgentContactState = "AFTER_CONTACT_WORK"
	AgentEnded           AgentContactState = "ENDED"
)

// ContactStateChange mirrors ContactStateChangeV2 (the fields Sentinel actually reads).
type ContactStateChange struct {
	ContactNo int64
	ContactID string
	AgentNo   int32
	State     ContactState
	Trigger   string
	Timestamp time.Time
	BusNo     int32
	TenantID  string
}

// AgentContactStateChange mirrors AgentContactStateChangeV2.
type AgentContactStateChange struct {
	ContactNo         int64
	AgentNo           int32
	State             AgentContactState
	ACWTimeoutSeconds int32
	Timestamp         time.Time
}

// FailureRecord mirrors the base64-JSON payload the failure-queue Lambda consumes from SQS.
// A record carrying an AgentNo is what triggers the whole-agent cleanup (the cascade).
type FailureRecord struct {
	Service   string
	Method    string
	ContactNo int64
	AgentNo   int32
	BusNo     int32
	TenantID  string
	Reason    string
}

// Severity ranks a detected problem.
type Severity int

const (
	SevInfo Severity = iota
	SevWarn
	SevCritical
)

func (s Severity) String() string {
	switch s {
	case SevCritical:
		return "CRITICAL"
	case SevWarn:
		return "WARN"
	default:
		return "INFO"
	}
}

// Detection is what the Detector emits when a signal fires.
type Detection struct {
	Signal      string   // e.g. "cascade-seed", "stuck-in-routing"
	Severity    Severity
	ContactNo   int64
	AgentNo     int32
	Summary     string
	VictimsAtRisk []int64 // healthy contacts that would be wiped by the cascade
	Trigger     *FailureRecord
}

// Diagnosis is the explainable, FSM-grounded output of the Diagnoser.
type Diagnosis struct {
	RootCause         string
	RecommendedAction Action
	Confidence        float64
	Explanation       string
}

// Action is one of EM Sentinel's FSM-guarded healing levers.
type Action string

const (
	ActionNone               Action = "NONE"
	ActionCascadeCircuitBreak Action = "CASCADE_CIRCUIT_BREAK" // contact-only quarantine; preserve agent
	ActionRequeue            Action = "REQUEUE_CONTACT"
	ActionSync               Action = "SYNC_CONTACT"
	ActionTerminate          Action = "TERMINATE_CONTACT"
)
