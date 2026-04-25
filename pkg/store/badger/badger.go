package badger

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	badgerdb "github.com/dgraph-io/badger/v4"

	"github.com/ckumar392/zonegit/pkg/store"
)

// Key prefixes for namespace separation within BadgerDB.
var (
	prefixObj    = []byte("o/") // o/<hash> -> kind\x00payload
	prefixRef    = []byte("r/") // r/<name> -> hash
	prefixReflog = []byte("l/") // l/<name>/<seqno-big-endian> -> json(ReflogEntry)
)

// Store is a BadgerDB-backed store.Storage.
type Store struct {
	db *badgerdb.DB
}

// Compile-time interface check.
var _ store.Storage = (*Store)(nil)

// Open opens (or creates) a BadgerDB at the given directory path and
// returns a Store wrapping it. Call Close() when done.
func Open(dir string) (*Store, error) {
	opts := badgerdb.DefaultOptions(dir).
		WithLogger(nil) // silence badger's default logger
	db, err := badgerdb.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("badger open %s: %w", dir, err)
	}
	return &Store{db: db}, nil
}

// OpenReadOnly opens an existing BadgerDB at dir for read-only access. It
// does not acquire the directory lock, so multiple readers (and one
// concurrent writer) can coexist. Mutating methods on the returned Store
// will return an error from BadgerDB.
func OpenReadOnly(dir string) (*Store, error) {
	opts := badgerdb.DefaultOptions(dir).
		WithLogger(nil).
		WithReadOnly(true).
		WithBypassLockGuard(true)
	db, err := badgerdb.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("badger open %s (ro): %w", dir, err)
	}
	return &Store{db: db}, nil
}

// --- Object CAS ---

func objKey(h store.Hash) []byte {
	k := make([]byte, len(prefixObj)+store.HashSize)
	copy(k, prefixObj)
	copy(k[len(prefixObj):], h[:])
	return k
}

func encodeObj(o store.Object) []byte {
	// kind\x00payload
	buf := make([]byte, 0, len(o.Kind)+1+len(o.Payload))
	buf = append(buf, o.Kind...)
	buf = append(buf, 0)
	buf = append(buf, o.Payload...)
	return buf
}

func decodeObj(val []byte) (store.Object, error) {
	idx := bytes.IndexByte(val, 0)
	if idx < 0 {
		return store.Object{}, fmt.Errorf("badger: malformed object value: %w", store.ErrCorrupt)
	}
	return store.Object{
		Kind:    string(val[:idx]),
		Payload: append([]byte(nil), val[idx+1:]...),
	}, nil
}

func (s *Store) PutObject(_ context.Context, h store.Hash, o store.Object) error {
	key := objKey(h)
	val := encodeObj(o)
	return s.db.Update(func(txn *badgerdb.Txn) error {
		return txn.Set(key, val)
	})
}

func (s *Store) GetObject(_ context.Context, h store.Hash) (store.Object, error) {
	key := objKey(h)
	var obj store.Object
	err := s.db.View(func(txn *badgerdb.Txn) error {
		item, err := txn.Get(key)
		if err == badgerdb.ErrKeyNotFound {
			return fmt.Errorf("object %s: %w", h.Short(), store.ErrNotFound)
		}
		if err != nil {
			return err
		}
		return item.Value(func(val []byte) error {
			obj, err = decodeObj(val)
			return err
		})
	})
	return obj, err
}

