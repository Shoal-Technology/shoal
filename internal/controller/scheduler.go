package controller

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	"example.com/shoal/api/v1alpha1"
	"example.com/shoal/internal/resource"
)

// request is what a replica asks of a node.
type request struct {
	cpu, memory int64 // millicores, bytes; 0 = no request
	hostPorts   []string
}

func requestOf(t *v1alpha1.WorkloadSpec) request {
	var r request
	// Templates were validated on apply, so parse errors can't happen; a
	// zero request is the safe fallback anyway.
	if s := t.GetResources().GetCpu(); s != "" {
		r.cpu, _ = resource.ParseCPU(s)
	}
	if s := t.GetResources().GetMemory(); s != "" {
		r.memory, _ = resource.ParseMemory(s)
	}
	for _, p := range t.GetPorts() {
		if p.GetHostPort() != 0 {
			r.hostPorts = append(r.hostPorts, fmt.Sprintf("%d/%s", p.GetHostPort(), p.GetProtocol()))
		}
	}
	return r
}

// usage is what the replicas bound to a node use.
type usage struct {
	cpu, memory int64
	replicas    int
	byWorkload  map[string]int
	hostPorts   map[string]bool
}

// candidate is a Ready node with its capacity and current usage.
type candidate struct {
	name              string
	capCPU, capMemory int64 // 0 = unknown, treated as unlimited
	used              *usage
}

func (n *candidate) fits(r request) bool {
	if n.capCPU > 0 && n.used.cpu+r.cpu > n.capCPU {
		return false
	}
	if n.capMemory > 0 && n.used.memory+r.memory > n.capMemory {
		return false
	}
	for _, p := range r.hostPorts {
		if n.used.hostPorts[p] {
			return false
		}
	}
	return true
}

// free returns the average fraction of capacity left after placing r; 1 if
// the capacity is unknown.
func (n *candidate) free(r request) float64 {
	frac := func(capacity, used int64) float64 {
		if capacity <= 0 {
			return 1
		}
		return float64(capacity-used) / float64(capacity)
	}
	return (frac(n.capCPU, n.used.cpu+r.cpu) + frac(n.capMemory, n.used.memory+r.memory)) / 2
}

func (n *candidate) add(workload string, r request) {
	n.used.cpu += r.cpu
	n.used.memory += r.memory
	n.used.replicas++
	n.used.byWorkload[workload]++
	for _, p := range r.hostPorts {
		n.used.hostPorts[p] = true
	}
}

// schedule binds every unscheduled replica to a node: it filters Ready
// nodes by free resources and host ports, then picks the node with the most
// free resources, preferring nodes with fewer replicas of the same workload
// on a tie.
func (c *Controller) schedule(ctx context.Context, s *state) []error {
	nodes := map[string]*candidate{}
	for name, n := range s.nodes {
		if n.Status.GetPhase() != v1alpha1.NodeReady {
			continue
		}
		nodes[name] = &candidate{
			name: name, capCPU: n.Status.GetCapacity().GetCpuMillis(), capMemory: n.Status.GetCapacity().GetMemoryBytes(),
			used: &usage{byWorkload: map[string]int{}, hostPorts: map[string]bool{}},
		}
	}
	var pending []*replica
	for _, r := range s.replicas {
		node := r.obj.Spec.GetNode()
		if node == "" {
			pending = append(pending, r)
		} else if n, ok := nodes[node]; ok {
			n.add(r.obj.Spec.GetWorkload(), requestOf(r.obj.Spec.GetTemplate()))
		}
	}
	// Oldest first, so earlier replicas don't starve behind newer ones.
	slices.SortFunc(pending, func(a, b *replica) int {
		if d := a.rec.CreatedAt.Compare(b.rec.CreatedAt); d != 0 {
			return d
		}
		return cmp.Compare(a.rec.Name, b.rec.Name)
	})

	var errs []error
	for _, r := range pending {
		req := requestOf(r.obj.Spec.GetTemplate())
		workload := r.obj.Spec.GetWorkload()
		var best *candidate
		for _, name := range sortedKeys(nodes) {
			n := nodes[name]
			if !n.fits(req) {
				continue
			}
			if best == nil || better(n, best, req, workload) {
				best = n
			}
		}
		if best == nil {
			st := proto.CloneOf(r.obj.Status)
			st.Phase, st.Message = v1alpha1.ReplicaPending, unschedulableMessage(len(nodes), req)
			if err := c.setReplicaStatus(ctx, r, st); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		// Status first: once bound, the replica's status belongs to the
		// node's agent.
		if err := c.setReplicaStatus(ctx, r, &v1alpha1.ReplicaStatus{Phase: v1alpha1.ReplicaPending, Message: "scheduled to " + best.name}); err != nil {
			errs = append(errs, err)
			continue
		}
		spec := proto.CloneOf(r.obj.Spec)
		spec.Node = best.name
		if err := c.updateReplicaSpec(ctx, r, spec); err != nil {
			errs = append(errs, err)
			continue
		}
		best.add(workload, req)
		c.log.Info("replica scheduled", "replica", r.rec.Name, "workload", workload, "node", best.name)
	}
	return errs
}

// better reports whether a is a better choice than b for a replica of
// workload with request r. Nodes are visited in name order, so equal nodes
// resolve to the first name.
func better(a, b *candidate, r request, workload string) bool {
	if fa, fb := a.free(r), b.free(r); fa != fb {
		return fa > fb
	}
	if wa, wb := a.used.byWorkload[workload], b.used.byWorkload[workload]; wa != wb {
		return wa < wb
	}
	return a.used.replicas < b.used.replicas
}

func unschedulableMessage(readyNodes int, r request) string {
	if readyNodes == 0 {
		return "unschedulable: no ready nodes"
	}
	var needs []string
	if r.cpu > 0 {
		needs = append(needs, fmt.Sprintf("cpu %dm", r.cpu))
	}
	if r.memory > 0 {
		needs = append(needs, fmt.Sprintf("memory %dMi", r.memory>>20))
	}
	if len(r.hostPorts) > 0 {
		needs = append(needs, "host ports "+strings.Join(r.hostPorts, ","))
	}
	return fmt.Sprintf("unschedulable: no ready node has %s free", strings.Join(needs, ", "))
}
