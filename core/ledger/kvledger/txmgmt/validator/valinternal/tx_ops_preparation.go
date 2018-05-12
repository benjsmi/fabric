/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package valinternal

import (
	"github.com/hyperledger/fabric/core/ledger/kvledger/txmgmt/privacyenabledstate"
	"github.com/hyperledger/fabric/core/ledger/kvledger/txmgmt/rwsetutil"
	"github.com/hyperledger/fabric/core/ledger/kvledger/txmgmt/statedb"
	"github.com/hyperledger/fabric/core/ledger/kvledger/txmgmt/storageutil"
	"github.com/hyperledger/fabric/core/ledger/kvledger/txmgmt/version"
	"github.com/hyperledger/fabric/protos/ledger/rwset/kvrwset"
)

func prepareTxOps(rwset *rwsetutil.TxRwSet, txht *version.Height,
	precedingUpdates *PubAndHashUpdates, db privacyenabledstate.DB) (txOps, error) {
	txops := txOps{}
	txops.applyTxRwset(rwset)
	//logger.Debugf("prepareTxOps() txops after applying raw rwset=%#v", spew.Sdump(txops))
	for ck, keyop := range txops {
		// check if the final state of the key, value and metadata, is already present in the transaction, then skip
		// otherwise we need to retrieve latest state and merge in the current value or metadata update
		if keyop.isDelete() || keyop.isUpsertAndMetadataUpdate() {
			continue
		}

		latestVal, err := retrieveLatestState(ck.ns, ck.coll, ck.key, precedingUpdates, db)
		if err != nil {
			return nil, err
		}

		// check if only value is updated in the current transaction then merge the metadata from last committed state
		if keyop.isOnlyUpsert() {
			if latestVal != nil {
				keyop.metadata = latestVal.Metadata
			}
			continue
		}

		// only metadata is updated in the current transaction. Merge the value from the last committed state
		// If the key does not exist in the last state, make this key as noop in current transaction
		if latestVal != nil {
			keyop.value = latestVal.Value
		} else {
			delete(txops, ck)
		}
	}
	//logger.Debugf("prepareTxOps() txops after final processing=%#v", spew.Sdump(txops))
	return txops, nil
}

// applyTxRwset records the upsertion/deletion of a kv and updatation/deletion
// of asociated metadata present in a txrwset
func (txops txOps) applyTxRwset(rwset *rwsetutil.TxRwSet) error {
	for _, nsRWSet := range rwset.NsRwSets {
		ns := nsRWSet.NameSpace
		for _, kvWrite := range nsRWSet.KvRwSet.Writes {
			txops.applyKVWrite(ns, "", kvWrite)
		}
		for _, kvMetadataWrite := range nsRWSet.KvRwSet.MetadataWrites {
			txops.applyMetadata(ns, "", kvMetadataWrite)
		}

		// apply collection level kvwrite and kvMetadataWrite
		for _, collHashRWset := range nsRWSet.CollHashedRwSets {
			coll := collHashRWset.CollectionName
			for _, hashedWrite := range collHashRWset.HashedRwSet.HashedWrites {
				txops.applyKVWrite(ns, coll,
					&kvrwset.KVWrite{
						Key:      string(hashedWrite.KeyHash),
						Value:    hashedWrite.ValueHash,
						IsDelete: hashedWrite.IsDelete,
					},
				)
			}

			for _, metadataWrite := range collHashRWset.HashedRwSet.MetadataWrites {
				txops.applyMetadata(ns, coll,
					&kvrwset.KVMetadataWrite{
						Key:     string(metadataWrite.KeyHash),
						Entries: metadataWrite.Entries,
					},
				)
			}
		}
	}
	return nil
}

// applyKVWrite records upsertion/deletion of a kvwrite
func (txops txOps) applyKVWrite(ns, coll string, kvWrite *kvrwset.KVWrite) {
	if kvWrite.IsDelete {
		txops.delete(compositeKey{ns, coll, kvWrite.Key})
	} else {
		txops.upsert(compositeKey{ns, coll, kvWrite.Key}, kvWrite.Value)
	}
}

// applyMetadata records updatation/deletion of a metadataWrite
func (txops txOps) applyMetadata(ns, coll string, metadataWrite *kvrwset.KVMetadataWrite) error {
	if metadataWrite.Entries == nil {
		txops.metadataDelete(compositeKey{ns, coll, metadataWrite.Key})
	} else {
		metadataBytes, err := storageutil.SerializeMetadata(metadataWrite.Entries)
		if err != nil {
			return err
		}
		txops.metadataUpdate(compositeKey{ns, coll, metadataWrite.Key}, metadataBytes)
	}
	return nil
}

// retrieveLatestState returns the value of the key from the precedingUpdates (if the key was operated upon by a previous tran in the block).
// If the key not present in the precedingUpdates, then this function, pulls the latest value from statedb
// TODO FAB-11328, pulling from state for (especially for couchdb) will pay significant performance penalty so a bulkload would be helpful.
// Further, all the keys that gets written will be required to pull from statedb by vscc for endorsement policy check (in the case of key level
// endorsement) and hence, the bulkload should be combined
func retrieveLatestState(ns, coll, key string,
	precedingUpdates *PubAndHashUpdates, db privacyenabledstate.DB) (*statedb.VersionedValue, error) {
	var vv *statedb.VersionedValue
	var err error
	if coll == "" {
		vv := precedingUpdates.PubUpdates.Get(ns, key)
		if vv == nil {
			vv, err = db.GetState(ns, key)
		}
		return vv, err
	}

	vv = precedingUpdates.HashUpdates.Get(ns, coll, key)
	if vv == nil {
		vv, err = db.GetValueHash(ns, coll, []byte(key))
	}
	return vv, err
}