func (s *Store) HasObject(_ context.Context, h store.Hash) (bool, error) {
	key := objKey(h)
	err := s.db.View(func(txn *badgerdb.Txn) error {
		_, err := txn.Get(key)
		return err
	})
	if err == badgerdb.ErrKeyNotFound {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) IterObjects(_ context.Context, fn func(store.Hash, store.Object) error) error {
	return s.db.View(func(txn *badgerdb.Txn) error {
		opts := badgerdb.DefaultIteratorOptions
		opts.Prefix = prefixObj
		it := txn.NewIterator(opts)
		defer it.Close()
		for it.Rewind(); it.Valid(); it.Next() {
			item := it.Item()
			key := item.Key()
			if len(key) != len(prefixObj)+store.HashSize {
				continue
			}
			var h store.Hash
			copy(h[:], key[len(prefixObj):])
			err := item.Value(func(val []byte) error {
				obj, err := decodeObj(val)
				if err != nil {
					return err
				}
				return fn(h, obj)
			})
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// --- Refs ---

func refKey(name string) []byte {
	return append(append([]byte(nil), prefixRef...), name...)
}

func (s *Store) GetRef(_ context.Context, name string) (store.Hash, bool, error) {
	key := refKey(name)
	var h store.Hash
	err := s.db.View(func(txn *badgerdb.Txn) error {
		item, err := txn.Get(key)
		if err == badgerdb.ErrKeyNotFound {
			return nil // ok=false handled below
		}
		if err != nil {
			return err
		}
		return item.Value(func(val []byte) error {
			if len(val) != store.HashSize {
				return fmt.Errorf("badger: ref %s: bad hash length %d", name, len(val))
			}
			copy(h[:], val)
			return nil
		})
	})
	if err != nil {
		return h, false, err
	}
	if h.IsZero() {
		// Could be genuinely absent. Check again.
		var found bool
		_ = s.db.View(func(txn *badgerdb.Txn) error {
			_, err := txn.Get(key)
			found = err == nil
			return nil
		})
		return h, found, nil
	}
	return h, true, nil
}

func (s *Store) CASRef(_ context.Context, name string, expected, next store.Hash) error {
	if next.IsZero() {
		return fmt.Errorf("CASRef: next hash is zero (use DeleteRef): %w", store.ErrInvalidObject)
	}
	key := refKey(name)
	return s.db.Update(func(txn *badgerdb.Txn) error {
		item, err := txn.Get(key)
		switch {
		case err == badgerdb.ErrKeyNotFound && expected.IsZero():
			// create
		case err == badgerdb.ErrKeyNotFound:
			return fmt.Errorf("CASRef %s: %w", name, store.ErrConflict)
		case err != nil:
			return err
		default:
			var cur store.Hash
			if err := item.Value(func(val []byte) error {
				copy(cur[:], val)
				return nil
			}); err != nil {
				return err
			}
			if cur != expected {
				return fmt.Errorf("CASRef %s: %w", name, store.ErrConflict)
			}
		}
		return txn.Set(key, next[:])
	})
}

func (s *Store) DeleteRef(_ context.Context, name string, expected store.Hash) error {
	key := refKey(name)
	return s.db.Update(func(txn *badgerdb.Txn) error {
		item, err := txn.Get(key)
		if err == badgerdb.ErrKeyNotFound {
			return fmt.Errorf("DeleteRef %s: %w", name, store.ErrNotFound)
		}
		if err != nil {
			return err
		}
		var cur store.Hash
		if err := item.Value(func(val []byte) error {
			copy(cur[:], val)
			return nil
		}); err != nil {
			return err
		}
		if cur != expected {
			return fmt.Errorf("DeleteRef %s: %w", name, store.ErrConflict)
		}
		return txn.Delete(key)
	})
}

func (s *Store) ListRefs(_ context.Context, prefix string) ([]store.RefEntry, error) {
	scanPrefix := refKey(prefix)
	var out []store.RefEntry
	err := s.db.View(func(txn *badgerdb.Txn) error {
		opts := badgerdb.DefaultIteratorOptions
		opts.Prefix = scanPrefix
		it := txn.NewIterator(opts)
		defer it.Close()
		for it.Rewind(); it.Valid(); it.Next() {
			item := it.Item()
			name := strings.TrimPrefix(string(item.Key()), string(prefixRef))
			var h store.Hash
			if err := item.Value(func(val []byte) error {
				copy(h[:], val)
				return nil
			}); err != nil {
				return err
			}
			out = append(out, store.RefEntry{Name: name, Hash: h})
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, err
}

// --- Reflog ---

func reflogKeyPrefix(name string) []byte {
	return append(append([]byte(nil), prefixReflog...), name+"/"...)
}

func reflogKey(name string, seq uint64) []byte {
	prefix := reflogKeyPrefix(name)
	buf := make([]byte, len(prefix)+8)
	copy(buf, prefix)
	binary.BigEndian.PutUint64(buf[len(prefix):], seq)
	return buf
}

func (s *Store) AppendReflog(_ context.Context, name string, e store.ReflogEntry) error {
	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("reflog marshal: %w", err)
	}
	return s.db.Update(func(txn *badgerdb.Txn) error {
		// Count existing entries to get next sequence.
		prefix := reflogKeyPrefix(name)
		opts := badgerdb.DefaultIteratorOptions
		opts.Prefix = prefix
		opts.PrefetchValues = false
		it := txn.NewIterator(opts)
		var count uint64
		for it.Rewind(); it.Valid(); it.Next() {
			count++
		}
		it.Close()
		return txn.Set(reflogKey(name, count), data)
	})
}

func (s *Store) ReadReflog(_ context.Context, name string) ([]store.ReflogEntry, error) {
	prefix := reflogKeyPrefix(name)
	var out []store.ReflogEntry
	err := s.db.View(func(txn *badgerdb.Txn) error {
		opts := badgerdb.DefaultIteratorOptions
		opts.Prefix = prefix
		it := txn.NewIterator(opts)
		defer it.Close()
		for it.Rewind(); it.Valid(); it.Next() {
			item := it.Item()
			if err := item.Value(func(val []byte) error {
				var e store.ReflogEntry
				if err := json.Unmarshal(val, &e); err != nil {
					return err
				}
				out = append(out, e)
				return nil
			}); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

func (s *Store) Close() error {
	return s.db.Close()
}
