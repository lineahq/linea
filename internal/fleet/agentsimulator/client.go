package agentsimulator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash/fnv"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/open-telemetry/opamp-go/client"
	"github.com/open-telemetry/opamp-go/client/types"
	"github.com/open-telemetry/opamp-go/protobufs"
)

// collectorHeartbeat is the opamp-go WebSocket default, which the Collector's
// OpAMP extension keeps: it does not declare ReportsHeartbeat.
const collectorHeartbeat = 30 * time.Second

// tap observes one connection of an official opamp-go client. The client owns
// dialing, retries, heartbeats and reconnects; the tap only counts bytes, reads
// the HTTP status of a refused handshake and reports when a session ends.
type tap struct {
	net.Conn
	r           *runner
	p           *peer
	head        [12]byte // first response bytes: "HTTP/1.1 503"
	n           int
	established atomic.Bool
	once        sync.Once
}

func (t *tap) Read(b []byte) (int, error) {
	n, err := t.Conn.Read(b)
	if t.n < len(t.head) {
		t.n += copy(t.head[t.n:], b[:n])
	}
	t.r.stats.received.Add(uint64(n))
	return n, err
}

func (t *tap) Write(b []byte) (int, error) {
	n, err := t.Conn.Write(b)
	t.r.stats.sent.Add(uint64(n))
	return n, err
}

func (t *tap) Close() error {
	t.once.Do(func() { t.r.closed(t.p, t) })
	return t.Conn.Close()
}

// status is the handshake's HTTP status, or 0 when it was not read in
// plaintext (over TLS the tap sees ciphertext).
func (t *tap) status() int {
	if t.n < len(t.head) || string(t.head[:7]) != "HTTP/1." {
		return 0
	}
	code, err := strconv.Atoi(string(t.head[9:12]))
	if err != nil {
		return 0
	}
	return code
}

var errNoDownloads = errors.New("simulated agents accept no packages")

func noDownloads(context.Context, *protobufs.DownloadableFile) (*http.Client, error) {
	return nil, errNoDownloads
}

// start runs a new opamp-go client for the peer's current identity, set up
// like a Collector's OpAMP extension that reports status and health.
func (r *runner) start(p *peer) error {
	p.acked.Store(false)
	p.configs.Store(0)
	p.stopping.Store(false)
	c := client.NewWebSocket(nil)
	description := &protobufs.AgentDescription{IdentifyingAttributes: []*protobufs.KeyValue{
		{Key: "service.name", Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: "linea-fleet-simulator"}}},
		{Key: "service.instance.id", Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: hex.EncodeToString(p.uid[:])}}},
	}}
	if len(r.padding) > 0 {
		description.NonIdentifyingAttributes = []*protobufs.KeyValue{{Key: "fleet.capacity.synthetic_padding", Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_BytesValue{BytesValue: r.padding}}}}
	}
	if err := c.SetAgentDescription(description); err != nil {
		return err
	}
	if err := c.SetHealth(&protobufs.ComponentHealth{Healthy: p.healthy, StartTimeUnixNano: uint64(r.started.UnixNano())}); err != nil {
		return err
	}
	capabilities := protobufs.AgentCapabilities_AgentCapabilities_ReportsStatus | protobufs.AgentCapabilities_AgentCapabilities_ReportsHealth
	settings := types.StartSettings{
		OpAMPServerURL: r.config.Endpoint,
		Header:         http.Header{"Authorization": {"Bearer " + r.config.Token}},
		InstanceUid:    types.InstanceUid(p.uid),
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return r.dial(ctx, p, network, addr)
		},
		RemoteConfigStatus: p.status.Load(),
		Callbacks: types.Callbacks{
			OnConnect:          func(context.Context) { r.connected(p) },
			OnConnectFailed:    func(context.Context, error) { r.connectFailed(p) },
			OnMessage:          func(_ context.Context, m *types.MessageData) { r.message(p, c, m) },
			OnError:            func(context.Context, *protobufs.ServerErrorResponse) { r.serverError() },
			GetEffectiveConfig: func(context.Context) (*protobufs.EffectiveConfig, error) { r.fullState(p); return nil, nil },
			DownloadHTTPClient: noDownloads,
		},
	}
	// Other cadences declare ReportsHeartbeat, which opamp-go requires
	// before it applies a configured interval.
	if r.config.Heartbeat != collectorHeartbeat {
		capabilities |= protobufs.AgentCapabilities_AgentCapabilities_ReportsHeartbeat
		heartbeat := r.config.Heartbeat
		settings.HeartbeatInterval = &heartbeat
	}
	if r.config.AcceptConfig {
		capabilities |= protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig | protobufs.AgentCapabilities_AgentCapabilities_ReportsRemoteConfig
	}
	if len(r.builds) > 0 {
		capabilities |= protobufs.AgentCapabilities_AgentCapabilities_ReportsAvailableComponents
	}
	if err := c.SetCapabilities(&capabilities); err != nil {
		return err
	}
	if len(r.builds) > 0 {
		if err := c.SetAvailableComponents(r.build(p)); err != nil {
			return err
		}
	}
	p.client = c
	return c.Start(context.Background(), settings)
}

