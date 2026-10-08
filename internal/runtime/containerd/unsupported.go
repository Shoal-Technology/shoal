//go:build !linux

package containerd

import (
	"context"
	"errors"
	"fmt"
	"os"
)

var errUnsupported = errors.New("the containerd runtime is only supported on linux")

// New is unavailable on this platform.
func New(context.Context, Options) (Client, error) { return nil, errUnsupported }

// RunLogDriver is unavailable on this platform.
func RunLogDriver(string) {
	fmt.Fprintln(os.Stderr, errUnsupported)
	os.Exit(1)
}
