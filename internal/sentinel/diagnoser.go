package sentinel

import "fmt"

// Diagnoser produces an explainable, FSM-grounded diagnosis and a recommended action.
//
// In production this calls Claude (latest Opus/Sonnet) with the cross-service timeline
// AND the FSM rules as context, so the explanation is verifiable rather than hallucinated.
// The LLM only *explains and ranks* — the recommended Action is always re-checked against
// the FSM before the Healer executes it. The interface below is the seam where the real
// model call drops in.
type Diagnoser interface {
	Diagnose(d Detection) Diagnosis
}

// RuleDiagnoser is the offline, deterministic implementation used for the demo. It
// returns the same structured shape a model would, so swapping in a real LLM call is
// a one-line change in main.go.
type RuleDiagnoser struct{}

func NewRuleDiagnoser() *RuleDiagnoser { return &RuleDiagnoser{} }

func (RuleDiagnoser) Diagnose(d Detection) Diagnosis {
	switch d.Signal {
	case "cascade-seed":
		n := len(d.VictimsAtRisk)
		// Confidence scales with blast radius: the more healthy contacts on the agent,
		// the more certain we are that whole-agent cleanup is the wrong scope.
		conf := 0.80 + 0.03*float64(n)
		if conf > 0.98 {
			conf = 0.98
		}
		expl := fmt.Sprintf(
			"Contact %d failed AssignContact to agent %d. Agent %d is currently handling %d other "+
				"healthy contact(s): %v. The failure-queue cleanup scope is the whole agent, so a 1s "+
				"DynamoDB TTL would be set on the agent record and ALL %d healthy contacts — turning "+
				"1 real failure into %d. The FSM shows those contacts are in valid working states; "+
				"only contact %d is failing. Correct scope is contact-only quarantine.",
			d.ContactNo, d.AgentNo, d.AgentNo, n, d.VictimsAtRisk, n, n+1, d.ContactNo)
		return Diagnosis{
			RootCause:         "Failure-queue whole-agent cleanup over-scopes a single AssignContact failure.",
			RecommendedAction: ActionCascadeCircuitBreak,
			Confidence:        conf,
			Explanation:       expl,
		}
	case "cascade-burst":
		// Aggregate signal from live failure-queue logs: many whole-agent wipes in a window.
		n := len(d.VictimsAtRisk)
		conf := 0.82 + 0.02*float64(n)
		if conf > 0.97 {
			conf = 0.97
		}
		return Diagnosis{
			RootCause:         "Failure-queue whole-agent cleanup is firing in bursts — each wipe takes an agent's healthy contacts with it (the cascade).",
			RecommendedAction: ActionCascadeCircuitBreak,
			Confidence:        conf,
			Explanation: fmt.Sprintf("Observed %d+ whole-agent TTL wipes in the window (sample agents %v). Each is an "+
				"AssignContact failure escalated to whole-agent scope. Recommended: engage the cascade circuit breaker "+
				"(contact-only quarantine in failure-queue) and raise an incident for the cell.", n, d.VictimsAtRisk),
		}
	case "stuck-in-routing":
		return Diagnosis{
			RootCause:         "Ring timeout likely lost; contact never left ROUTING.",
			RecommendedAction: ActionRequeue,
			Confidence:        0.85,
			Explanation: fmt.Sprintf("Contact %d is past the ring timeout in ROUTING. Safe lever: "+
				"RequeueContact (ROUTING -> QUEUING, CONTACT_REFUSED) — FSM-validated.", d.ContactNo),
		}
	case "stuck-in-queuing":
		return Diagnosis{
			RootCause:         "Match never produced; possible FindMatch/Match Processor backlog.",
			RecommendedAction: ActionSync,
			Confidence:        0.7,
			Explanation: fmt.Sprintf("Contact %d exceeded match SLA in QUEUING. Lever: SyncContactV2 "+
				"to refresh state and re-trigger matching.", d.ContactNo),
		}
	case "acw-stuck":
		return Diagnosis{
			RootCause:         "ACW timer never fired; agent contact stuck in AFTER_CONTACT_WORK, blocking the agent.",
			RecommendedAction: ActionTerminate,
			Confidence:        0.8,
			Explanation: fmt.Sprintf("Agent contact %d is past the ACW timeout, so agent %d can't be offered new "+
				"contacts. Safe lever: TerminateContact to release the dead agent-contact and free the agent.",
				d.ContactNo, d.AgentNo),
		}
	default:
		return Diagnosis{RecommendedAction: ActionNone, Confidence: 0}
	}
}
