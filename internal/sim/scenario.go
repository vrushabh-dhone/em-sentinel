package sim

import (
	"time"

	"github.com/vrushabh-dhone/cx-guardian/internal/engine"
)

// Scenario holds the synthetic cascade fixture: one agent handling several contacts,
// one of which (the seed) is about to fail AssignContact.
type Scenario struct {
	AgentNo      int32
	SeedContact  int64
	AllContacts  []int64
	Seed         engine.FailureRecord
}

// CascadeFixture builds the canonical demo: agent 42 with 6 contacts; contact 1001 is the
// seed failure; 1002-1006 are healthy victims that the whole-agent cleanup would wipe.
// Agent 55 (bystander) is also active with 3 healthy contacts — unaffected by the cascade.
func CascadeFixture() (*Store, *engine.Tracker, Scenario) {
	const agent int32 = 42
	const bystander int32 = 55
	contacts := []int64{1001, 1002, 1003, 1004, 1005, 1006}
	bystanderContacts := []int64{5001, 5002, 5003}
	seed := int64(1001)

	store := NewStore()
	tracker := engine.NewTracker()
	now := time.Now()

	store.Put(&Record{Kind: KindAgent, ID: int64(agent), AgentNo: agent, State: "WorkingContacts"})
	store.Put(&Record{Kind: KindAgent, ID: int64(bystander), AgentNo: bystander, State: "WorkingContacts"})

	for _, c := range contacts {
		state := engine.StateWithAgent
		if c == seed {
			state = engine.StateRouting
		}
		store.Put(&Record{Kind: KindContact, ID: c, AgentNo: agent, State: string(state)})
		tracker.OnContactStateChange(engine.ContactStateChange{
			ContactNo: c, AgentNo: agent, State: state, Timestamp: now,
			Trigger: "AGENT_ASSIGNED", BusNo: 123, TenantID: "demo-tenant",
		})
	}
	for _, c := range bystanderContacts {
		store.Put(&Record{Kind: KindContact, ID: c, AgentNo: bystander, State: string(engine.StateWithAgent)})
		tracker.OnContactStateChange(engine.ContactStateChange{
			ContactNo: c, AgentNo: bystander, State: engine.StateWithAgent, Timestamp: now,
			Trigger: "AGENT_ASSIGNED", BusNo: 123, TenantID: "demo-tenant",
		})
	}

	sc := Scenario{
		AgentNo:     agent,
		SeedContact: seed,
		AllContacts: contacts,
		Seed: engine.FailureRecord{
			Service:   "orch-entity-contact",
			Method:    "AssignContact",
			ContactNo: seed,
			AgentNo:   agent,
			BusNo:     123,
			TenantID:  "demo-tenant",
			Reason:    "INVALID_ARGUMENT: AgentNo is incorrect",
		},
	}
	return store, tracker, sc
}

// StuckFixture builds the second scenario: agent 50 with 2 contacts; contact 2001 is stuck
// in ROUTING past the ring timeout (its dwell start is 92s in the past), 2002 is healthy.
// Agent 51 (bystander) is also active with 2 healthy contacts — unaffected.
func StuckFixture() (*Store, *engine.Tracker, Scenario) {
	const agent int32 = 50
	const bystander int32 = 51
	const stuck = int64(2001)

	store := NewStore()
	tracker := engine.NewTracker()
	now := time.Now()

	store.Put(&Record{Kind: KindAgent, ID: int64(agent), AgentNo: agent, State: "WorkingContacts"})
	store.Put(&Record{Kind: KindAgent, ID: int64(bystander), AgentNo: bystander, State: "WorkingContacts"})

	store.Put(&Record{Kind: KindContact, ID: stuck, AgentNo: agent, State: string(engine.StateRouting), Stuck: true})
	tracker.OnContactStateChange(engine.ContactStateChange{
		ContactNo: stuck, AgentNo: agent, State: engine.StateRouting,
		Timestamp: now.Add(-92 * time.Second), Trigger: "MATCH_FOUND", BusNo: 77, TenantID: "demo-tenant",
	})
	for _, c := range []int64{2002, 2003, 2004} {
		store.Put(&Record{Kind: KindContact, ID: c, AgentNo: agent, State: string(engine.StateWithAgent)})
		tracker.OnContactStateChange(engine.ContactStateChange{
			ContactNo: c, AgentNo: agent, State: engine.StateWithAgent,
			Timestamp: now, Trigger: "AGENT_ASSIGNED", BusNo: 77, TenantID: "demo-tenant",
		})
	}
	store.Put(&Record{Kind: KindContact, ID: 2101, AgentNo: bystander, State: string(engine.StateWithAgent)})
	store.Put(&Record{Kind: KindContact, ID: 2102, AgentNo: bystander, State: string(engine.StateWithAgent)})

	return store, tracker, Scenario{AgentNo: agent, SeedContact: stuck, AllContacts: []int64{2001, 2002, 2003, 2004}}
}

