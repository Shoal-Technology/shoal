// Package containerd implements runtime.Runtime on top of containerd, with
// CNI bridge networking and an in-binary logging driver.
//
// All state lives in containerd (container labels) and in StateDir (logs,
// generated /etc files, IPAM), so the agent itself can restart freely.
package containerd

import (
	"io"

	"github.com/kaiserern/shoal/internal/runtime"
)

const (
	// LogDriverFlag is argv[1] when containerd's shim runs this binary as the
	// logging driver for a container: "shoal --shoal-log-driver <log path>".
	LogDriverFlag = "--shoal-log-driver"

	// LogMaxSize and LogMaxFiles bound per-container log disk usage.
	LogMaxSize  = 10 << 20
	LogMaxFiles = 3
)

// Options configures the containerd runtime. Zero values get defaults.
type Options struct {
	Address     string // containerd socket, default /run/containerd/containerd.sock
	Namespace   string // containerd namespace, default "shoal"
	StateDir    string // default /var/lib/shoal
	PodCIDR     string // container subnet on this node, default 10.250.0.0/24
	BridgeName  string // default "shoal0"
	NetworkName string // CNI network name, default "shoal"
	CNIBinDir   string // default /opt/cni/bin
	// LogDriverPath is the binary the shim runs as logging driver; it must
	// handle LogDriverFlag. Default: the current executable.
	LogDriverPath string
	// DNSServer, if set, is the containers' only nameserver (the agent's
	// resolver), with DNSSearch as search domains. Otherwise containers use
	// the host's upstream resolvers.
	DNSServer string
	DNSSearch []string
}

func (o *Options) setDefaults() {
	def := func(v *string, d string) {
		if *v == "" {
			*v = d
		}
	}
	def(&o.Address, "/run/containerd/containerd.sock")
	def(&o.Namespace, "shoal")
	def(&o.StateDir, "/var/lib/shoal")
	def(&o.PodCIDR, "10.250.0.0/24")
	def(&o.BridgeName, "shoal0")
	def(&o.NetworkName, "shoal")
	def(&o.CNIBinDir, "/opt/cni/bin")
}

// Client is a runtime.Runtime that holds a containerd connection.
type Client interface {
	runtime.Runtime
	io.Closer
}
