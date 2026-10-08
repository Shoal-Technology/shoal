package memory

import (
	"testing"

	"example.com/shoal/internal/store"
	"example.com/shoal/internal/store/storetest"
)

func TestConformance(t *testing.T) {
	storetest.Run(t, func(*testing.T) store.Store { return New(nil) })
}
