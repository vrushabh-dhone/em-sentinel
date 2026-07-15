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

// VictimClient reads the OTHER half of the cascade from mon-na1 Loki: the healthy contacts
// that hit ERROR_CODE_RECORD_NOT_FOUND after their agent's record was wiped. This is the real,
// validated victim signal — EM service logs live under service_name="entitymanagement-<cell>".
//
// Read-only. Auth is a Grafana read-only service-account token via the datasource proxy, so
// no direct Loki network access is needed. Returns nil from NewVictimClient when unconfigured
// (GRAFANA_URL/GRAFANA_TOKEN absent) so the cascade still works on the CloudWatch seed alone.
type VictimClient struct {
	grafanaURL string
	token      string
	lokiUID    string
	http       *http.Client
}

func NewVictimClient() *VictimClient {
	url := strings.TrimRight(os.Getenv("GRAFANA_URL"), "/")
	token := firstNonEmpty(os.Getenv("GRAFANA_TOKEN"), os.Getenv("GRAFANA_SERVICE_ACCOUNT_TOKEN"))
	if url == "" || token == "" {
		return nil
	}
	return &VictimClient{
		grafanaURL: url,
		token:      token,
		lokiUID:    firstNonEmpty(os.Getenv("LOKI_UID"), "loki"),
		http:       &http.Client{Timeout: 45 * time.Second},
	}
}

var (
	reVictimAgent   = regexp.MustCompile(`agent_no:(\d+)`)
	reVictimContact = regexp.MustCompile(`contact_no:(\d+)`)
)

// Victims returns the distinct victim contacts and the agents they were attached to, observed
// in the lookback window. LogQL mirrors the investigator cascade-pattern.
func (c *VictimClient) Victims(ctx context.Context, lookback time.Duration, now time.Time) (contacts, agents []int64, err error) {
	const logql = `{service_name=~"entitymanagement.*"} |~ "ERROR_CODE_RECORD_NOT_FOUND" |~ "agent.?[Cc]ontact not found|agent not found"`
	endpoint := fmt.Sprintf("%s/api/datasources/proxy/uid/%s/loki/api/v1/query_range",
		c.grafanaURL, url.PathEscape(c.lokiUID))
	q := url.Values{}
	q.Set("query", logql)
	q.Set("start", strconv.FormatInt(now.Add(-lookback).UnixNano(), 10))
	q.Set("end", strconv.FormatInt(now.UnixNano(), 10))
	q.Set("limit", "1000")
	q.Set("direction", "backward")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+q.Encode(), nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, nil, fmt.Errorf("loki %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	var lr struct {
		Data struct {
			Result []struct {
				Values [][2]string `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil {
		return nil, nil, err
	}

	cset, aset := map[int64]struct{}{}, map[int64]struct{}{}
	for _, s := range lr.Data.Result {
		for _, v := range s.Values {
			line := v[1]
			if m := reVictimContact.FindStringSubmatch(line); m != nil {
				if n, e := strconv.ParseInt(m[1], 10, 64); e == nil {
					cset[n] = struct{}{}
				}
			}
			if m := reVictimAgent.FindStringSubmatch(line); m != nil {
				if n, e := strconv.ParseInt(m[1], 10, 64); e == nil {
					aset[n] = struct{}{}
				}
			}
		}
	}
	for k := range cset {
		contacts = append(contacts, k)
	}
	for k := range aset {
		agents = append(agents, k)
	}
	return contacts, agents, nil
}
