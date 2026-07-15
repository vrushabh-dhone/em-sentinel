// Command em-sentinel runs the EM Sentinel demo: a self-healing reliability agent for
// Entity Management, with an embedded web dashboard. No external dependencies to run the
// demo offline — `go run .` and open http://localhost:8080. If ANTHROPIC_API_KEY is set,
// the diagnose step uses Claude (Opus 4.8); otherwise it uses the offline rule engine.
package main

import (
	"bufio"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/nice-cxone/em-sentinel/internal/live"
	"github.com/nice-cxone/em-sentinel/internal/sentinel"
	"github.com/nice-cxone/em-sentinel/internal/sim"
)

//go:embed web/*
var webFS embed.FS

// diagnoser is selected once at startup: Claude if a key is present, else the rule engine.
var (
	diagnoser   sentinel.Diagnoser
	diagEngine  string
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	flag.Parse()

	loadEnvFile("sentinel.env") // local-only creds for live mode (gitignored)

	if cd := sentinel.NewClaudeDiagnoser(); cd != nil {
		diagnoser, diagEngine = cd, "claude (opus-4-8)"
	} else {
		diagnoser, diagEngine = sentinel.NewRuleDiagnoser(), "rules (offline)"
	}

	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(sub)))
	mux.HandleFunc("/api/run", runHandler)
	mux.HandleFunc("/api/live", liveHandler)

	log.Printf("EM Sentinel dashboard → http://localhost%s  (diagnoser: %s)", *addr, diagEngine)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

// loadEnvFile reads KEY=VALUE lines from a local config file and sets any env var not
// already present in the environment (real env vars win). Used for local-only live creds.
func loadEnvFile(path string) {
	f, err := os.Open(path)
	if err != nil {
		return // no config file — fine, rely on real env vars
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if _, exists := os.LookupEnv(k); !exists {
			os.Setenv(k, v)
		}
	}
}

func sse(w http.ResponseWriter, fl http.Flusher, event string, payload any) {
	b, _ := json.Marshal(payload)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
	fl.Flush()
}

func logLine(tag, msg string) map[string]any {
	return map[string]any{"ts": time.Now().Format("15:04:05.000"), "tag": tag, "msg": msg}
}

func runHandler(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	sentinelOn := r.URL.Query().Get("mode") == "on"
	step := makeStep(r.URL.Query().Get("fast") == "1")
	switch r.URL.Query().Get("scenario") {
	case "stuck":
		runStuck(w, fl, sentinelOn, step)
	case "acw":
		runACW(w, fl, sentinelOn, step)
	case "queue":
		runQueue(w, fl, sentinelOn, step)
	default:
		runCascade(w, fl, sentinelOn, step)
	}
}

// makeStep returns the pacing function. Fast mode (for headless screenshots) collapses
// the animation delays so a run completes near-instantly.
func makeStep(fast bool) func() {
	d := 650 * time.Millisecond
	if fast {
		d = 15 * time.Millisecond
	}
	return func() { time.Sleep(d) }
}

// emitDiagnosis runs the selected diagnoser and streams the result, tagging which engine ran.
func emitDiagnosis(w http.ResponseWriter, fl http.Flusher, det sentinel.Detection) sentinel.Diagnosis {
	diag := diagnoser.Diagnose(det)
	sse(w, fl, "diagnosis", map[string]any{
		"engine":      diagEngine,
		"rootCause":   diag.RootCause,
		"action":      diag.RecommendedAction,
		"confidence":  diag.Confidence,
		"explanation": diag.Explanation,
	})
	return diag
}

// ---- Scenario 1: failure-queue cascade ----

