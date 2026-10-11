package containerd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/core/runtime/v2/logging"

	"github.com/Shoal-Technology/shoal/internal/runtime/logfile"
)

// RunLogDriver runs this process as a containerd binary logging driver that
// writes the container's stdout/stderr to path. It never returns.
//
// The shim starts it per task with the output pipes as fds 3 and 4, so logs
// keep flowing even while the agent itself is down or restarting.
func RunLogDriver(path string) {
	logging.Run(func(_ context.Context, cfg *logging.Config, ready func() error) error {
		w, err := logfile.OpenWriter(path, LogMaxSize, LogMaxFiles)
		if err != nil {
			return fmt.Errorf("log driver for %s: %w", cfg.ID, err)
		}
		defer w.Close()
		if err := ready(); err != nil {
			return fmt.Errorf("log driver for %s: signal ready: %w", cfg.ID, err)
		}

		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i, s := range []struct {
			name string
			r    io.Reader
		}{{"stdout", cfg.Stdout}, {"stderr", cfg.Stderr}} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[i] = logfile.Copy(w, s.r, s.name, time.Now)
			}()
		}
		wg.Wait()
		if err := errors.Join(errs...); err != nil {
			fmt.Fprintf(os.Stderr, "log driver for %s: %v\n", cfg.ID, err)
			return err
		}
		return nil
	})
}
