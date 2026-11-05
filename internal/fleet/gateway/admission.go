package gateway

import (
	"context"
	"math/rand/v2"
	"strconv"
	"sync"
	"time"

	"github.com/open-telemetry/opamp-go/server/types"
)

// DefaultPendingSessions bounds the handshakes per replica that are between
// admission and their first commit. Admission then follows how fast the
// credential checks and the writer actually finish, instead of a fixed rate:
// a fast database admits a storm quickly, a slow one is not queued deeper.
// Two writer batches keep the writer committing while the next one fills;
// more would only queue.
const DefaultPendingSessions = 400

// admissionWait is how long a handshake may wait for a slot. A refusal pushes
// an opamp-go client into exponential backoff (x1.5 up to 60 s): an agent
// refused a few times waits far longer than the storm lasts, while slots go
// unused. Waiting is cheap and keeps the order of arrival. opamp-go, which
// the Collector's OpAMP extension and the supervisor use, gives a handshake
// 45 s; the wait leaves room for the credential check and the upgrade.
// Waiting handshakes are bounded by the agent TCP connection limit.
const admissionWait = 25 * time.Second

// responseWrite is the time to write a response once a handshake is admitted.
// It replaces the agent listener's write timeout, which counts from the
// request headers and so would expire during a long wait.
const responseWrite = 10 * time.Second

// handshakeHold frees a slot whose session never reached its first commit:
// the 10 s first-message deadline and the 3 s commit timeout, with margin.
const handshakeHold = 15 * time.Second

// admission is a semaphore over sessions in their handshake.
type admission struct {
	mu    sync.Mutex
	slots chan struct{}
	wait  time.Duration
	stop  chan struct{}
	once  sync.Once
}

func newAdmission(limit int) *admission {
	return &admission{slots: make(chan struct{}, limit), wait: admissionWait, stop: make(chan struct{})}
}

// close ends every wait; the replica is draining or closed.
func (a *admission) close() { a.once.Do(func() { close(a.stop) }) }

func (a *admission) setLimit(limit int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.slots = make(chan struct{}, limit)
}

func (a *admission) state() (slots chan struct{}, wait time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.slots, a.wait
}

// handshake holds one admission slot until its session commits or ends.
type handshake struct {
	once  sync.Once
	timer *time.Timer
	slots chan struct{}
}

func (h *handshake) release() {
	if h == nil {
		return
	}
	h.once.Do(func() {
		h.timer.Stop()
		<-h.slots
	})
}

// acquire takes a slot, waiting up to the admission wait or until admission
// closes. held reports that the handshake had to wait.
func (a *admission) acquire(ctx context.Context) (h *handshake, held, ok bool) {
	slots, wait := a.state()
	select {
	case slots <- struct{}{}:
	default:
		held = true
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case slots <- struct{}{}:
		case <-timer.C:
			return nil, held, false
		case <-ctx.Done():
			return nil, held, false
		case <-a.stop:
			return nil, held, false
		}
	}
	h = &handshake{slots: slots}
	h.timer = time.AfterFunc(handshakeHold, h.release)
	return h, held, true
}

// retryAfter spreads refused agents over 1-5 seconds.
func retryAfter() string { return strconv.Itoa(1 + rand.IntN(5)) }

// SetPendingSessions changes how many handshakes a replica runs at once; values
// below one select DefaultPendingSessions. Call it before serving.
func (g *Gateway) SetPendingSessions(limit int) {
	if limit < 1 {
		limit = DefaultPendingSessions
	}
	g.admission.setLimit(limit)
}

// Spread closes admitted sockets evenly over the window so agents reconnect
// to other replicas gradually instead of in one burst. Drain must run first.
func (g *Gateway) Spread(ctx context.Context, window time.Duration) {
	g.mu.Lock()
	peers := make([]types.Connection, 0, len(g.connections))
	for c := range g.connections {
		peers = append(peers, c)
	}
	g.mu.Unlock()
	if len(peers) == 0 {
		return
	}
	interval := window / time.Duration(len(peers))
	ticker := time.NewTicker(max(interval, time.Microsecond))
	defer ticker.Stop()
	for i, c := range peers {
		if i > 0 {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
		disconnect(c)
	}
}
