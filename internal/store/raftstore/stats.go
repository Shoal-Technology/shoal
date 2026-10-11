package raftstore

// Stats returns the Raft node's counters (commit_index, applied_index,
// last_log_index, state, ...) as reported by hashicorp/raft.
func (s *Store) Stats() map[string]string { return s.raft.Stats() }
