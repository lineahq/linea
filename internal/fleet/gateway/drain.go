package gateway

// Drain rejects new sockets without interrupting already admitted reports.
func (g *Gateway) Drain() {
	g.mu.Lock()
	g.draining = true
	g.mu.Unlock()
	g.admission.close()
}

// CloseAgent closes the local socket of an agent whose identity was taken
// over by a newer generation on any replica. It never blocks.
func (g *Gateway) CloseAgent(id string, generation uint64) {
	g.mu.Lock()
	e, ok := g.agents[id]
	g.mu.Unlock()
	if ok && e.generation < generation {
		disconnect(e.conn)
	}
}

// CloseCredential closes every local socket authenticated by a revoked
// credential. The periodic sweep remains the fallback for missed hints.
func (g *Gateway) CloseCredential(id string) {
	g.mu.Lock()
	var peers []*session
	for _, s := range g.connections {
		if s.principal.ID == id {
			peers = append(peers, s)
		}
	}
	g.mu.Unlock()
	for _, s := range peers {
		disconnect(s.conn)
	}
}
