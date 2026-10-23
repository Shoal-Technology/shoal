package controller

import (
	"context"
	"fmt"
	"net/netip"
	"slices"

	"google.golang.org/protobuf/proto"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/objects"
	"github.com/Shoal-Technology/shoal/internal/tenancy"
)

// syncServices gives every Service a virtual IP from the service CIDR and
// sets its endpoints to the IPs of the selected workload's available
// replicas on Ready nodes. Node uncertainty withdraws traffic endpoints without
// changing assignment retention or the eviction deadline. The controller is the
// only writer of Service status.
func (c *Controller) syncServices(ctx context.Context, s *state) []error {
	used := map[netip.Addr]bool{}
	for _, svc := range s.services {
		if ip, err := netip.ParseAddr(svc.Status.GetClusterIp()); err == nil {
			used[ip] = true
		}
	}
	var errs []error
	for _, name := range sortedKeys(s.services) {
		svc := s.services[name]
		st := proto.CloneOf(svc.Status)
		if st.ClusterIp == "" {
			ip, ok := c.freeServiceIP(used)
			if !ok {
				errs = append(errs, fmt.Errorf("service %s: no free IP left in %s", name, c.cfg.ServiceCIDR))
				continue
			}
			used[ip], st.ClusterIp = true, ip.String()
			c.log.Info("service IP assigned", "service", name, "ip", st.ClusterIp)
		}
		st.Endpoints = nil
		for _, r := range s.replicasOf(tenancy.Key(svc.GetMetadata().GetProject(), svc.Spec.GetSelector())) {
			if available(r) && r.obj.Status.GetIp() != "" &&
				s.nodes[r.obj.Spec.GetNode()].GetStatus().GetPhase() == v1alpha1.NodeReady {
				st.Endpoints = append(st.Endpoints, r.obj.Status.GetIp())
			}
		}
		slices.Sort(st.Endpoints)
		if proto.Equal(svc.Status, st) {
			continue
		}
		b, err := objects.Encode(st)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := c.st.UpdateStatus(ctx, v1alpha1.KindService, name, b); err != nil {
			errs = append(errs, fmt.Errorf("update status of service %s: %w", name, err))
			continue
		}
		svc.Status = st
	}
	return errs
}

// freeServiceIP returns the lowest unused address of the service CIDR,
// skipping the network address.
func (c *Controller) freeServiceIP(used map[netip.Addr]bool) (netip.Addr, bool) {
	p := c.cfg.ServiceCIDR
	for ip := p.Addr().Next(); p.Contains(ip); ip = ip.Next() {
		if !used[ip] && p.Contains(ip.Next()) { // not the broadcast address
			return ip, true
		}
	}
	return netip.Addr{}, false
}