// ACWFixture: agent 60 with a contact wedged in AFTER_CONTACT_WORK past the ACW timeout,
// blocking the agent from taking new contacts; a second contact is healthy.
// Agent 61 (bystander) is also active with 2 healthy contacts — unaffected.
func ACWFixture() (*Store, *engine.Tracker, Scenario) {
	const agent int32 = 60
	const bystander int32 = 61
	const stuck = int64(3001)

	store := NewStore()
	tracker := engine.NewTracker()
	now := time.Now()

	store.Put(&Record{Kind: KindAgent, ID: int64(agent), AgentNo: agent, State: "WorkingContacts"})
	store.Put(&Record{Kind: KindAgent, ID: int64(bystander), AgentNo: bystander, State: "WorkingContacts"})
	store.Put(&Record{Kind: KindContact, ID: stuck, AgentNo: agent, State: string(engine.AgentAfterContactWork), Stuck: true})
	tracker.OnContactStateChange(engine.ContactStateChange{
		ContactNo: stuck, AgentNo: agent, State: engine.StateWithAgent,
		Timestamp: now.Add(-120 * time.Second), Trigger: "ENTER_ACW", BusNo: 88, TenantID: "demo-tenant",
	})
	for _, c := range []int64{3002, 3003, 3004} {
		store.Put(&Record{Kind: KindContact, ID: c, AgentNo: agent, State: string(engine.StateWithAgent)})
	}
	store.Put(&Record{Kind: KindContact, ID: 3101, AgentNo: bystander, State: string(engine.StateWithAgent)})
	store.Put(&Record{Kind: KindContact, ID: 3102, AgentNo: bystander, State: string(engine.StateWithAgent)})

	return store, tracker, Scenario{AgentNo: agent, SeedContact: stuck, AllContacts: []int64{3001, 3002, 3003, 3004}}
}

// QueueFixture: agent 70 available, but contact 4001 is stuck in QUEUING past the match SLA
// (no match produced — FindMatch/Match Processor backlog).
// Agent 71 (bystander) is also awaiting contacts — unaffected.
func QueueFixture() (*Store, *engine.Tracker, Scenario) {
	const agent int32 = 70
	const bystander int32 = 71
	const stuck = int64(4001)

	store := NewStore()
	tracker := engine.NewTracker()
	now := time.Now()

	store.Put(&Record{Kind: KindAgent, ID: int64(agent), AgentNo: agent, State: "AwaitingContacts"})
	store.Put(&Record{Kind: KindAgent, ID: int64(bystander), AgentNo: bystander, State: "AwaitingContacts"})
	// stuck contact shown under agent 70 on the floor so it's visible
	store.Put(&Record{Kind: KindContact, ID: stuck, AgentNo: agent, State: string(engine.StateQueuing), Stuck: true})
	tracker.OnContactStateChange(engine.ContactStateChange{
		ContactNo: stuck, AgentNo: agent, State: engine.StateQueuing,
		Timestamp: now.Add(-300 * time.Second), Trigger: "QUEUED", BusNo: 99, TenantID: "demo-tenant",
	})
	store.Put(&Record{Kind: KindContact, ID: 4002, AgentNo: agent, State: string(engine.StateWithAgent)})
	store.Put(&Record{Kind: KindContact, ID: 4101, AgentNo: bystander, State: string(engine.StateWithAgent)})
	store.Put(&Record{Kind: KindContact, ID: 4102, AgentNo: bystander, State: string(engine.StateWithAgent)})

	return store, tracker, Scenario{AgentNo: agent, SeedContact: stuck, AllContacts: []int64{4001, 4002}}
}
