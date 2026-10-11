package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"

	"github.com/Shoal-Technology/shoal/internal/agent"
	"github.com/Shoal-Technology/shoal/internal/metrics"
	"github.com/Shoal-Technology/shoal/internal/nodestat"
)

// startMetrics serves /metrics, /healthz and /readyz on addr until ctx ends.
// The listener is bound before returning, so a bad address fails the command.
func startMetrics(ctx context.Context, addr string, reg *metrics.Registry, ready func() error,
	log *slog.Logger, wg *sync.WaitGroup) error {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("metrics listen on %s: %w", addr, err)
	}
	log.Info("metrics listening", "address", lis.Addr().String())
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := metrics.Serve(ctx, lis, metrics.Handler(reg, ready)); err != nil {
			log.Error("metrics server failed", "err", err)
		}
	}()
	return nil
}

// agentMetrics builds the agent's registry. Values are read at scrape time.
func agentMetrics(a *agent.Connected, stateDir string) *metrics.Registry {
	reg := metrics.NewRegistry()
	reg.NewCollector("shoal_agent_connected", "1 while connected to the server.", "gauge", nil,
		func(emit func(float64, ...string)) {
			v := 0.0
			if a.Stats().Connected {
				v = 1
			}
			emit(v)
		})
	reg.NewCollector("shoal_replicas", "Replicas on this node: desired (assigned), running, ready.",
		"gauge", []string{"state"}, func(emit func(float64, ...string)) {
			st := a.Stats()
			emit(float64(st.Desired), "desired")
			emit(float64(st.Running), "running")
			emit(float64(st.Ready), "ready")
		})
	reg.NewCollector("shoal_replica_restarts", "Restarts of the current replicas on this node (sum).",
		"gauge", nil, func(emit func(float64, ...string)) { emit(float64(a.Stats().Restarts)) })
	reg.NewCollector("shoal_node_memory_bytes", "Node memory in bytes (Linux only).", "gauge",
		[]string{"kind"}, func(emit func(float64, ...string)) {
			total, avail, err := nodestat.Memory()
			if err != nil {
				return
			}
			emit(float64(total), "total")
			emit(float64(avail), "available")
		})
	reg.NewCollector("shoal_node_disk_bytes", "Size and free bytes of the file system holding the state dir.",
		"gauge", []string{"kind"}, func(emit func(float64, ...string)) {
			total, free, err := nodestat.Disk(stateDir)
			if err != nil {
				return
			}
			emit(float64(total), "total")
			emit(float64(free), "free")
		})
	return reg
}

// agentReady is /readyz for an agent: ready while connected to the server.
func agentReady(a *agent.Connected) func() error {
	return func() error {
		if !a.Stats().Connected {
			return errors.New("not connected to the server")
		}
		return nil
	}
}
