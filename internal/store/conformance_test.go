package store_test

import (
	"testing"

	"github.com/jkreileder/titelheld/internal/store"
	"github.com/jkreileder/titelheld/internal/store/storetest"
)

// The in-memory store is held to the same conformance suite as Firestore.
func TestMemoryConformance(t *testing.T) {
	t.Parallel()

	storetest.Suite(t, func(*testing.T) store.Store {
		return store.NewMemory()
	})
}
