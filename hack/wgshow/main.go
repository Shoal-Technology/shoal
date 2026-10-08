// wgshow prints a WireGuard interface's peers (test helper; `wg` is not
// installed on the test VM).
package main

import (
	"fmt"
	"os"

	"golang.zx2c4.com/wireguard/wgctrl"
)

func main() {
	c, err := wgctrl.New()
	if err != nil {
		fmt.Println("wgctrl:", err)
		os.Exit(1)
	}
	defer c.Close()
	for _, name := range os.Args[1:] {
		d, err := c.Device(name)
		if err != nil {
			fmt.Println(name, "error:", err)
			continue
		}
		fmt.Printf("%s: port %d, public key %s..., %d peer(s)\n", name, d.ListenPort, d.PublicKey.String()[:8], len(d.Peers))
		for _, p := range d.Peers {
			fmt.Printf("  peer %s... endpoint %v allowed %v\n", p.PublicKey.String()[:8], p.Endpoint, p.AllowedIPs)
		}
	}
}
