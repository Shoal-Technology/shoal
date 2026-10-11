package install

// Pinned release versions (ADR 0017, 1.3). Every download is checked against
// the SHA-256 of the published asset before any byte is written. The hashes
// come from the releases' own checksum files: containerd v2.4.1
// (containerd-2.4.1-linux-<arch>.tar.gz.sha256sum), containernetworking/plugins
// v1.9.1 (cni-plugins-linux-<arch>-v1.9.1.tgz.sha256) and opencontainers/runc
// v1.5.2 (runc.sha256sum). The CI workflows install the same containerd and
// CNI versions.
const (
	ContainerdVersion = "2.4.1"
	RuncVersion       = "1.5.2"
	CNIVersion        = "1.9.1"
)

// Asset is one downloadable file and its expected SHA-256 in lower-case hex.
type Asset struct {
	URL    string
	SHA256 string
}

var containerdAssets = map[string]Asset{
	"amd64": {
		URL:    "https://github.com/containerd/containerd/releases/download/v" + ContainerdVersion + "/containerd-" + ContainerdVersion + "-linux-amd64.tar.gz",
		SHA256: "d65eda6a188aac1006848d8060099be88ba9c4bcddbfd9a23a961194710d0dd4",
	},
	"arm64": {
		URL:    "https://github.com/containerd/containerd/releases/download/v" + ContainerdVersion + "/containerd-" + ContainerdVersion + "-linux-arm64.tar.gz",
		SHA256: "67f9b0a81c7140aaf15fe69053e88175270fadd33aeaffc1b9017123c1f55cea",
	},
}

var runcAssets = map[string]Asset{
	"amd64": {
		URL:    "https://github.com/opencontainers/runc/releases/download/v" + RuncVersion + "/runc.amd64",
		SHA256: "599f6f94ff8c5057241eff0d54c3c74f95c34935b6457b33fe545defc61e9488",
	},
	"arm64": {
		URL:    "https://github.com/opencontainers/runc/releases/download/v" + RuncVersion + "/runc.arm64",
		SHA256: "d10ecae898361832a059be2089bab92d158aec54661b18ed7346ed79628b46b0",
	},
}

var cniAssets = map[string]Asset{
	"amd64": {
		URL:    "https://github.com/containernetworking/plugins/releases/download/v" + CNIVersion + "/cni-plugins-linux-amd64-v" + CNIVersion + ".tgz",
		SHA256: "b98f74a0f8522f0a83867178729c1aa70f2158f90c45a2ca8fa791db1c76b303",
	},
	"arm64": {
		URL:    "https://github.com/containernetworking/plugins/releases/download/v" + CNIVersion + "/cni-plugins-linux-arm64-v" + CNIVersion + ".tgz",
		SHA256: "56171987d3947707c3563db2f4001bccaf50fd63468611b9f3cbecb1375ee7ec",
	},
}

// cniPlugins are the plugins the agent needs; their presence means CNI is in place.
var cniPlugins = []string{"bridge", "host-local", "portmap", "firewall", "loopback"}

func assetFor(m map[string]Asset, arch string) (Asset, error) {
	a, ok := m[arch]
	if !ok {
		return Asset{}, errUnsupportedArch(arch)
	}
	return a, nil
}
