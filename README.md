# EM Sentinel — Self-Healing Reliability Agent (Sparkathon 2026 prototype)

An always-on AI agent for Entity Management that **detects** a contact/agent failure on the
live event stream, **diagnoses** the root cause using the FSM as ground truth, and **heals**
it with a safe, FSM-guarded action — before it impacts customers.

This prototype demonstrates the **flagship scenario: the Cascade Circuit Breaker**, which stops
the documented *failure-queue cascade* (a single bad `AssignContact` amplifying 5–7× into a
whole-agent wipe — Schwab: 13 → 77 failures; Disney: 1,806 seed failures).

## Run it (zero dependencies, no build step)

```bash
cd em-sentinel
go run .
# open http://localhost:8080
```

Requires Go 1.22+ (developed on 1.26). No npm, no Docker, no AWS — the web UI is embedded in
the binary and the EM runtime is simulated in-memory.

Custom port: `go run . -addr :9000`

## What you'll see

1. **Agent #42** handling 6 contacts. Contact **#1001** fails `AssignContact`.
2. **▶ Run WITHOUT Sentinel** — the failure-queue does its current whole-agent cleanup:
   all 6 contacts + the agent record get a 1-second TTL. **1 real failure → 6 wipes (6×).** 💥
3. **🛡 Run WITH Sentinel** — Sentinel detects the seed, computes the blast radius (5 healthy
   victims), the AI explains the over-scoping with 95% confidence, and the **circuit breaker**
   applies a *contact-only* quarantine. **1 real failure → 1 wipe; 5 contacts saved.** ✓

## How it maps to real Entity Management

This is a faithful skeleton — swap the `internal/sim` package for a real `orch-entity-streams`
consumer + DynamoDB and the `internal/sentinel` engine is unchanged.

| Prototype | Real EM |
|-----------|---------|
| `internal/sentinel/model.go` types | `ContactStateChangeV2` / `AgentContactStateChangeV2` (orch-entity-event-contracts) |
| `Tracker` (in-mem FSM map) | fed by `orch-entity-streams` `GenericHandler` |
| `Detector` cascade-seed rule | `.claude/skills/investigator/cascade-patterns/*.yml` (log marker `"Agent record ttl set"`, `entityoperations.go:76`) |
| `Diagnoser` (`RuleDiagnoser`) | a Claude call with the timeline + FSM rules as context; the `Diagnoser` interface is the seam |
| `FailureQueue.WholeAgentCleanup` | `orch-entity-failure-queue` `recordprocessor.go:54-91`, `entityoperations.go:61-99` |
| `FailureQueue.CascadeCircuitBreak` | proposed contact-only quarantine path |
| `Actuator` levers (Requeue/Sync/Terminate) | protobuf commands to Contact/Agent **Command** topics (FSM-validated) |

## Architecture

```
 (sim) event stream ─► Tracker ─► Detector ─► Diagnoser (LLM seam) ─► Healer ─► Actuator
                                     │            │                      │         │
                                cascade-seed  FSM-grounded RCA      dry-run|auto   FSM-guarded
                                + dwell rules  + confidence         + conf gate    levers
```

- **Default posture is safe:** `Healer.DryRun` proposes without acting; an `AutoBelow`
  confidence gate holds low-confidence actions for human approval. Flip to auto for the demo.
- Levers never write state directly — they go through existing FSM-guarded mechanisms.

## Layout

```
em-sentinel/
├── main.go                     # HTTP server + SSE demo orchestration
├── web/                        # embedded dashboard (index.html, styles.css, app.js)
└── internal/
    ├── sentinel/               # the engine (model, tracker, detector, diagnoser, healer)
    └── sim/                    # in-memory EM runtime (store=mock DynamoDB, failure-queue, scenario)
```

## Live mode — connect to ic-dev (read-only, verified)

The **Live (ic-dev)** tab connects Sentinel to the **real** Entity Management dev
environment by tapping the genuine cascade-seed signal in **CloudWatch**: the
`orch-entity-failure-queue` Lambda's `"Agent record ttl set"` log
(`persistence/entityoperations.go:76`), each carrying the `agentNo` whose whole record was
wiped. Strictly **read-only** (CloudWatch Logs `FilterLogEvents`); the Healer runs **dry-run**
and never writes to the shared environment.

What it does each poll:
1. `FilterLogEvents` on `/aws/lambda/orch-entity-failure-queue` for `"Agent record ttl set"` over the lookback window.
2. Parse `agentNo` + timestamp; aggregate into a burst (total wipes, distinct agents, peak/min).
3. If wipes ≥ threshold → build a `cascade-burst` Detection → Diagnose → **dry-run** Heal → stream to the dashboard.

The Live tab has two modes:
- **⟳ Scan 24h** — retrospective sweep (default on tab open); surfaces the cascade history.
- **● Watch live (15m)** — continuous 30s polling of the last 15 min for real-time burst alerts.

(Both just set `?lookback=` on `/api/live`; `15m`/`24h`/etc. any Go duration.)

Verified against real ic-dev: the 24h scan found **55 whole-agent wipes / 47 distinct agents /
peak 9-10/min** (cascade live and ongoing, ~1,000 wipes/week); a 15m watch showed **0 wipes —
healthy** (no burst in the window).

Config via environment variables (defaults target ic-dev; no secrets in code):

```bash
export EM_AWS_PROFILE="aws-session"     # default; the MFA session profile (run ~/scripts/mcp-awslab-creds.sh)
export EM_AWS_REGION="us-west-2"        # default
export EM_FQ_LOG_GROUP="/aws/lambda/orch-entity-failure-queue"   # default
export EM_CW_LOOKBACK="24h"             # default; use e.g. 15m for a live watch
go run .   # then open the "Live (ic-dev)" tab
```

Auth uses the AWS shared-config profile (`aws-session`), validated lazily — if creds are
missing/expired the tab shows a clear error pointing at `~/scripts/mcp-awslab-creds.sh`.

> Production shape: run Sentinel in-cluster with an IAM role that has `logs:FilterLogEvents`
> on the failure-queue group (no MFA profile needed). Remediation writes (the actual
> contact-only quarantine / circuit breaker) stay dry-run until run in a cell you own.

### Where the EM signals live (discovered via MCP)

| Source | EM cascade signal? |
|--------|--------------------|
| mon-na1 Loki | ❌ monitoring-stack self-logs only |
| mon-na1 Mimir (metrics) | ⚠️ aggregate failure rates per cell (`em_incoming_contact_failure_total`, `dlq_failure_count_total`) — symptoms, not the seed |
| **CloudWatch `/aws/lambda/orch-entity-failure-queue`** | ✅ the real seed (`"Agent record ttl set"` + agentNo) |

## During the 48h hackathon

The simulation is the safe, always-works stage demo. Stretch goals, in order:
1. Point the `Tracker` at a real test-env Kafka consumer (copy `orch-entity-timer` bootstrap).
2. Replace `RuleDiagnoser` with a real Claude call (the interface is ready).
3. Add the other signals (stuck-in-ROUTING, ACW-never-released) — the `Detector` already stubs them.
