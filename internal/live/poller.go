package live

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/nice-cxone/em-sentinel/internal/sentinel"
)

// Emit pushes a UI event (same shape the dashboard already understands).
type Emit func(event string, data map[string]any)

// noopActuator satisfies sentinel.Actuator; never invoked because the live Healer runs
// dry-run (Apply returns before touching the actuator). Live mode is strictly READ-ONLY.
type noopActuator struct{}

func (noopActuator) CascadeCircuitBreak(int64, int32) sentinel.HealResult { return sentinel.HealResult{} }
func (noopActuator) WholeAgentCleanup(int64, int32) sentinel.HealResult   { return sentinel.HealResult{} }
func (noopActuator) Requeue(int64) sentinel.HealResult                    { return sentinel.HealResult{} }
func (noopActuator) Sync(int64) sentinel.HealResult                       { return sentinel.HealResult{} }
func (noopActuator) Terminate(int64) sentinel.HealResult                  { return sentinel.HealResult{} }

// Poller watches ic-dev CloudWatch for failure-queue cascade bursts and drives the engine.
type Poller struct {
	client    *Client
	diagnoser sentinel.Diagnoser
	healer    *sentinel.Healer
	engine    string
	victims   *VictimClient // optional: real cascade victims from mon-na1 Loki (nil if unconfigured)
	events    *EventClient  // optional: real FSM dwell from mon-na1 Loki (nil if unconfigured)
	Signal    string        // "cascade" (default) | "stuck" | "queue" | "acw"
}

func NewPoller(c *Client, diag sentinel.Diagnoser, engine string) *Poller {
	h := sentinel.NewHealer(noopActuator{})
	h.DryRun = true
	return &Poller{client: c, diagnoser: diag, healer: h, engine: engine,
		victims: NewVictimClient(), events: NewEventClient(), Signal: "cascade"}
}

// dwellSpec maps a live dwell signal to its event method, state regex, target state, threshold.
type dwellSpec struct {
	method     string
	stateRe    *regexp.Regexp
	target     string // state suffix in the log enum
	thresholdS int
	engineSig  string // Detection.Signal so the shared diagnoser picks the right lever
	label      string
}

var dwellSpecs = map[string]dwellSpec{
	"stuck": {"ContactStateChangeV2", ReContactState, "ROUTING", 60, "stuck-in-routing", "Stuck-in-ROUTING"},
	"queue": {"ContactStateChangeV2", ReContactState, "QUEUING", 300, "stuck-in-queuing", "Queue Stuck"},
	"acw":   {"AgentContactStateChangeV2", ReAgentContactState, "AFTER_CONTACT_WORK", 30, "acw-stuck", "ACW Stuck"},
}

func logLine(tag, msg string) map[string]any {
	return map[string]any{"ts": time.Now().Format("15:04:05.000"), "tag": tag, "msg": msg}
}