func runCascade(w http.ResponseWriter, fl http.Flusher, sentinelOn bool, step func()) {
	store, tracker, sc := sim.CascadeFixture()
	fq := sim.NewFailureQueue(store, tracker)

	sse(w, fl, "scene", scene(sentinelOn, sc.AgentNo, store))
	step()
	sse(w, fl, "log", logLine("FAILURE", fmt.Sprintf(
		"Contact %d failed AssignContact to agent %d — %q. Failure record routed to failure-queue (SQS).",
		sc.SeedContact, sc.AgentNo, sc.Seed.Reason)))
	step()

	var result sentinel.HealResult
	if sentinelOn {
		det := sentinel.NewDetector(tracker)
		healer := sentinel.NewHealer(fq)
		if detection, fired := det.InspectFailure(sc.Seed); fired {
			sse(w, fl, "log", logLine("DETECT", fmt.Sprintf(
				"⚡ CASCADE SEED DETECTED [%s] — %d healthy contact(s) at risk: %v",
				detection.Severity, len(detection.VictimsAtRisk), detection.VictimsAtRisk)))
			step()
			diag := emitDiagnosis(w, fl, detection)
			step()
			result = healer.Apply(detection, diag)
			sse(w, fl, "log", logLine("HEAL", "🛡  "+result.Message))
		}
	} else {
		sse(w, fl, "log", logLine("FQUEUE", "failure-queue Lambda: getRelatedRecords → whole-agent cleanup scope"))
		step()
		sse(w, fl, "log", logLine("FQUEUE", fmt.Sprintf("\"Agent record ttl set\" agentNo=%d (entityoperations.go:76)", sc.AgentNo)))
		result = fq.WholeAgentCleanup(sc.SeedContact, sc.AgentNo)
		sse(w, fl, "log", logLine("FQUEUE", result.Message))
	}
	step()

	store.ExpireDue()
	sse(w, fl, "result", map[string]any{"quarantined": result.Quarantined, "preserved": result.Preserved})
	sse(w, fl, "scene", scene(sentinelOn, sc.AgentNo, store))
	step()

	wiped := countWipedContacts(store.Snapshot())
	saved := 0
	if sentinelOn {
		saved = len(sc.AllContacts) - wiped
	}
	amp := float64(wiped)
	verdict := map[string]any{"good": false, "text": fmt.Sprintf(
		"💥 Cascade — 1 real failure wiped %d contacts + the agent record.", wiped)}
	if sentinelOn {
		verdict = map[string]any{"good": true, "text": fmt.Sprintf(
			"🛡  Cascade stopped — %d healthy contacts saved. 1 real failure = 1 wipe.", saved)}
	}
	sse(w, fl, "summary", map[string]any{
		"verdict": verdict,
		"stats": []map[string]any{
			{"num": "1", "label": "Real failures", "kind": ""},
			{"num": fmt.Sprintf("%d", wiped), "label": "Contacts wiped", "kind": "danger"},
			{"num": fmt.Sprintf("%d", saved), "label": "Contacts saved", "kind": "safe"},
			{"num": fmt.Sprintf("%.0f×", amp), "label": "Amplification", "kind": "amp"},
		},
	})
}

// ---- Scenario 2: stuck contact in ROUTING ----

func runStuck(w http.ResponseWriter, fl http.Flusher, sentinelOn bool, step func()) {
	store, tracker, sc := sim.StuckFixture()
	fq := sim.NewFailureQueue(store, tracker)
	now := time.Now()

	sse(w, fl, "scene", scene(sentinelOn, sc.AgentNo, store))
	step()
	sse(w, fl, "log", logLine("FAILURE", fmt.Sprintf(
		"Contact %d has been in ROUTING ~92s (ring timeout 60s) — no agent picked up; customer waiting.",
		sc.SeedContact)))
	step()

	recovered := false
	if sentinelOn {
		det := sentinel.NewDetector(tracker)
		healer := sentinel.NewHealer(fq)
		dets := det.InspectDwell(now)
		if len(dets) > 0 {
			detection := dets[0]
			sse(w, fl, "log", logLine("DETECT", fmt.Sprintf(
				"⚡ STUCK CONTACT DETECTED [%s] — contact %d in ROUTING past ring timeout",
				detection.Severity, detection.ContactNo)))
			step()
			diag := emitDiagnosis(w, fl, detection)
			step()
			result := healer.Apply(detection, diag)
			sse(w, fl, "log", logLine("HEAL", "🛡  "+result.Message))
			recovered = true
		}
	} else {
		sse(w, fl, "log", logLine("FQUEUE", "no remediation — contact remains stuck in ROUTING; customer will abandon."))
	}
	step()

	sse(w, fl, "scene", scene(sentinelOn, sc.AgentNo, store))
	step()

	verdict := map[string]any{"good": false, "text": fmt.Sprintf(
		"💥 Contact %d stuck in ROUTING — customer abandons; needs manual requeue.", sc.SeedContact)}
	if recovered {
		verdict = map[string]any{"good": true, "text": fmt.Sprintf(
			"🛡  Sentinel requeued contact %d in ~1s — recovered, customer kept.", sc.SeedContact)}
	}
	stuckCount, recoveredCount, wait, mttr := 1, 0, "∞", "manual"
	if recovered {
		recoveredCount, wait, mttr = 1, "~1s", "auto"
	}
	sse(w, fl, "summary", map[string]any{
		"verdict": verdict,
		"stats": []map[string]any{
			{"num": fmt.Sprintf("%d", stuckCount), "label": "Stuck contacts", "kind": "danger"},
			{"num": fmt.Sprintf("%d", recoveredCount), "label": "Auto-recovered", "kind": "safe"},
			{"num": wait, "label": "Customer wait", "kind": "amp"},
			{"num": mttr, "label": "MTTR", "kind": ""},
		},
	})
}

