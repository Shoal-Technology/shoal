package install

import (
	"fmt"
	"strings"
)

// Logical paths (prefixed by the host's root directory when one is set).
const (
	pathShoalBin         = "/usr/local/bin/shoal"
	pathRunc             = "/usr/local/bin/runc"
	pathContainerd       = "/usr/local/bin/containerd"
	dirCNI               = "/opt/cni/bin"
	pathContainerdConfig = "/etc/containerd/config.toml"
	pathContainerdUnit   = "/etc/systemd/system/containerd.service"
	pathServerUnit       = "/etc/systemd/system/shoal-server.service"
	pathAgentUnit        = "/etc/systemd/system/shoal-agent.service"
	pathSysctl           = "/etc/sysctl.d/99-shoal.conf"
	pathModules          = "/etc/modules-load.d/shoal.conf"
	dirState             = "/var/lib/shoal"
	pathRecord           = "/var/lib/shoal/init.json"
	pathAdmin            = "/var/lib/shoal/server/admin.conf"
	pathCreds            = "/var/lib/shoal/agent/credentials.conf"
	pathJoinToken        = "/run/shoal/join.token"
	dirRun               = "/run/shoal"
	dataDirServer        = "/var/lib/shoal/server"
	serverAddr           = "127.0.0.1:7443"
	containerdSocket     = "/run/containerd/containerd.sock"
)

const (
	unitServer     = "shoal-server.service"
	unitAgent      = "shoal-agent.service"
	unitContainerd = "containerd.service"
)

const modulesConf = "# managed by shoal init\nbr_netfilter\n"

const sysctlConf = "# managed by shoal init\n" +
	"net.ipv4.ip_forward = 1\n" +
	"net.bridge.bridge-nf-call-iptables = 1\n"

// sysctlKeys are the settings sysctlConf sets, in the order they are applied.
var sysctlKeys = []string{"net.ipv4.ip_forward", "net.bridge.bridge-nf-call-iptables"}

// containerdConfig disables the CRI plugins: Shoal talks to containerd through
// its client API, not through CRI (ADR 0017, 1.2 step 5). The plugin ids are
// the containerd 2.x names; the e2e workflow checks that CRI is not running.
const containerdConfig = "# managed by shoal init\n" +
	"version = 3\n" +
	"disabled_plugins = [\"io.containerd.cri.v1.images\", \"io.containerd.cri.v1.runtime\"]\n"

// containerdUnit is the upstream containerd.service, pinned as a copy.
const containerdUnit = `[Unit]
Description=containerd container runtime
Documentation=https://containerd.io
After=network.target local-fs.target

[Service]
ExecStartPre=-/sbin/modprobe overlay
ExecStart=/usr/local/bin/containerd

Type=notify
Delegate=yes
KillMode=process
Restart=always
RestartSec=5
LimitNPROC=infinity
LimitCORE=infinity
LimitNOFILE=infinity
TasksMax=infinity
OOMScoreAdjust=-999

[Install]
WantedBy=multi-user.target
`

// serverUnit runs the server with the web portal on :7444. ACME flags are
// added only when both --email and --accept-tos are given (ADR 0017, 1.5).
// acmeDir and caRoots are test-only overrides.
func serverUnit(email string, acceptTOS bool, acmeDir, caRoots string) string {
	args := []string{"/usr/local/bin/shoal", "server", "--listen", ":7443", "--ui-listen", ":7444",
		"--data-dir", dataDirServer}
	if email != "" && acceptTOS {
		args = append(args, "--acme-email", email, "--acme-accept-tos")
		if acmeDir != "" {
			args = append(args, "--acme-directory", acmeDir)
		}
		if caRoots != "" {
			args = append(args, "--acme-ca-roots", caRoots)
		}
	}
	return unit("Shoal server", []string{"network-online.target"}, strings.Join(args, " "), "always")
}

// agentUnit runs the node agent. The edge ports are always passed: they only
// listen while the node carries the shoal.dev/edge label. The join token is
// read from pathJoinToken on the first start and deleted by the agent.
func agentUnit(node, server, caHash string, local bool) string {
	args := []string{"/usr/local/bin/shoal", "agent", "--server", server,
		"--token-file", pathJoinToken, "--node-name", node,
		"--edge-http", ":80", "--edge-https", ":443"}
	if caHash != "" {
		args = append(args, "--ca-hash", caHash)
	}
	after := []string{"network-online.target", unitContainerd}
	if local {
		after = append(after, unitServer)
	}
	return unit("Shoal agent", after, strings.Join(args, " "), "on-failure")
}

func unit(desc string, after []string, exec, restart string) string {
	return fmt.Sprintf(`[Unit]
Description=%s
After=%s
Wants=network-online.target

[Service]
ExecStart=%s
Restart=%s
RestartSec=2

[Install]
WantedBy=multi-user.target
`, desc, strings.Join(after, " "), exec, restart)
}

// parseToken reads the join command that `shoal token create` prints.
func parseToken(out string) (server, token, caHash string, err error) {
	fields := strings.Fields(out)
	field := func(name string) string {
		for i := 0; i+1 < len(fields); i++ {
			if fields[i] == name {
				return fields[i+1]
			}
		}
		return ""
	}
	server, token, caHash = field("--server"), field("--token"), field("--ca-hash")
	if server == "" || token == "" || caHash == "" {
		return "", "", "", fmt.Errorf("token create printed no join command")
	}
	return server, token, caHash, nil
}