// Run polls until ctx is cancelled.
func (p *Poller) Run(ctx context.Context, emit Emit) {
	p.emitStatus(emit)
	tick := time.NewTicker(p.client.cfg.Interval)
	defer tick.Stop()
	for {
		p.poll(ctx, emit)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// RunOnce does a single poll then returns (for headless screenshots / one-shot checks).
func (p *Poller) RunOnce(ctx context.Context, emit Emit) {
	p.emitStatus(emit)
	p.poll(ctx, emit)
}

func (p *Poller) emitStatus(emit Emit) {
	src := p.client.LogGroup() // cascade: CloudWatch failure-queue
	if spec, ok := dwellSpecs[p.Signal]; ok && p.events != nil {
		src = fmt.Sprintf("mon-na1 Loki entitymanagement-%s (%s)", p.events.Cell(), spec.method)
	}
	emit("status", map[string]any{
		"state":  "connected",
		"text":   fmt.Sprintf("Watching %s (last %s, read-only, dry-run)", src, p.client.Lookback()),
		"engine": p.engine,
	})
}

// poll dispatches to the cascade (CloudWatch) or a dwell (Loki) detector per Signal.
func (p *Poller) poll(ctx context.Context, emit Emit) {
	if spec, ok := dwellSpecs[p.Signal]; ok {
		p.pollDwell(ctx, emit, spec)
		return
	}
	p.pollCascade(ctx, emit)
}

// pollDwell reconstructs FSM dwell from the real EM event stream and flags stuck contacts —
// the live mirror of the Simulation stuck/queue/ACW scenarios, using the SAME diagnoser/healer.
func (p *Poller) pollDwell(ctx context.Context, emit Emit, spec dwellSpec) {
	if p.events == nil {
		emit("status", map[string]any{"state": "not-configured",
			"text": "Live dwell needs mon-na1 Loki access — set GRAFANA_URL + GRAFANA_TOKEN (in sentinel.env)."})
		return
	}
	now := time.Now()
	stuck, total, distinct, err := p.events.Dwell(ctx, p.client.cfg.Lookback, now, spec.method, spec.stateRe, spec.target, spec.thresholdS)
	if err != nil {
		emit("status", map[string]any{"state": "error", "text": "Loki dwell query failed: " + err.Error()})
		return
	}
	emit("log", logLine("LOKI", fmt.Sprintf(
		"%s scan: %d events, %d distinct contacts in last %s; %d in %s past %ds",
		spec.method, total, distinct, p.client.Lookback(), len(stuck), spec.target, spec.thresholdS)))

	if len(stuck) == 0 {
		emit("detection", map[string]any{"healthy": true, "total": 0, "agents": []int64{}})
		return
	}

	sample := make([]int64, 0, 6)
	for _, s := range stuck {
		if len(sample) < 6 {
			sample = append(sample, s.ContactNo)
		}
	}
	det := sentinel.Detection{
		Signal:    spec.engineSig,
		Severity:  sentinel.SevWarn,
		ContactNo: stuck[0].ContactNo,
		Summary:   fmt.Sprintf("%d contact(s) dwelling in %s past %ds in entitymanagement-%s.", len(stuck), spec.target, spec.thresholdS, p.events.Cell()),
	}
	emit("log", logLine("DETECT", fmt.Sprintf(
		"⚡ %s DETECTED [WARN] — %d contact(s) stuck in %s (oldest %ds). Sample contactNos: %v",
		spec.label, len(stuck), spec.target, oldestAge(stuck), sample)))

	diag := p.diagnoser.Diagnose(det)
	emit("diagnosis", map[string]any{
		"engine": p.engine, "rootCause": diag.RootCause, "action": diag.RecommendedAction,
		"confidence": diag.Confidence, "explanation": diag.Explanation,
	})
	res := p.healer.Apply(det, diag)
	emit("log", logLine("HEAL", "🛡  "+res.Message+" (dry-run — read-only against shared ic-dev)"))

	emit("detection", map[string]any{
		"healthy": false, "total": len(stuck), "distinctAgents": distinct,
		"peakPerMin": 0, "agents": sample, "action": diag.RecommendedAction, "victimContacts": -1,
	})
}

func oldestAge(stuck []Stuck) int {
	max := 0
	for _, s := range stuck {
		if s.AgeSec > max {
			max = s.AgeSec
		}
	}
	return max
}

func (p *Poller) pollCascade(ctx context.Context, emit Emit) {
	now := time.Now()
	seeds, err := p.client.Seeds(ctx, now)
	if err != nil {
		emit("status", map[string]any{
			"state": "error",
			"text":  "CloudWatch query failed (creds? run ~/scripts/mcp-awslab-creds.sh): " + err.Error(),
		})
		return
	}

	b := Summarize(seeds)
	emit("log", logLine("CWLOGS", fmt.Sprintf(
		"failure-queue scan: %d whole-agent wipes, %d distinct agents, peak %d/min in last %s",
		b.Total, b.DistinctAgents, b.PeakPerMin, p.client.Lookback())))

	if b.Total < p.client.cfg.Threshold {
		emit("detection", map[string]any{"healthy": true, "total": b.Total, "agents": b.SampleAgents})
		return
	}

	severity := sentinel.SevWarn
	if b.PeakPerMin >= p.client.cfg.BurstPerMin {
		severity = sentinel.SevCritical
	}
	det := sentinel.Detection{
		Signal:        "cascade-burst",
		Severity:      severity,
		VictimsAtRisk: b.SampleAgents, // wiped agents (each takes its contacts with it)
		Summary: fmt.Sprintf("%d whole-agent cleanups observed (peak %d/min at %s) — failure-queue cascade.",
			b.Total, b.PeakPerMin, b.PeakMinute.Format("2006-01-02 15:04Z")),
	}

	emit("log", logLine("DETECT", fmt.Sprintf(
		"⚡ CASCADE BURST DETECTED [%s] — %d agents wiped, peak %d/min. Sample agentNos: %v",
		severity, b.DistinctAgents, b.PeakPerMin, b.SampleAgents)))

	// Correlate the OTHER half of the cascade — real victim contacts from mon-na1 Loki.
	victimContacts := -1
	if p.victims != nil {
		vc, va, verr := p.victims.Victims(ctx, p.client.cfg.Lookback, now)
		if verr != nil {
			emit("log", logLine("VICTIMS", "Loki victim lookup failed (check GRAFANA_URL/token): "+verr.Error()))
		} else {
			victimContacts = len(vc)
			det.VictimsAtRisk = vc
			det.Summary = fmt.Sprintf("%d whole-agent cleanups → %d victim contacts hit RECORD_NOT_FOUND across %d agents (real ic-dev cascade).",
				b.Total, len(vc), len(va))
			emit("log", logLine("VICTIMS", fmt.Sprintf(
				"🩸 %d victim contacts lost (RECORD_NOT_FOUND) across %d agents — the cascade's blast radius (mon-na1 Loki)",
				len(vc), len(va))))
		}
	}

	diag := p.diagnoser.Diagnose(det)
	emit("diagnosis", map[string]any{
		"engine": p.engine, "rootCause": diag.RootCause, "action": diag.RecommendedAction,
		"confidence": diag.Confidence, "explanation": diag.Explanation,
	})

	res := p.healer.Apply(det, diag)
	emit("log", logLine("HEAL", "🛡  "+res.Message+" (dry-run — read-only against shared ic-dev)"))

	emit("detection", map[string]any{
		"healthy": false, "total": b.Total, "distinctAgents": b.DistinctAgents,
		"peakPerMin": b.PeakPerMin, "agents": b.SampleAgents, "action": diag.RecommendedAction,
		"victimContacts": victimContacts,
	})
}
