package live

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// EventClient reconstructs FSM dwell from the REAL EM event stream in mon-na1 Loki.
// EM state-change logs ("event execution") carry the state enum + contact_no + timestamp:
//
//	method:ContactStateChangeV2 … contact_no:177… contact_state:UNIFIED_CONTACT_STATE_ROUTING
//	method:AgentContactStateChangeV2 … contact_no:177… agent_contact_state:UNIFIED_AGENT_CONTACT_STATE_AFTER_CONTACT_WORK
//
// It fetches those events over a window, keeps each contact's latest state+timestamp, and
// flags contacts dwelling in a target state past a threshold — the same dwell the Simulation
// feeds into Detector.InspectDwell. Read-only (Grafana datasource proxy).
type EventClient struct {
	grafanaURL string
	token      string
	lokiUID    string
	cell       string
	http       *http.Client
}

func NewEventClient() *EventClient {
	u := strings.TrimRight(os.Getenv("GRAFANA_URL"), "/")
	token := firstNonEmpty(os.Getenv("GRAFANA_TOKEN"), os.Getenv("GRAFANA_SERVICE_ACCOUNT_TOKEN"))
	if u == "" || token == "" {
		return nil
	}
	return &EventClient{
		grafanaURL: u,
		token:      token,
		lokiUID:    firstNonEmpty(os.Getenv("LOKI_UID"), "loki"),
		cell:       firstNonEmpty(os.Getenv("EM_CELL"), "phoenix"),
		http:       &http.Client{Timeout: 55 * time.Second},
	}
}

func (c *EventClient) Cell() string { return c.cell }

// Stuck is one contact dwelling in a state past its threshold.
type Stuck struct {
	ContactNo int64
	State     string
	AgeSec    int
}

var (
	// require whitespace before contact_no so we don't match point_of_contact_no:
	reEvContact = regexp.MustCompile(`(?:^|\s)contact_no:(\d+)`)
)

// Dwell fetches state-change events of `method`, parses the state via `stateRe` (group 1 =
// state suffix), keeps each contact's latest state, and returns those whose latest state is
// `target` and older than thresholdSec. Also returns total events and distinct contacts seen.
func (c *EventClient) Dwell(ctx context.Context, lookback time.Duration, now time.Time,
	method string, stateRe *regexp.Regexp, target string, thresholdSec int) (stuck []Stuck, totalEvents, distinct int, err error) {

	logql := fmt.Sprintf(`{service_name="entitymanagement-%s"} |= %q`, c.cell, method)
	endpoint := fmt.Sprintf("%s/api/datasources/proxy/uid/%s/loki/api/v1/query_range",
		c.grafanaURL, url.PathEscape(c.lokiUID))
	q := url.Values{}
	q.Set("query", logql)
	q.Set("start", strconv.FormatInt(now.Add(-lookback).UnixNano(), 10))
	q.Set("end", strconv.FormatInt(now.UnixNano(), 10))
	q.Set("limit", "5000")
	q.Set("direction", "forward")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+q.Encode(), nil)
	if err != nil {
		return nil, 0, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, 0, 0, fmt.Errorf("loki %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	var lr struct {
		Data struct {
			Result []struct {
				Values [][2]string `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil {
		return nil, 0, 0, err
	}

	type latest struct {
		state string
		ts    int64 // ns
	}
	byContact := map[int64]latest{}
	for _, s := range lr.Data.Result {
		for _, v := range s.Values {
			totalEvents++
			line := v[1]
			cm := reEvContact.FindStringSubmatch(line)
			sm := stateRe.FindStringSubmatch(line)
			if cm == nil || sm == nil {
				continue
			}
			contactNo, _ := strconv.ParseInt(cm[1], 10, 64)
			tsNs, _ := strconv.ParseInt(v[0], 10, 64)
			if cur, ok := byContact[contactNo]; !ok || tsNs > cur.ts {
				byContact[contactNo] = latest{state: sm[1], ts: tsNs}
			}
		}
	}
	distinct = len(byContact)
	for contactNo, l := range byContact {
		if l.state != target {
			continue
		}
		age := int(now.Sub(time.Unix(0, l.ts)).Seconds())
		if age >= thresholdSec {
			stuck = append(stuck, Stuck{ContactNo: contactNo, State: l.state, AgeSec: age})
		}
	}
	return stuck, totalEvents, distinct, nil
}

// State-field regexes (group 1 = the state suffix, e.g. ROUTING / QUEUING / AFTER_CONTACT_WORK).
var (
	ReContactState      = regexp.MustCompile(`(?:^|\s)contact_state:UNIFIED_CONTACT_STATE_(\w+)`)
	ReAgentContactState = regexp.MustCompile(`agent_contact_state:UNIFIED_AGENT_CONTACT_STATE_(\w+)`)
)