// stop ends the session like a Collector shutting down: opamp-go sends
// AgentDisconnect and closes the socket. The loss is not counted.
func (r *runner) stop(p *peer) {
	if p.client == nil {
		return
	}
	p.stopping.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = p.client.Stop(ctx)
	p.client = nil
}

// dial, connectFailed and connected run on the client's connect goroutine,
// one attempt at a time.
func (r *runner) dial(ctx context.Context, p *peer, network, addr string) (net.Conn, error) {
	p.attempt++
	p.pending = nil
	r.stats.update(func(c *Counters) { c.DialAttempts++ })
	conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	p.pending = &tap{Conn: conn, r: r, p: p}
	return p.pending, nil
}

// connectFailed counts a failed attempt once; opamp-go reports some twice.
// Admission refusals are retried with the client's backoff. Other failures
// are expected only during an outage; an unread status over TLS is left to
// the session deadline.
func (r *runner) connectFailed(p *peer) {
	if p.failed == p.attempt || p.stopping.Load() {
		return
	}
	p.failed = p.attempt
	status := 0
	if p.pending != nil {
		status = p.pending.status()
	}
	refused := status == http.StatusServiceUnavailable || status == http.StatusTooManyRequests
	outage := r.outage.Load()
	r.stats.update(func(c *Counters) {
		c.DialFailures++
		if refused {
			c.Refusals++
		}
		if outage {
			c.ReconnectRetries++
		}
	})
	if !refused && !outage && (status != 0 || !r.tls) {
		r.fail("handshake_failed")
	}
}

func (r *runner) connected(p *peer) {
	if p.pending == nil {
		return
	}
	p.pending.established.Store(true)
	p.acked.Store(false)
	r.stats.update(func(c *Counters) { c.Active++; c.PeakActive = max(c.PeakActive, c.Active) })
}

// closed handles the end of a socket. Outside an armed outage an unplanned
// loss is a failure; during one opamp-go reconnects and the loss is counted.
func (r *runner) closed(p *peer, t *tap) {
	if !t.established.Load() {
		return
	}
	r.stats.update(func(c *Counters) { c.Active-- })
	if p.stopping.Load() || r.ctx.Err() != nil {
		return
	}
	if !r.outage.Load() {
		r.stats.update(func(c *Counters) { c.RequestErrors++ })
		r.fail("connection_lost")
		return
	}
	storm, now := r.storm.Load(), time.Now().UnixNano()
	r.stormLost.CompareAndSwap(0, now)
	r.stormLastLost.Store(now)
	if p.lostIn.Swap(storm) != storm {
		r.lostCount.Add(1)
		r.stats.update(func(c *Counters) { c.StormLost++ })
	}
}

