// Package agentsimulator is an optional capacity fixture, not a Collector runtime.
package agentsimulator

import (
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Bounds follow the workload model: 10,000 Collectors per
// replica, tested to 20,000 per installation.
const (
	MaxAgents      = 20000
	maxConnectRate = 1000
	// maxMessageRate bounds the steady heartbeat and health stream.
	maxMessageRate = 5000
	maxChurnRate   = 200
	maxStorms      = 8
	maxBuilds      = 64
)

// Config drives official opamp-go WebSocket clients, one per simulated agent,
// configured like the Collector's OpAMP extension. The clients own retries,
// backoff and heartbeats; the simulator only paces starts and planned
// restarts, and toggles health.
type Config struct {
	// Optional read-only fixture mount. The driver releases named phase gates.
	CheckpointDirectory                                 string
	Endpoint, Token                                     string
	AllowInsecure                                       bool
	Agents, ConnectRate, PayloadBytes, ReconnectPercent int
	// HealthChangeEvery toggles each agent's health every that many
	// heartbeat intervals; zero keeps it healthy.
	HealthChangeEvery int
	// ChurnRate replaces this many identities per second during the steady
	// phase: a session disconnects and returns under a fresh instance UID.
	ChurnRate int
	// Storms arms that many outage windows after the planned reconnects. In
	// each the driver breaks some or all sessions (for example by killing a
	// replica) and the run times how long the lost sessions take to recover,
	// within StormTimeout.
	Storms int
	// Heartbeat is the opamp-go heartbeat interval. 30 s is the Collector's;
	// another value declares ReportsHeartbeat so opamp-go applies it.
	Heartbeat, Duration, Hold, StormTimeout time.Duration
	// AcceptConfig declares AcceptsRemoteConfig and ReportsRemoteConfig like
	// a Collector run by the OpAMP Supervisor. Each offered config is
	// reported APPLYING, then after ApplyDelay APPLIED, or FAILED for
	// FailPercent of the agents. Without it opamp-go drops offered configs.
	AcceptConfig bool
	FailPercent  int
	ApplyDelay   time.Duration
	// Builds reports AvailableComponents, spreading agents over that many
	// builds with equal components. MinimalPercent of the agents instead
	// run a build without the filelog receiver.
	Builds, MinimalPercent int
}

func (c Config) Validate() error {
	if c.CheckpointDirectory != "" {
		info, err := os.Stat(c.CheckpointDirectory)
		if err != nil || !info.IsDir() || !filepath.IsAbs(c.CheckpointDirectory) {
			return errors.New("invalid checkpoint directory")
		}
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "/v1/opamp" || (u.Scheme != "ws" && u.Scheme != "wss") {
		return errors.New("invalid OpAMP endpoint")
	}
	if u.Scheme == "ws" && !c.AllowInsecure {
		ip := net.ParseIP(u.Hostname())
		if ip == nil || !ip.IsLoopback() {
			return errors.New("protected plaintext transport must be explicitly acknowledged")
		}
	}
	if strings.ContainsAny(c.Token, "\r\n\x00") {
		return errors.New("invalid bearer token")
	}
	if len(c.Token) < 32 || len(c.Token) > 1024 || c.Agents < 1 || c.Agents > MaxAgents || c.ConnectRate < 1 || c.ConnectRate > maxConnectRate || c.PayloadBytes < 0 || c.PayloadBytes > 64<<10 || c.ReconnectPercent < 0 || c.ReconnectPercent > 25 || c.HealthChangeEvery < 0 || c.HealthChangeEvery > 1000 || c.ChurnRate < 0 || c.ChurnRate > maxChurnRate {
		return errors.New("simulation bounds exceeded")
	}
	if c.Heartbeat < 100*time.Millisecond || c.Heartbeat > time.Minute || c.Duration < 2*c.Heartbeat || c.Duration > time.Hour || c.Hold < time.Second || c.Hold > 30*time.Second {
		return errors.New("invalid simulation timing")
	}
	if c.Storms < 0 || c.Storms > maxStorms || c.Storms > 0 && (c.StormTimeout < 10*time.Second || c.StormTimeout > 10*time.Minute) {
		return errors.New("invalid storm timeout")
	}
	if c.FailPercent < 0 || c.FailPercent > 100 || c.ApplyDelay < 0 || c.ApplyDelay > time.Minute || !c.AcceptConfig && (c.FailPercent != 0 || c.ApplyDelay != 0) {
		return errors.New("invalid config acceptance")
	}
	if c.Builds < 0 || c.Builds > maxBuilds || c.MinimalPercent < 0 || c.MinimalPercent > 100 || c.Builds == 0 && c.MinimalPercent != 0 {
		return errors.New("invalid component builds")
	}
	// Bound the steady stream: heartbeats, health changes, and two
	// exchanges per new session (registration and the full state).
	rate := float64(c.Agents) / c.Heartbeat.Seconds()
	if c.HealthChangeEvery > 0 {
		rate += rate / float64(c.HealthChangeEvery)
	}
	if rate+float64(2*c.ConnectRate+2*c.ChurnRate) > maxMessageRate {
		return errors.New("message rate exceeds the simulation bound")
	}
	return nil
}

func (c Config) reconnectCount() int {
	if c.ReconnectPercent == 0 {
		return 0
	}
	return max(1, (c.Agents*c.ReconnectPercent+99)/100)
}
