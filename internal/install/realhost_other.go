//go:build !linux

package install

import "errors"

// NewLocalHost is only implemented on linux: shoal init sets up a Linux node.
func NewLocalHost(root string) (Host, error) {
	return nil, errors.New("shoal init runs on linux only")
}
