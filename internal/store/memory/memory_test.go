package memory

import (
	"testing"

	"github.com/kaiserern/shoal/internal/store"
	"github.com/kaiserern/shoal/internal/store/storetest"
)

func TestConformance(t *testing.T) {
	storetest.Run(t, func(*testing.T) store.Store { return New(nil) })
}
