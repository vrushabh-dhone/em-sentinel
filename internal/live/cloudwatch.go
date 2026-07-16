// Package live is the read-only adapter that connects CX Guardian to a LIVE Entity
// Management environment (ic-dev) by tapping the real cascade-seed signal in CloudWatch:
// the orch-entity-failure-queue Lambda's "Agent record ttl set" log (entityoperations.go:76),
// each carrying the agentNo whose whole record was wiped.
//
// Everything here is READ-ONLY (CloudWatch Logs FilterLogEvents). The Healer runs dry-run.
package live

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
)

// Config is resolved from environment variables (with ic-dev defaults).
type Config struct {
	Profile    string        // EM_AWS_PROFILE (default "aws-session" — the MFA session profile)
	Region     string        // EM_AWS_REGION (default us-west-2)
	LogGroup   string        // EM_FQ_LOG_GROUP (default /aws/lambda/orch-entity-failure-queue)
	Lookback   time.Duration // EM_CW_LOOKBACK (default 24h)
	Interval   time.Duration // poll cadence
	Threshold  int           // min wipes in window to call it a cascade
	BurstPerMin int          // wipes-per-minute that marks a CRITICAL burst
}

// FromEnv builds a Config. ok is always true — CloudWatch creds are validated lazily on the
// first query (so the UI can show a precise auth error rather than a silent "not configured").
func FromEnv() (Config, bool) {
	c := Config{
		Profile:     firstNonEmpty(os.Getenv("EM_AWS_PROFILE"), "aws-session"),
		Region:      firstNonEmpty(os.Getenv("EM_AWS_REGION"), "us-west-2"),
		LogGroup:    firstNonEmpty(os.Getenv("EM_FQ_LOG_GROUP"), "/aws/lambda/orch-entity-failure-queue"),
		Lookback:    durationEnv("EM_CW_LOOKBACK", 24*time.Hour),
		Interval:    30 * time.Second,
		Threshold:   3,
		BurstPerMin: 5,
	}
	return c, true
}

// Seed is one observed whole-agent cleanup (the cascade seed).
type Seed struct {
	TS      time.Time
	AgentNo int64
}

// Client queries the failure-queue Lambda log group.
type Client struct {
	cfg    Config
	cw     *cloudwatchlogs.Client
	initErr error
}

func NewClient(cfg Config) *Client {
	c := &Client{cfg: cfg}
	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithSharedConfigProfile(cfg.Profile),
		awsconfig.WithRegion(cfg.Region),
	)
	if err != nil {
		c.initErr = err
		return c
	}
	c.cw = cloudwatchlogs.NewFromConfig(awsCfg)
	return c
}

func (c *Client) LogGroup() string { return c.cfg.LogGroup }
func (c *Client) Lookback() time.Duration { return c.cfg.Lookback }

var reAgentNo = regexp.MustCompile(`"agentNo":\s*(\d+)`)

// Seeds returns every "Agent record ttl set" event in the lookback window.
func (c *Client) Seeds(ctx context.Context, now time.Time) ([]Seed, error) {
	if c.initErr != nil {
		return nil, c.initErr
	}
	if c.cw == nil {
		return nil, fmt.Errorf("cloudwatch client not initialized")
	}
	start := now.Add(-c.cfg.Lookback).UnixMilli()
	end := now.UnixMilli()

	var seeds []Seed
	var token *string
	for page := 0; page < 20; page++ { // cap pages to bound work
		out, err := c.cw.FilterLogEvents(ctx, &cloudwatchlogs.FilterLogEventsInput{
			LogGroupName:  aws.String(c.cfg.LogGroup),
			StartTime:     aws.Int64(start),
			EndTime:       aws.Int64(end),
			FilterPattern: aws.String(`"Agent record ttl set"`),
			NextToken:     token,
		})
		if err != nil {
			return nil, err
		}
		for _, e := range out.Events {
			if e.Message == nil {
				continue
			}
			m := reAgentNo.FindStringSubmatch(*e.Message)
			if m == nil {
				continue
			}
			agentNo, _ := strconv.ParseInt(m[1], 10, 64)
			ts := time.Time{}
			if e.Timestamp != nil {
				ts = time.UnixMilli(*e.Timestamp)
			}
			seeds = append(seeds, Seed{TS: ts, AgentNo: agentNo})
		}
		if out.NextToken == nil {
			break
		}
		token = out.NextToken
	}
	return seeds, nil
}

// Burst summarizes the cascade activity in a window.
type Burst struct {
	Total          int     // total agent wipes
	DistinctAgents int     // unique agentNos wiped
	PeakPerMin     int     // worst single-minute count
	PeakMinute     time.Time
	SampleAgents   []int64 // a few representative agentNos
}

// Summarize aggregates seeds into burst stats.
func Summarize(seeds []Seed) Burst {
	perMin := map[int64]int{}
	distinct := map[int64]struct{}{}
	var b Burst
	b.Total = len(seeds)
	for _, s := range seeds {
		distinct[s.AgentNo] = struct{}{}
		perMin[s.TS.Unix()/60]++
	}
	b.DistinctAgents = len(distinct)
	for minute, n := range perMin {
		if n > b.PeakPerMin {
			b.PeakPerMin = n
			b.PeakMinute = time.Unix(minute*60, 0).UTC()
		}
	}
	// up to 6 sample agentNos, newest first
	sort.Slice(seeds, func(i, j int) bool { return seeds[i].TS.After(seeds[j].TS) })
	seen := map[int64]struct{}{}
	for _, s := range seeds {
		if _, ok := seen[s.AgentNo]; ok {
			continue
		}
		seen[s.AgentNo] = struct{}{}
		b.SampleAgents = append(b.SampleAgents, s.AgentNo)
		if len(b.SampleAgents) >= 6 {
			break
		}
	}
	return b
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func durationEnv(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