// liveHandler streams real cascade-seed detections from a live ic-dev environment via
// Grafana/Loki. Read-only: the Healer runs in dry-run. If the environment isn't configured,
// it emits a status event telling the user exactly which env vars to set.
func liveHandler(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	emit := func(event string, data map[string]any) { sse(w, fl, event, data) }

	cfg, _ := live.FromEnv()
	// ?lookback=15m|24h overrides the scan window (watch vs retrospective scan).
	if lb := r.URL.Query().Get("lookback"); lb != "" {
		if d, err := time.ParseDuration(lb); err == nil {
			cfg.Lookback = d
		}
	}
	poller := live.NewPoller(live.NewClient(cfg), diagnoser, diagEngine)
	if sig := r.URL.Query().Get("signal"); sig != "" {
		poller.Signal = sig
	}
	if r.URL.Query().Get("once") == "1" {
		poller.RunOnce(r.Context(), emit) // single poll then close (for headless capture)
		return
	}
	poller.Run(r.Context(), emit) // blocks until the client disconnects
}

// runSingleContact is the shared shape for single-contact remediation scenarios
// (stuck-in-routing, ACW-stuck, queue-stuck): scene → problem → [Sentinel: detect/diagnose/heal]
// → scene → summary. The Detection's RecommendedAction (from the diagnoser) selects the lever.
func runSingleContact(w http.ResponseWriter, fl http.Flusher, sentinelOn bool, step func(),
	store *sim.Store, fq *sim.FailureQueue, agentNo int32,
	failureMsg, detectMsg, offMsg, badVerdict, goodVerdict string,
	det sentinel.Detection, stats func(recovered bool) []map[string]any) {

	sse(w, fl, "scene", scene(sentinelOn, agentNo, store))
	step()
	sse(w, fl, "log", logLine("FAILURE", failureMsg))
	step()

	recovered := false
	if sentinelOn {
		healer := sentinel.NewHealer(fq)
		sse(w, fl, "log", logLine("DETECT", detectMsg))
		step()
		diag := emitDiagnosis(w, fl, det)
		step()
		res := healer.Apply(det, diag)
		sse(w, fl, "log", logLine("HEAL", "🛡  "+res.Message))
		recovered = true
	} else {
		sse(w, fl, "log", logLine("FQUEUE", offMsg))
	}
	step()
	sse(w, fl, "scene", scene(sentinelOn, agentNo, store))
	step()

	v := map[string]any{"good": false, "text": badVerdict}
	if recovered {
		v = map[string]any{"good": true, "text": goodVerdict}
	}
	sse(w, fl, "summary", map[string]any{"verdict": v, "stats": stats(recovered)})
}

