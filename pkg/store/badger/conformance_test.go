package badger_test

import (
	"testing"

	"github.com/ckumar392/zonegit/pkg/store"
	badgerstore "github.com/ckumar392/zonegit/pkg/store/badger"
	"github.com/ckumar392/zonegit/pkg/store/storetest"
)

func TestConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Storage {
		dir := t.TempDir()
		s, err := badgerstore.Open(dir)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	})
}
