package gateway

import (
	"net/http"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/open-telemetry/opamp-go/protobufs"
)

func TestDrainRefusesNewConnectionsButFinishesReports(t *testing.T) {
	g, _, endpoint, token := setup(t, 3, 3)
	c := dial(t, endpoint, token)
	uid := make([]byte, 16)
	exchange(t, c, &protobufs.AgentToServer{InstanceUid: uid, SequenceNum: 1})
	exchange(t, c, &protobufs.AgentToServer{InstanceUid: uid, SequenceNum: 2})
	g.Drain()
	other, response, err := websocket.DefaultDialer.Dial(endpoint, http.Header{"Authorization": {"Bearer " + token}})
	if other != nil {
		other.Close()
	}
	if err == nil || response == nil || response.StatusCode != 503 {
		t.Fatal("draining admitted a new socket")
	}
	response.Body.Close()
	exchange(t, c, &protobufs.AgentToServer{InstanceUid: uid, SequenceNum: 3, Health: &protobufs.ComponentHealth{Healthy: true}})
}
