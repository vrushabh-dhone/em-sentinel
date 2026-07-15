package sentinel

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

// ClaudeDiagnoser is the real, LLM-backed Diagnoser. It sends the detection plus the
// EM FSM rules to Claude (Opus 4.8) and asks for a root cause + recommended action.
// The FSM is passed as ground truth so the explanation is verifiable, and the action
// Claude returns is re-validated against the allowed lever set before use.
//
// It degrades gracefully: if ANTHROPIC_API_KEY is unset or the call fails, it falls back
// to the offline RuleDiagnoser so the demo always runs.
type ClaudeDiagnoser struct {
	client   anthropic.Client
	fallback *RuleDiagnoser
	model    anthropic.Model
}

// NewClaudeDiagnoser returns a ClaudeDiagnoser if ANTHROPIC_API_KEY is set, otherwise nil.
func NewClaudeDiagnoser() *ClaudeDiagnoser {
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		return nil
	}
	return &ClaudeDiagnoser{
		client:   anthropic.NewClient(),
		fallback: NewRuleDiagnoser(),
		model:    anthropic.ModelClaudeOpus4_8,
	}
}

const fsmContext = `Entity Management FSMs (ground truth):
Unified Contact states: CREATED, QUEUING, REFINING, ROUTING, PREVIEWING, CONNECTING, WITH_AGENT, IDLE, ENDED.
Agent Contact states: CREATED, PREVIEW, CONNECTING, CONTACT_WORK, AFTER_CONTACT_WORK, ENDED.
Failure-queue cascade: a failed AssignContact routes a failure record whose AgentNo triggers a WHOLE-AGENT cleanup — a 1s DynamoDB TTL on the agent record AND every contact that agent handles, turning 1 real failure into N wipes (5-7x amplification observed in audits). The healthy contacts are "victims".
Safe FSM-guarded levers (you must choose exactly one):
- CASCADE_CIRCUIT_BREAK: contact-only quarantine — expire only the seed contact, preserve the agent + victims. Use for cascade-seed.
- REQUEUE_CONTACT: ROUTING -> QUEUING (CONTACT_REFUSED). Use for stuck-in-routing.
- SYNC_CONTACT: refresh state from DynamoDB. Use for queue/version issues.
- TERMINATE_CONTACT: end a genuinely dead contact. Last resort.
- NONE: take no action.`

func (d *ClaudeDiagnoser) Diagnose(det Detection) Diagnosis {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	prompt := fmt.Sprintf(`%s

A reliability signal fired on the live event stream:
- signal: %s
- severity: %s
- contactNo: %d
- agentNo: %d
- healthy victims at risk: %v
- summary: %s

Respond with ONLY a JSON object (no prose, no markdown fences):
{"rootCause": "<one sentence>", "action": "<one of CASCADE_CIRCUIT_BREAK|REQUEUE_CONTACT|SYNC_CONTACT|TERMINATE_CONTACT|NONE>", "confidence": <0..1>, "explanation": "<2-3 sentences grounded in the FSM and the blast radius>"}`,
		fsmContext, det.Signal, det.Severity, det.ContactNo, det.AgentNo, det.VictimsAtRisk, det.Summary)

	resp, err := d.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     d.model,
		MaxTokens: 1024,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)),
		},
	})
	if err != nil {
		return d.fallback.Diagnose(det)
	}

	var text strings.Builder
	for _, block := range resp.Content {
		if b, ok := block.AsAny().(anthropic.TextBlock); ok {
			text.WriteString(b.Text)
		}
	}

	dx, ok := parseDiagnosis(text.String())
	if !ok {
		return d.fallback.Diagnose(det)
	}
	// Re-validate the action against the allowed lever set; if Claude returned
	// something unexpected, trust the deterministic rule recommendation instead.
	if !validAction(dx.RecommendedAction) {
		dx.RecommendedAction = d.fallback.Diagnose(det).RecommendedAction
	}
	return dx
}

func parseDiagnosis(s string) (Diagnosis, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return Diagnosis{}, false
	}
	var raw struct {
		RootCause   string  `json:"rootCause"`
		Action      string  `json:"action"`
		Confidence  float64 `json:"confidence"`
		Explanation string  `json:"explanation"`
	}
	if err := json.Unmarshal([]byte(s[start:end+1]), &raw); err != nil {
		return Diagnosis{}, false
	}
	return Diagnosis{
		RootCause:         raw.RootCause,
		RecommendedAction: Action(raw.Action),
		Confidence:        raw.Confidence,
		Explanation:       raw.Explanation,
	}, true
}

func validAction(a Action) bool {
	switch a {
	case ActionCascadeCircuitBreak, ActionRequeue, ActionSync, ActionTerminate, ActionNone:
		return true
	}
	return false
}
