package state

import (
	"fmt"

	"github.com/dgraph-io/badger"
)

// badgerStore implements IndexedStore.
type badgerStore struct {
	opts *storeOptions
	db   *badger.DB
}

func newBadgerStore(prefix string, opts ...storeOpt) (*badgerStore, error) {
	s := &badgerStore{
		opts: defaultStoreOptions(),
	}
	for _, o := range opts {
		if o != nil {
			o(s.opts)
		}
	}
	badgerOpts := badger.DefaultOptions
	badgerOpts.Dir = prefix
	badgerOpts.ValueDir = prefix
	badgerOpts.SyncWrites = s.opts.SyncWrites
	db, err := badger.Open(badgerOpts)
	if err != nil {
		return nil, err
	}
	s.db = db
	return s, nil
}

func (s *badgerStore) View(k *Key, fn PeekFunc) error {
	return s.db.View(func(tx *badger.Txn) error {
		v, err := tx.Get(k.Bytes())
		if err == badger.ErrKeyNotFound {
			return ErrNotFound
		} else if err != nil {
			err = fmt.Errorf("item get error: %v", err)
			return err
		}
		vv, err := v.Value()
		if err != nil {
			err = fmt.Errorf("value read error: %v", err)
			return err
		}
		return fn(k, vv)
	})
}

func (s *badgerStore) Update(k *Key, fn ModifyFunc) error {
	return s.db.Update(func(tx *badger.Txn) error {
		if fn == nil {
			return nil
		}
		key := k.Bytes()
		v, err := tx.Get(key)
		if err == badger.ErrKeyNotFound {
			vv, err := fn(k, nil)
			if err == ErrNoUpdate {
				return nil
			} else if err != nil {
				return err
			}
			if k.TTL > 0 {
				return tx.SetWithTTL(key, vv, k.TTL)
			}
			return tx.Set(key, vv)
		} else if err != nil {
			err = fmt.Errorf("item set error: %v", err)
			return err
		}
		vv, err := v.ValueCopy(nil)
		if err != nil {
			return err
		}
		vv, err = fn(k, vv)
		if err == ErrNoUpdate {
			return nil
		} else if err != nil {
			return err
		}
		if k.TTL > 0 {
			return tx.SetWithTTL(key, vv, k.TTL)
		}
		return tx.Set(key, vv)
	})
}

func (s *badgerStore) RangeKeys(b Bucket, fn KeyFunc) (*RangeOptions, error) {
	var opt *RangeOptions
	err := s.db.View(func(tx *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchSize = 10
		opts.PrefetchValues = false
		it := tx.NewIterator(opts)
		defer it.Close()

		for it.Seek(b.NewKey(nil).Bytes()); it.Valid(); it.Next() {
			item := it.Item()
			k := (&Key{}).Unmarshal(item.Key())
			if k.Bucket.ID != b.ID {
				return nil
			}
			fn(k)
		}
		return nil
	})
	return opt, err
}

func (s *badgerStore) RangePeek(b Bucket, fn PeekFunc) (*RangeOptions, error) {
	var opt *RangeOptions
	err := s.db.View(func(tx *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchSize = 10
		it := tx.NewIterator(opts)
		defer it.Close()

		for it.Seek(b.NewKey(nil).Bytes()); it.Valid(); it.Next() {
			item := it.Item()
			k := (&Key{}).Unmarshal(item.Key())
			if k.Bucket.ID != b.ID {
				return nil
			}
			v, err := it.Item().Value()
			if err != nil {
				return err
			}
			if err := fn(k, v); err == ErrRangeStop {
				return nil
			} else if err != nil {
				return err
			}
		}
		return nil
	})
	return opt, err
}

func (s *badgerStore) RangeModify(b Bucket, fn ModifyFunc) (*RangeOptions, error) {
	var opt *RangeOptions
	err := s.db.Update(func(tx *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchSize = 10
		it := tx.NewIterator(opts)
		defer it.Close()

		for it.Seek(b.NewKey(nil).Bytes()); it.Valid(); it.Next() {
			item := it.Item()
			k := (&Key{}).Unmarshal(item.Key())
			if k.Bucket.ID != b.ID {
				return nil
			}
			v, err := it.Item().Value()
			if err != nil {
				return err
			}
			vv, err := fn(k, v)
			if err == ErrNoUpdate {
				continue
			} else if err != nil && err != ErrRangeStop {
				return err
			} else if err := tx.Set(item.Key(), vv); err != nil {
				return err
			}
			if err == ErrRangeStop {
				return nil
			}
		}
		return nil
	})
	return opt, err
}

func (s *badgerStore) Delete(k *Key) error {
	if k == nil {
		return nil
	}
	return s.db.View(func(tx *badger.Txn) error {
		if err := tx.Delete(k.Bytes()); err == badger.ErrKeyNotFound {
			return nil
		} else if err != nil {
			return err
		}
		return nil
	})
}

func (s *badgerStore) Close() error {
	return s.db.Close()
}
