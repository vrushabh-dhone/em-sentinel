package sentinel

import "time"

// Detector turns raw signals into Detections. The cascade-seed rule mirrors the
// signatures in .claude/skills/investigator/cascade-patterns/*.yml:
//
//   - failure-queue "Agent record ttl set" + an SQS AssignContact failure carrying an AgentNo
//     => the whole-agent cleanup is about to fire => every other healthy contact is a victim.
type Detector struct {
	tracker *Tracker

	// dwell thresholds (configurable per skill in production)
	RoutingTimeout time.Duration
	QueuingSLA     time.Duration
}

func NewDetector(t *Tracker) *Detector {
	return &Detector{
		tracker:        t,
		RoutingTimeout: 60 * time.Second,
		QueuingSLA:     300 * time.Second,
	}
}

// InspectFailure is called the moment a FailureRecord destined for the failure-queue
// is observed. If it carries an AgentNo, Sentinel computes the blast radius.
func (d *Detector) InspectFailure(f FailureRecord) (Detection, bool) {
	if f.AgentNo == 0 {
		return Detection{}, false
	}
	victims := d.tracker.HealthyContactsForAgent(f.AgentNo, f.ContactNo)
	sev := SevWarn
	if len(victims) > 0 {
		sev = SevCritical
	}
	rec := f // copy
	return Detection{
		Signal:        "cascade-seed",
		Severity:      sev,
		ContactNo:     f.ContactNo,
		AgentNo:       f.AgentNo,
		VictimsAtRisk: victims,
		Trigger:       &rec,
		Summary: "AssignContact failure on contact (seed). The failure-queue is about to " +
			"wipe the whole agent record, which would cascade to healthy contacts.",
	}, true
}

// InspectDwell scans for contacts stuck in a state past threshold (stuck-in-ROUTING etc.).
func (d *Detector) InspectDwell(now time.Time) []Detection {
	var out []Detection
	for _, cv := range d.tracker.DwellExceeded(StateRouting, d.RoutingTimeout, now) {
		out = append(out, Detection{
			Signal:    "stuck-in-routing",
			Severity:  SevWarn,
			ContactNo: cv.ContactNo,
			AgentNo:   cv.AgentNo,
			Summary:   "Contact has been in ROUTING beyond the ring timeout with no state change.",
		})
	}
	for _, cv := range d.tracker.DwellExceeded(StateQueuing, d.QueuingSLA, now) {
		out = append(out, Detection{
			Signal:    "stuck-in-queuing",
			Severity:  SevWarn,
			ContactNo: cv.ContactNo,
			AgentNo:   cv.AgentNo,
			Summary:   "Contact has been in QUEUING beyond the match SLA while agents are available.",
		})
	}
	return out
}