// message counts a server response. The first after each connect means the
// server registered the session.
func (r *runner) message(p *peer, c client.OpAMPClient, m *types.MessageData) {
	// Reporting never offers connection settings, identity changes or plugin
	// messages, and configuration only to agents that accept it. opamp-go
	// drops commands itself, because the simulated agent, like a reporting
	// Collector, does not accept them.
	if m.AgentIdentification != nil || m.CustomMessage != nil || m.CustomCapabilities != nil || m.OfferedConnectionsSettingsHash != nil || m.PackagesAvailable != nil {
		r.fail("unexpected_response")
		return
	}
	if m.RemoteConfig != nil {
		r.apply(p, c, m.RemoteConfig.GetConfigHash())
	}
	if sent := p.healthSent.Swap(0); sent != 0 {
		r.stats.latency(time.Since(time.Unix(0, sent)))
	}
	p.progress.Add(1)
	r.stats.update(func(c *Counters) { c.Acknowledged++ })
	if p.acked.Swap(true) {
		return
	}
	if storm := r.storm.Load(); r.outage.Load() && p.lostIn.Load() == storm && p.backIn.Swap(storm) != storm {
		now := time.Now().UnixNano()
		r.stormFirstBack.CompareAndSwap(0, now)
		r.stormLast.Store(now)
		r.backCount.Add(1)
		r.stats.update(func(c *Counters) { c.StormRecovered++ })
	}
	select {
	case p.registered <- struct{}{}:
	default:
	}
}

// apply reports an offered config like the Supervisor: APPLYING at once, the
// outcome after the apply delay. Whether it fails is fixed per agent and
// config, so a resent config fails again. A newer offer supersedes a pending
// outcome. opamp-go drops offers to agents that do not accept config.
func (r *runner) apply(p *peer, c client.OpAMPClient, hash []byte) {
	r.stats.update(func(c *Counters) { c.ConfigsOffered++ })
	applying := &protobufs.RemoteConfigStatus{LastRemoteConfigHash: hash, Status: protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLYING}
	p.status.Store(applying)
	_ = c.SetRemoteConfigStatus(applying)
	f := fnv.New32a()
	_, _ = f.Write(p.uid[:])
	_, _ = f.Write(hash)
	outcome := &protobufs.RemoteConfigStatus{LastRemoteConfigHash: hash, Status: protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED}
	if int(f.Sum32()%100) < r.config.FailPercent {
		outcome.Status, outcome.ErrorMessage = protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED, "simulated apply failure"
	}
	time.AfterFunc(r.config.ApplyDelay, func() {
		if r.ctx.Err() != nil || !p.status.CompareAndSwap(applying, outcome) {
			return
		}
		if c.SetRemoteConfigStatus(outcome) == nil {
			r.stats.update(func(c *Counters) {
				if outcome.Status == protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED {
					c.ConfigsFailed++
				} else {
					c.ConfigsApplied++
				}
			})
		}
	})
}

// builds returns n builds with the components of a typical contrib
// Collector, and a last minimal build without the filelog receiver. Each
// has its own hash, like distinct distributions.
func builds(n int) []*protobufs.AvailableComponents {
	full := map[string][]string{
		"receivers":  {"otlp", "filelog", "hostmetrics", "prometheus"},
		"processors": {"batch", "memory_limiter", "resourcedetection"},
		"exporters":  {"debug", "otlp", "otlphttp"},
		"extensions": {"opamp", "health_check"},
		"connectors": {"forward"},
	}
	out := make([]*protobufs.AvailableComponents, n+1)
	for i := range out {
		ac := &protobufs.AvailableComponents{Components: map[string]*protobufs.ComponentDetails{}}
		for section, names := range full {
			sub := map[string]*protobufs.ComponentDetails{}
			for _, t := range names {
				if i == n && t == "filelog" {
					continue
				}
				sub[t] = &protobufs.ComponentDetails{}
			}
			ac.Components[section] = &protobufs.ComponentDetails{SubComponentMap: sub}
		}
		digest := sha256.Sum256(binary.BigEndian.AppendUint32([]byte("linea-fleet-simulated-build"), uint32(i)))
		ac.Hash = bytes.Clone(digest[:])
		out[i] = ac
	}
	return out
}

// build picks the peer's build from its identity.
func (r *runner) build(p *peer) *protobufs.AvailableComponents {
	v := binary.BigEndian.Uint32(p.uid[:4])
	if int(v%100) < r.config.MinimalPercent {
		return r.builds[len(r.builds)-1]
	}
	return r.builds[int(v/100)%(len(r.builds)-1)]
}

// fullState counts the full-state reports the server requested; the first
// report of each client is full by itself.
func (r *runner) fullState(p *peer) {
	if p.configs.Add(1) > 1 {
		r.stats.update(func(c *Counters) { c.Resyncs++ })
	}
}

func (r *runner) serverError() {
	r.stats.update(func(c *Counters) { c.ServerErrors++ })
	if !r.outage.Load() {
		r.fail("server_error")
	}
}