func runACW(w http.ResponseWriter, fl http.Flusher, sentinelOn bool, step func()) {
	store, tracker, sc := sim.ACWFixture()
	fq := sim.NewFailureQueue(store, tracker)
	det := sentinel.Detection{
		Signal: "acw-stuck", Severity: sentinel.SevWarn, ContactNo: sc.SeedContact, AgentNo: sc.AgentNo,
		Summary: "Agent contact stuck in AFTER_CONTACT_WORK past the ACW timeout.",
	}
	stats := func(recovered bool) []map[string]any {
		freed, wait, mttr := 0, "∞", "manual"
		if recovered {
			freed, wait, mttr = 1, "~1s", "auto"
		}
		return []map[string]any{
			{"num": "1", "label": "Blocked agents", "kind": "danger"},
			{"num": fmt.Sprintf("%d", freed), "label": "Agents freed", "kind": "safe"},
			{"num": wait, "label": "Agent blocked", "kind": "amp"},
			{"num": mttr, "label": "MTTR", "kind": ""},
		}
	}
	runSingleContact(w, fl, sentinelOn, step, store, fq, sc.AgentNo,
		fmt.Sprintf("Contact %d stuck in AFTER_CONTACT_WORK ~120s (ACW timeout 30s) — agent %d blocked from new contacts.", sc.SeedContact, sc.AgentNo),
		fmt.Sprintf("⚡ ACW STUCK DETECTED [WARN] — contact %d in ACW past timeout; agent %d blocked", sc.SeedContact, sc.AgentNo),
		fmt.Sprintf("no remediation — agent %d stays blocked; new contacts can't route to it.", sc.AgentNo),
		fmt.Sprintf("💥 Agent %d blocked in ACW — contact %d never released; agent idle-but-unavailable.", sc.AgentNo, sc.SeedContact),
		fmt.Sprintf("🛡  Sentinel released contact %d (TerminateContact) — agent %d free to take contacts again.", sc.SeedContact, sc.AgentNo),
		det, stats)
}

func runQueue(w http.ResponseWriter, fl http.Flusher, sentinelOn bool, step func()) {
	store, tracker, sc := sim.QueueFixture()
	fq := sim.NewFailureQueue(store, tracker)
	det := sentinel.Detection{
		Signal: "stuck-in-queuing", Severity: sentinel.SevWarn, ContactNo: sc.SeedContact, AgentNo: sc.AgentNo,
		Summary: "Contact stuck in QUEUING past the match SLA while agents are available.",
	}
	stats := func(recovered bool) []map[string]any {
		rec, wait, mttr := 0, "∞", "manual"
		if recovered {
			rec, wait, mttr = 1, "~1s", "auto"
		}
		return []map[string]any{
			{"num": "1", "label": "Stuck contacts", "kind": "danger"},
			{"num": fmt.Sprintf("%d", rec), "label": "Recovered", "kind": "safe"},
			{"num": wait, "label": "Customer wait", "kind": "amp"},
			{"num": mttr, "label": "MTTR", "kind": ""},
		}
	}
	runSingleContact(w, fl, sentinelOn, step, store, fq, sc.AgentNo,
		fmt.Sprintf("Contact %d stuck in QUEUING ~300s (match SLA) while agents available — no match produced.", sc.SeedContact),
		fmt.Sprintf("⚡ STUCK-IN-QUEUE DETECTED [WARN] — contact %d past match SLA, agents available", sc.SeedContact),
		"no remediation — contact waits indefinitely; customer abandons.",
		fmt.Sprintf("💥 Contact %d stuck in QUEUING — match never produced; customer abandons.", sc.SeedContact),
		fmt.Sprintf("🛡  Sentinel re-synced contact %d (SyncContactV2) — re-entered matching, customer kept.", sc.SeedContact),
		det, stats)
}

func scene(sentinelOn bool, agentNo int32, store *sim.Store) map[string]any {
	return map[string]any{"sentinelOn": sentinelOn, "agentNo": agentNo, "records": store.Snapshot()}
}

func countWipedContacts(recs []sim.Record) int {
	n := 0
	for _, r := range recs {
		if r.Wiped && r.Kind == sim.KindContact {
			n++
		}
	}
	return n
}
