package sentinel

// HealResult records what a healing action actually did, for the audit feed / UI.
type HealResult struct {
	Action         Action
	DryRun         bool
	Quarantined    []int64 // contacts whose state was expired (the genuinely bad ones)
	Preserved      []int64 // healthy contacts saved from the cascade
	AgentPreserved bool
	Message        string
}

// Actuator is the set of FSM-guarded levers Sentinel can pull. The simulator implements
// it against a mock DynamoDB; in production each method produces a protobuf command to the
// owning service's command topic (RequeueContact / SyncContactV2 / TerminateContact) or
// signals the failure-queue to use contact-only cleanup scope.
type Actuator interface {
	// CascadeCircuitBreak: contact-only quarantine — expire just the seed, preserve agent + victims.
	CascadeCircuitBreak(seedContact int64, agentNo int32) HealResult
	// WholeAgentCleanup: the current (dangerous) failure-queue default — expire agent + all contacts.
	WholeAgentCleanup(seedContact int64, agentNo int32) HealResult
	Requeue(contactNo int64) HealResult
	Sync(contactNo int64) HealResult
	Terminate(contactNo int64) HealResult
}

// Healer executes a Diagnosis through the Actuator, honoring dry-run and a confidence gate.
type Healer struct {
	act       Actuator
	DryRun    bool
	AutoBelow float64 // actions with confidence below this require human approval (when not dry-run)
}

func NewHealer(act Actuator) *Healer {
	return &Healer{act: act, DryRun: false, AutoBelow: 0.75}
}

// Apply runs the recommended action for a detection+diagnosis and returns the result.
func (h *Healer) Apply(d Detection, dx Diagnosis) HealResult {
	if h.DryRun {
		return HealResult{
			Action:  dx.RecommendedAction,
			DryRun:  true,
			Message: "DRY-RUN: would " + string(dx.RecommendedAction),
		}
	}
	if dx.Confidence < h.AutoBelow {
		return HealResult{
			Action:  ActionNone,
			Message: "Held for human approval (confidence below auto threshold).",
		}
	}
	switch dx.RecommendedAction {
	case ActionCascadeCircuitBreak:
		return h.act.CascadeCircuitBreak(d.ContactNo, d.AgentNo)
	case ActionRequeue:
		return h.act.Requeue(d.ContactNo)
	case ActionSync:
		return h.act.Sync(d.ContactNo)
	case ActionTerminate:
		return h.act.Terminate(d.ContactNo)
	default:
		return HealResult{Action: ActionNone, Message: "no action"}
	}
}
