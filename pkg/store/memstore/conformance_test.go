package memstore_test

import (
	"testing"

	"github.com/ckumar392/dnsdb/pkg/store"
	"github.com/ckumar392/dnsdb/pkg/store/memstore"
	"github.com/ckumar392/dnsdb/pkg/store/storetest"
)

func TestConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Storage {
		return memstore.New()
	})
}
