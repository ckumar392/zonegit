package memstore_test

import (
	"testing"

	"github.com/ckumar392/zonegit/pkg/store"
	"github.com/ckumar392/zonegit/pkg/store/memstore"
	"github.com/ckumar392/zonegit/pkg/store/storetest"
)

func TestConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Storage {
		return memstore.New()
	})
}
