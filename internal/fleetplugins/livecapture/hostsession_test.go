package livecapture

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lineahq/linea/pluginsdk"
)

// A silent host needs two polls in a row to end the session; any answer that
// refuses it, a 421 included, or a changed epoch ends it at once.
func TestHostSessionLossPolicy(t *testing.T) {
	const closed = "http://127.0.0.1:1"
	var status atomic.Int32
	epoch := strings.Repeat("1", 32)
	var current atomic.Value
	current.Store(epoch)
	var instance string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != pluginsdk.SessionPath {
			w.WriteHeader(404)
			return
		}
		if code := int(status.Load()); code != 0 {
			w.WriteHeader(code)
			return
		}
		_ = json.NewEncoder(w).Encode(pluginsdk.Session{PluginID: "test", InstanceID: instance, Epoch: current.Load().(string), Permissions: []string{pluginsdk.AgentsRead, pluginsdk.CredentialsIntrospect}})
	}))
	t.Cleanup(server.Close)

	steps := []struct {
		name     string
		endpoint string
		status   int32
		epoch    string
		held     []bool // hostOK after each poll
	}{
		{"one silent poll is tolerated", closed, 0, epoch, []bool{true}},
		{"two silent polls end the session", closed, 0, epoch, []bool{true, false}},
		{"421 ends it at once", server.URL, 421, epoch, []bool{false}},
		{"403 ends it at once", server.URL, 403, epoch, []bool{false}},
		// The new epoch is adopted in the same poll, after the old work ended.
		{"a changed epoch ends the old work at once", server.URL, 0, strings.Repeat("2", 32), []bool{true}},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			s, _, _ := fixture(t)
			instance = s.instance
			s.host.Endpoint = server.URL
			status.Store(0)
			current.Store(epoch)
			s.checkHost()
			if !s.hostOK || s.epoch != epoch {
				t.Fatal("session not established")
			}
			s.host.Endpoint = step.endpoint
			status.Store(step.status)
			current.Store(step.epoch)
			for i, want := range step.held {
				s.checkHost()
				s.mu.Lock()
				got := s.hostOK
				s.mu.Unlock()
				if got != want {
					t.Fatalf("poll %d: hostOK is %v, want %v", i+1, got, want)
				}
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if step.epoch != epoch && (s.epoch != step.epoch || len(s.channels) != 0) {
				t.Fatalf("old epoch work survived: epoch %q, %d channels", s.epoch, len(s.channels))
			}
		})
	}
	t.Run("a good poll resets the count", func(t *testing.T) {
		s, _, _ := fixture(t)
		instance = s.instance
		s.host.Endpoint = server.URL
		status.Store(0)
		current.Store(epoch)
		s.checkHost()
		s.host.Endpoint = closed
		s.checkHost()
		s.host.Endpoint = server.URL
		s.checkHost()
		s.host.Endpoint = closed
		s.checkHost()
		if !s.hostOK {
			t.Fatal("silent polls separated by an answer ended the session")
		}
	})
	t.Run("the session poll has its own client", func(t *testing.T) {
		s, _, _ := fixture(t)
		if s.host.SessionHTTP == nil || s.host.SessionHTTP == s.host.HTTP || s.host.SessionHTTP.Transport == s.host.HTTP.Transport {
			t.Fatal("session poll shares the general host client")
		}
	})
}
