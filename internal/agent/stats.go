package agent

// Stats is a snapshot of a server-managed agent for /metrics and /readyz.
// Desired is the number of replicas assigned to this node; the rest count
// over the latest reconcile report.
type Stats struct {
	Connected bool
	Desired   int
	Running   int
	Ready     int
	Restarts  int
}

// Stats returns the current snapshot.
func (c *Connected) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.replicas
	s.Connected = c.connected
	return s
}

func (c *Connected) setConnected(v bool) {
	c.mu.Lock()
	c.connected = v
	c.mu.Unlock()
}

// countReplicas summarises a reconcile report. Callers hold c.mu.
func countReplicas(statuses []ReplicaStatus) Stats {
	var s Stats
	s.Desired = len(statuses)
	for _, r := range statuses {
		if r.Phase == PhaseRunning {
			s.Running++
		}
		if r.Ready {
			s.Ready++
		}
		s.Restarts += r.Restarts
	}
	return s
}
