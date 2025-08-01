// Copyright 2023 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package blobpool

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/sha256"
	"errors"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/misc/eip1559"
	"github.com/ethereum/go-ethereum/consensus/misc/eip4844"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/txpool"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/crypto/kzg4844"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/holiman/billy"
	"github.com/holiman/uint256"
)

var (
	testBlobs       []*kzg4844.Blob
	testBlobCommits []kzg4844.Commitment
	testBlobProofs  []kzg4844.Proof
	testBlobVHashes [][32]byte
)

const testMaxBlobsPerBlock = 6

func init() {
	for i := 0; i < 24; i++ {
		testBlob := &kzg4844.Blob{byte(i)}
		testBlobs = append(testBlobs, testBlob)

		testBlobCommit, _ := kzg4844.BlobToCommitment(testBlob)
		testBlobCommits = append(testBlobCommits, testBlobCommit)

		testBlobProof, _ := kzg4844.ComputeBlobProof(testBlob, testBlobCommit)
		testBlobProofs = append(testBlobProofs, testBlobProof)

		testBlobVHash := kzg4844.CalcBlobHashV1(sha256.New(), &testBlobCommit)
		testBlobVHashes = append(testBlobVHashes, testBlobVHash)
	}
}

// testBlockChain is a mock of the live chain for testing the pool.
type testBlockChain struct {
	config  *params.ChainConfig
	basefee *uint256.Int
	blobfee *uint256.Int
	statedb *state.StateDB

	blocks map[uint64]*types.Block
}

func (bc *testBlockChain) Config() *params.ChainConfig {
	return bc.config
}

func (bc *testBlockChain) CurrentBlock() *types.Header {
	// Yolo, life is too short to invert misc.CalcBaseFee and misc.CalcBlobFee,
	// just binary search it them.

	// The base fee at 5714 ETH translates into the 21000 base gas higher than
	// mainnet ether existence, use that as a cap for the tests.
	var (
		blockNumber = new(big.Int).Add(bc.config.LondonBlock, big.NewInt(1))
		blockTime   = *bc.config.CancunTime + 1
		gasLimit    = uint64(30_000_000)
	)
	lo := new(big.Int)
	hi := new(big.Int).Mul(big.NewInt(5714), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil))

	for new(big.Int).Add(lo, big.NewInt(1)).Cmp(hi) != 0 {
		mid := new(big.Int).Add(lo, hi)
		mid.Div(mid, big.NewInt(2))

		if eip1559.CalcBaseFee(bc.config, &types.Header{
			Number:   blockNumber,
			GasLimit: gasLimit,
			GasUsed:  0,
			BaseFee:  mid,
		}).Cmp(bc.basefee.ToBig()) > 0 {
			hi = mid
		} else {
			lo = mid
		}
	}
	baseFee := lo

	// The excess blob gas at 2^27 translates into a blob fee higher than mainnet
	// ether existence, use that as a cap for the tests.
	lo = new(big.Int)
	hi = new(big.Int).Exp(big.NewInt(2), big.NewInt(27), nil)

	for new(big.Int).Add(lo, big.NewInt(1)).Cmp(hi) != 0 {
		mid := new(big.Int).Add(lo, hi)
		mid.Div(mid, big.NewInt(2))

		tmp := mid.Uint64()
		if eip4844.CalcBlobFee(bc.Config(), &types.Header{
			Number:        blockNumber,
			Time:          blockTime,
			ExcessBlobGas: &tmp,
		}).Cmp(bc.blobfee.ToBig()) > 0 {
			hi = mid
		} else {
			lo = mid
		}
	}
	excessBlobGas := lo.Uint64()

	return &types.Header{
		Number:        blockNumber,
		Time:          blockTime,
		GasLimit:      gasLimit,
		BaseFee:       baseFee,
		ExcessBlobGas: &excessBlobGas,
		Difficulty:    common.Big0,
	}
}

func (bc *testBlockChain) CurrentFinalBlock() *types.Header {
	return &types.Header{
		Number: big.NewInt(0),
	}
}

func (bc *testBlockChain) GetBlock(hash common.Hash, number uint64) *types.Block {
	// This is very yolo for the tests. If the number is the origin block we use
	// to init the pool, return an empty block with the correct starting header.
	//
	// If it is something else, return some baked in mock data.
	if number == bc.config.LondonBlock.Uint64()+1 {
		return types.NewBlockWithHeader(bc.CurrentBlock())
	}
	return bc.blocks[number]
}

func (bc *testBlockChain) StateAt(common.Hash) (*state.StateDB, error) {
	return bc.statedb, nil
}

// makeAddressReserver is a utility method to sanity check that accounts are
// properly reserved by the blobpool (no duplicate reserves or unreserves).
func makeAddressReserver() txpool.AddressReserver {
	var (
		reserved = make(map[common.Address]struct{})
		lock     sync.Mutex
	)
	return func(addr common.Address, reserve bool) error {
		lock.Lock()
		defer lock.Unlock()

		_, exists := reserved[addr]
		if reserve {
			if exists {
				panic("already reserved")
			}
			reserved[addr] = struct{}{}
			return nil
		}
		if !exists {
			panic("not reserved")
		}
		delete(reserved, addr)
		return nil
	}
}

// makeTx is a utility method to construct a random blob transaction and sign it
// with a valid key, only setting the interesting fields from the perspective of
// the blob pool.
func makeTx(nonce uint64, gasTipCap uint64, gasFeeCap uint64, blobFeeCap uint64, key *ecdsa.PrivateKey) *types.Transaction {
	blobtx := makeUnsignedTx(nonce, gasTipCap, gasFeeCap, blobFeeCap)
	return types.MustSignNewTx(key, types.LatestSigner(params.MainnetChainConfig), blobtx)
}

// makeMultiBlobTx is a utility method to construct a ramdom blob tx with
// certain number of blobs in its sidecar.
func makeMultiBlobTx(nonce uint64, gasTipCap uint64, gasFeeCap uint64, blobFeeCap uint64, blobCount int, key *ecdsa.PrivateKey) *types.Transaction {
	var (
		blobs       []kzg4844.Blob
		blobHashes  []common.Hash
		commitments []kzg4844.Commitment
		proofs      []kzg4844.Proof
	)
	for i := 0; i < blobCount; i++ {
		blobs = append(blobs, *testBlobs[i])
		commitments = append(commitments, testBlobCommits[i])
		proofs = append(proofs, testBlobProofs[i])
		blobHashes = append(blobHashes, testBlobVHashes[i])
	}
	blobtx := &types.BlobTx{
		ChainID:    uint256.MustFromBig(params.MainnetChainConfig.ChainID),
		Nonce:      nonce,
		GasTipCap:  uint256.NewInt(gasTipCap),
		GasFeeCap:  uint256.NewInt(gasFeeCap),
		Gas:        21000,
		BlobFeeCap: uint256.NewInt(blobFeeCap),
		BlobHashes: blobHashes,
		Value:      uint256.NewInt(100),
		Sidecar: &types.BlobTxSidecar{
			Blobs:       blobs,
			Commitments: commitments,
			Proofs:      proofs,
		},
	}
	return types.MustSignNewTx(key, types.LatestSigner(params.MainnetChainConfig), blobtx)
}

// makeUnsignedTx is a utility method to construct a random blob transaction
// without signing it.
func makeUnsignedTx(nonce uint64, gasTipCap uint64, gasFeeCap uint64, blobFeeCap uint64) *types.BlobTx {
	return makeUnsignedTxWithTestBlob(nonce, gasTipCap, gasFeeCap, blobFeeCap, rnd.Intn(len(testBlobs)))
}

// makeUnsignedTxWithTestBlob is a utility method to construct a random blob transaction
// without signing it.
func makeUnsignedTxWithTestBlob(nonce uint64, gasTipCap uint64, gasFeeCap uint64, blobFeeCap uint64, blobIdx int) *types.BlobTx {
	return &types.BlobTx{
		ChainID:    uint256.MustFromBig(params.MainnetChainConfig.ChainID),
		Nonce:      nonce,
		GasTipCap:  uint256.NewInt(gasTipCap),
		GasFeeCap:  uint256.NewInt(gasFeeCap),
		Gas:        21000,
		BlobFeeCap: uint256.NewInt(blobFeeCap),
		BlobHashes: []common.Hash{testBlobVHashes[blobIdx]},
		Value:      uint256.NewInt(100),
		Sidecar: &types.BlobTxSidecar{
			Blobs:       []kzg4844.Blob{*testBlobs[blobIdx]},
			Commitments: []kzg4844.Commitment{testBlobCommits[blobIdx]},
			Proofs:      []kzg4844.Proof{testBlobProofs[blobIdx]},
		},
	}
}

// verifyPoolInternals iterates over all the transactions in the pool and checks
// that sort orders, calculated fields, cumulated fields are correct.
func verifyPoolInternals(t *testing.T, pool *BlobPool) {
	// Mark this method as a helper to remove from stack traces
	t.Helper()

	// Verify that all items in the index are present in the tx lookup and nothing more
	seen := make(map[common.Hash]struct{})
	for addr, txs := range pool.index {
		for _, tx := range txs {
			if _, ok := seen[tx.hash]; ok {
				t.Errorf("duplicate hash #%x in transaction index: address %s, nonce %d", tx.hash, addr, tx.nonce)
			}
			seen[tx.hash] = struct{}{}
		}
	}
	for hash, id := range pool.lookup.txIndex {
		if _, ok := seen[hash]; !ok {
			t.Errorf("tx lookup entry missing from transaction index: hash #%x, id %d", hash, id)
		}
		delete(seen, hash)
	}
	for hash := range seen {
		t.Errorf("indexed transaction hash #%x missing from tx lookup table", hash)
	}
	// Verify that all blobs in the index are present in the blob lookup and nothing more
	blobs := make(map[common.Hash]map[common.Hash]struct{})
	for _, txs := range pool.index {
		for _, tx := range txs {
			for _, vhash := range tx.vhashes {
				if blobs[vhash] == nil {
					blobs[vhash] = make(map[common.Hash]struct{})
				}
				blobs[vhash][tx.hash] = struct{}{}
			}
		}
	}
	for vhash, txs := range pool.lookup.blobIndex {
		for txhash := range txs {
			if _, ok := blobs[vhash][txhash]; !ok {
				t.Errorf("blob lookup entry missing from transaction index: blob hash #%x, tx hash #%x", vhash, txhash)
			}
			delete(blobs[vhash], txhash)
			if len(blobs[vhash]) == 0 {
				delete(blobs, vhash)
			}
		}
	}
	for vhash := range blobs {
		t.Errorf("indexed transaction blob hash #%x missing from blob lookup table", vhash)
	}
	// Verify that transactions are sorted per account and contain no nonce gaps,
	// and that the first nonce is the next expected one based on the state.
	for addr, txs := range pool.index {
		for i := 1; i < len(txs); i++ {
			if txs[i].nonce != txs[i-1].nonce+1 {
				t.Errorf("addr %v, tx %d nonce mismatch: have %d, want %d", addr, i, txs[i].nonce, txs[i-1].nonce+1)
			}
		}
		if txs[0].nonce != pool.state.GetNonce(addr) {
			t.Errorf("addr %v, first tx nonce mismatch: have %d, want %d", addr, txs[0].nonce, pool.state.GetNonce(addr))
		}
	}
	// Verify that calculated evacuation thresholds are correct
	for addr, txs := range pool.index {
		if !txs[0].evictionExecTip.Eq(txs[0].execTipCap) {
			t.Errorf("addr %v, tx %d eviction execution tip mismatch: have %d, want %d", addr, 0, txs[0].evictionExecTip, txs[0].execTipCap)
		}
		if math.Abs(txs[0].evictionExecFeeJumps-txs[0].basefeeJumps) > 0.001 {
			t.Errorf("addr %v, tx %d eviction execution fee jumps mismatch: have %f, want %f", addr, 0, txs[0].evictionExecFeeJumps, txs[0].basefeeJumps)
		}
		if math.Abs(txs[0].evictionBlobFeeJumps-txs[0].blobfeeJumps) > 0.001 {
			t.Errorf("addr %v, tx %d eviction blob fee jumps mismatch: have %f, want %f", addr, 0, txs[0].evictionBlobFeeJumps, txs[0].blobfeeJumps)
		}
		for i := 1; i < len(txs); i++ {
			wantExecTip := txs[i-1].evictionExecTip
			if wantExecTip.Gt(txs[i].execTipCap) {
				wantExecTip = txs[i].execTipCap
			}
			if !txs[i].evictionExecTip.Eq(wantExecTip) {
				t.Errorf("addr %v, tx %d eviction execution tip mismatch: have %d, want %d", addr, i, txs[i].evictionExecTip, wantExecTip)
			}

			wantExecFeeJumps := txs[i-1].evictionExecFeeJumps
			if wantExecFeeJumps > txs[i].basefeeJumps {
				wantExecFeeJumps = txs[i].basefeeJumps
			}
			if math.Abs(txs[i].evictionExecFeeJumps-wantExecFeeJumps) > 0.001 {
				t.Errorf("addr %v, tx %d eviction execution fee jumps mismatch: have %f, want %f", addr, i, txs[i].evictionExecFeeJumps, wantExecFeeJumps)
			}

			wantBlobFeeJumps := txs[i-1].evictionBlobFeeJumps
			if wantBlobFeeJumps > txs[i].blobfeeJumps {
				wantBlobFeeJumps = txs[i].blobfeeJumps
			}
			if math.Abs(txs[i].evictionBlobFeeJumps-wantBlobFeeJumps) > 0.001 {
				t.Errorf("addr %v, tx %d eviction blob fee jumps mismatch: have %f, want %f", addr, i, txs[i].evictionBlobFeeJumps, wantBlobFeeJumps)
			}
		}
	}
	// Verify that account balance accumulations are correct
	for addr, txs := range pool.index {
		spent := new(uint256.Int)
		for _, tx := range txs {
			spent.Add(spent, tx.costCap)
		}
		if !pool.spent[addr].Eq(spent) {
			t.Errorf("addr %v expenditure mismatch: have %d, want %d", addr, pool.spent[addr], spent)
		}
	}
	// Verify that pool storage size is correct
	var stored uint64
	for _, txs := range pool.index {
		for _, tx := range txs {
			stored += uint64(tx.size)
		}
	}
	if pool.stored != stored {
		t.Errorf("pool storage mismatch: have %d, want %d", pool.stored, stored)
	}
	// Verify the price heap internals
	verifyHeapInternals(t, pool.evict)

	// Verify that all the blobs can be retrieved
	verifyBlobRetrievals(t, pool)
}

// verifyBlobRetrievals attempts to retrieve all testing blobs and checks that
// whatever is in the pool, it can be retrieved correctly.
func verifyBlobRetrievals(t *testing.T, pool *BlobPool) {
	// Collect all the blobs tracked by the pool
	known := make(map[common.Hash]struct{})
	for _, txs := range pool.index {
		for _, tx := range txs {
			for _, vhash := range tx.vhashes {
				known[vhash] = struct{}{}
			}
		}
	}
	// Attempt to retrieve all test blobs
	hashes := make([]common.Hash, len(testBlobVHashes))
	for i := range testBlobVHashes {
		copy(hashes[i][:], testBlobVHashes[i][:])
	}
	blobs, proofs := pool.GetBlobs(hashes)

	// Cross validate what we received vs what we wanted
	if len(blobs) != len(hashes) || len(proofs) != len(hashes) {
		t.Errorf("retrieved blobs/proofs size mismatch: have %d/%d, want %d", len(blobs), len(proofs), len(hashes))
		return
	}
	for i, hash := range hashes {
		// If an item is missing, but shouldn't, error
		if blobs[i] == nil || proofs[i] == nil {
			if _, ok := known[hash]; ok {
				t.Errorf("tracked blob retrieval failed: item %d, hash %x", i, hash)
			}
			continue
		}
		// Item retrieved, make sure it matches the expectation
		if *blobs[i] != *testBlobs[i] || *proofs[i] != testBlobProofs[i] {
			t.Errorf("retrieved blob or proof mismatch: item %d, hash %x", i, hash)
			continue
		}
		delete(known, hash)
	}
	for hash := range known {
		t.Errorf("indexed blob #%x missing from retrieval", hash)
	}
}

// Tests that transactions can be loaded from disk on startup and that they are
// correctly discarded if invalid.
//
//   - 1. A transaction that cannot be decoded must be dropped
//   - 2. A transaction that cannot be recovered (bad signature) must be dropped
//   - 3. All transactions after a nonce gap must be dropped
//   - 4. All transactions after an already included nonce must be dropped
//   - 5. All transactions after an underpriced one (including it) must be dropped
//   - 6. All transactions after an overdrafting sequence must be dropped
//   - 7. All transactions exceeding the per-account limit must be dropped
//
// Furthermore, some strange corner-cases can also occur after a crash, as Billy's
// simplicity also allows it to resurrect past deleted entities:
//
//   - 8. Fully duplicate transactions (matching hash) must be dropped
//   - 9. Duplicate nonces from the same account must be dropped
func TestOpenDrops(t *testing.T) {
	//log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(os.Stderr, log.LevelTrace, true)))

	// Create a temporary folder for the persistent backend
	storage, _ := os.MkdirTemp("", "blobpool-")
	defer os.RemoveAll(storage)

	os.MkdirAll(filepath.Join(storage, pendingTransactionStore), 0700)
	store, _ := billy.Open(billy.Options{Path: filepath.Join(storage, pendingTransactionStore)}, newSlotter(testMaxBlobsPerBlock), nil)

	// Insert a malformed transaction to verify that decoding errors (or format
	// changes) are handled gracefully (case 1)
	malformed, _ := store.Put([]byte("this is a badly encoded transaction"))

	// Insert a transaction with a bad signature to verify that stale junk after
	// potential hard-forks can get evicted (case 2)
	tx := types.NewTx(&types.BlobTx{
		ChainID:    uint256.MustFromBig(params.MainnetChainConfig.ChainID),
		GasTipCap:  new(uint256.Int),
		GasFeeCap:  new(uint256.Int),
		Gas:        0,
		Value:      new(uint256.Int),
		Data:       nil,
		BlobFeeCap: new(uint256.Int),
		V:          new(uint256.Int),
		R:          new(uint256.Int),
		S:          new(uint256.Int),
	})
	blob, _ := rlp.EncodeToBytes(tx)
	badsig, _ := store.Put(blob)

	// Insert a sequence of transactions with a nonce gap in between to verify
	// that anything gapped will get evicted (case 3).
	var (
		gapper, _ = crypto.GenerateKey()

		valids = make(map[uint64]struct{})
		gapped = make(map[uint64]struct{})
	)
	for _, nonce := range []uint64{0, 1, 3, 4, 6, 7} { // first gap at #2, another at #5
		tx := makeTx(nonce, 1, 1, 1, gapper)
		blob, _ := rlp.EncodeToBytes(tx)

		id, _ := store.Put(blob)
		if nonce < 2 {
			valids[id] = struct{}{}
		} else {
			gapped[id] = struct{}{}
		}
	}
	// Insert a sequence of transactions with a gapped starting nonce to verify
	// that the entire set will get dropped (case 3).
	var (
		dangler, _ = crypto.GenerateKey()
		dangling   = make(map[uint64]struct{})
	)
	for _, nonce := range []uint64{1, 2, 3} { // first gap at #0, all set dangling
		tx := makeTx(nonce, 1, 1, 1, dangler)
		blob, _ := rlp.EncodeToBytes(tx)

		id, _ := store.Put(blob)
		dangling[id] = struct{}{}
	}
	// Insert a sequence of transactions with already passed nonces to veirfy
	// that the entire set will get dropped (case 4).
	var (
		filler, _ = crypto.GenerateKey()
		filled    = make(map[uint64]struct{})
	)
	for _, nonce := range []uint64{0, 1, 2} { // account nonce at 3, all set filled
		tx := makeTx(nonce, 1, 1, 1, filler)
		blob, _ := rlp.EncodeToBytes(tx)

		id, _ := store.Put(blob)
		filled[id] = struct{}{}
	}
	// Insert a sequence of transactions with partially passed nonces to verify
	// that the included part of the set will get dropped (case 4).
	var (
		overlapper, _ = crypto.GenerateKey()
		overlapped    = make(map[uint64]struct{})
	)
	for _, nonce := range []uint64{0, 1, 2, 3} { // account nonce at 2, half filled
		tx := makeTx(nonce, 1, 1, 1, overlapper)
		blob, _ := rlp.EncodeToBytes(tx)

		id, _ := store.Put(blob)
		if nonce >= 2 {
			valids[id] = struct{}{}
		} else {
			overlapped[id] = struct{}{}
		}
	}
	// Insert a sequence of transactions with an underpriced first to verify that
	// the entire set will get dropped (case 5).
	var (
		underpayer, _ = crypto.GenerateKey()
		underpaid     = make(map[uint64]struct{})
	)
	for i := 0; i < 5; i++ { // make #0 underpriced
		var tx *types.Transaction
		if i == 0 {
			tx = makeTx(uint64(i), 0, 0, 0, underpayer)
		} else {
			tx = makeTx(uint64(i), 1, 1, 1, underpayer)
		}
		blob, _ := rlp.EncodeToBytes(tx)

		id, _ := store.Put(blob)
		underpaid[id] = struct{}{}
	}

	// Insert a sequence of transactions with an underpriced in between to verify
	// that it and anything newly gapped will get evicted (case 5).
	var (
		outpricer, _ = crypto.GenerateKey()
		outpriced    = make(map[uint64]struct{})
	)
	for i := 0; i < 5; i++ { // make #2 underpriced
		var tx *types.Transaction
		if i == 2 {
			tx = makeTx(uint64(i), 0, 0, 0, outpricer)
		} else {
			tx = makeTx(uint64(i), 1, 1, 1, outpricer)
		}
		blob, _ := rlp.EncodeToBytes(tx)

		id, _ := store.Put(blob)
		if i < 2 {
			valids[id] = struct{}{}
		} else {
			outpriced[id] = struct{}{}
		}
	}
	// Insert a sequence of transactions fully overdrafted to verify that the
	// entire set will get invalidated (case 6).
	var (
		exceeder, _ = crypto.GenerateKey()
		exceeded    = make(map[uint64]struct{})
	)
	for _, nonce := range []uint64{0, 1, 2} { // nonce 0 overdrafts the account
		var tx *types.Transaction
		if nonce == 0 {
			tx = makeTx(nonce, 1, 100, 1, exceeder)
		} else {
			tx = makeTx(nonce, 1, 1, 1, exceeder)
		}
		blob, _ := rlp.EncodeToBytes(tx)

		id, _ := store.Put(blob)
		exceeded[id] = struct{}{}
	}
	// Insert a sequence of transactions partially overdrafted to verify that part
	// of the set will get invalidated (case 6).
	var (
		overdrafter, _ = crypto.GenerateKey()
		overdrafted    = make(map[uint64]struct{})
	)
	for _, nonce := range []uint64{0, 1, 2} { // nonce 1 overdrafts the account
		var tx *types.Transaction
		if nonce == 1 {
			tx = makeTx(nonce, 1, 100, 1, overdrafter)
		} else {
			tx = makeTx(nonce, 1, 1, 1, overdrafter)
		}
		blob, _ := rlp.EncodeToBytes(tx)

		id, _ := store.Put(blob)
		if nonce < 1 {
			valids[id] = struct{}{}
		} else {
			overdrafted[id] = struct{}{}
		}
	}
	// Insert a sequence of transactions overflowing the account cap to verify
	// that part of the set will get invalidated (case 7).
	var (
		overcapper, _ = crypto.GenerateKey()
		overcapped    = make(map[uint64]struct{})
	)
	for nonce := uint64(0); nonce < maxTxsPerAccount+3; nonce++ {
		blob, _ := rlp.EncodeToBytes(makeTx(nonce, 1, 1, 1, overcapper))

		id, _ := store.Put(blob)
		if nonce < maxTxsPerAccount {
			valids[id] = struct{}{}
		} else {
			overcapped[id] = struct{}{}
		}
	}
	// Insert a batch of duplicated transactions to verify that only one of each
	// version will remain (case 8).
	var (
		duplicater, _ = crypto.GenerateKey()
		duplicated    = make(map[uint64]struct{})
	)
	for _, nonce := range []uint64{0, 1, 2} {
		blob, _ := rlp.EncodeToBytes(makeTx(nonce, 1, 1, 1, duplicater))

		for i := 0; i < int(nonce)+1; i++ {
			id, _ := store.Put(blob)
			if i == 0 {
				valids[id] = struct{}{}
			} else {
				duplicated[id] = struct{}{}
			}
		}
	}
	// Insert a batch of duplicated nonces to verify that only one of each will
	// remain (case 9).
	var (
		repeater, _ = crypto.GenerateKey()
		repeated    = make(map[uint64]struct{})
	)
	for _, nonce := range []uint64{0, 1, 2} {
		for i := 0; i < int(nonce)+1; i++ {
			blob, _ := rlp.EncodeToBytes(makeTx(nonce, 1, uint64(i)+1 /* unique hashes */, 1, repeater))

			id, _ := store.Put(blob)
			if i == 0 {
				valids[id] = struct{}{}
			} else {
				repeated[id] = struct{}{}
			}
		}
	}
	store.Close()

	// Create a blob pool out of the pre-seeded data
	statedb, _ := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	statedb.AddBalance(crypto.PubkeyToAddress(gapper.PublicKey), uint256.NewInt(1000000), tracing.BalanceChangeUnspecified)
	statedb.AddBalance(crypto.PubkeyToAddress(dangler.PublicKey), uint256.NewInt(1000000), tracing.BalanceChangeUnspecified)
	statedb.AddBalance(crypto.PubkeyToAddress(filler.PublicKey), uint256.NewInt(1000000), tracing.BalanceChangeUnspecified)
	statedb.SetNonce(crypto.PubkeyToAddress(filler.PublicKey), 3, tracing.NonceChangeUnspecified)
	statedb.AddBalance(crypto.PubkeyToAddress(overlapper.PublicKey), uint256.NewInt(1000000), tracing.BalanceChangeUnspecified)
	statedb.SetNonce(crypto.PubkeyToAddress(overlapper.PublicKey), 2, tracing.NonceChangeUnspecified)
	statedb.AddBalance(crypto.PubkeyToAddress(underpayer.PublicKey), uint256.NewInt(1000000), tracing.BalanceChangeUnspecified)
	statedb.AddBalance(crypto.PubkeyToAddress(outpricer.PublicKey), uint256.NewInt(1000000), tracing.BalanceChangeUnspecified)
	statedb.AddBalance(crypto.PubkeyToAddress(exceeder.PublicKey), uint256.NewInt(1000000), tracing.BalanceChangeUnspecified)
	statedb.AddBalance(crypto.PubkeyToAddress(overdrafter.PublicKey), uint256.NewInt(1000000), tracing.BalanceChangeUnspecified)
	statedb.AddBalance(crypto.PubkeyToAddress(overcapper.PublicKey), uint256.NewInt(10000000), tracing.BalanceChangeUnspecified)
	statedb.AddBalance(crypto.PubkeyToAddress(duplicater.PublicKey), uint256.NewInt(1000000), tracing.BalanceChangeUnspecified)
	statedb.AddBalance(crypto.PubkeyToAddress(repeater.PublicKey), uint256.NewInt(1000000), tracing.BalanceChangeUnspecified)
	statedb.Commit(0, true, false)

	chain := &testBlockChain{
		config:  params.MainnetChainConfig,
		basefee: uint256.NewInt(params.InitialBaseFee),
		blobfee: uint256.NewInt(params.BlobTxMinBlobGasprice),
		statedb: statedb,
	}
	pool := New(Config{Datadir: storage}, chain)
	if err := pool.Init(1, chain.CurrentBlock(), makeAddressReserver()); err != nil {
		t.Fatalf("failed to create blob pool: %v", err)
	}
	defer pool.Close()

	// Verify that the malformed (case 1), badly signed (case 2) and gapped (case
	// 3) txs have been deleted from the pool
	alive := make(map[uint64]struct{})
	for _, txs := range pool.index {
		for _, tx := range txs {
			switch tx.id {
			case malformed:
				t.Errorf("malformed RLP transaction remained in storage")
			case badsig:
				t.Errorf("invalidly signed transaction remained in storage")
			default:
				if _, ok := dangling[tx.id]; ok {
					t.Errorf("dangling transaction remained in storage: %d", tx.id)
				} else if _, ok := filled[tx.id]; ok {
					t.Errorf("filled transaction remained in storage: %d", tx.id)
				} else if _, ok := overlapped[tx.id]; ok {
					t.Errorf("overlapped transaction remained in storage: %d", tx.id)
				} else if _, ok := gapped[tx.id]; ok {
					t.Errorf("gapped transaction remained in storage: %d", tx.id)
				} else if _, ok := underpaid[tx.id]; ok {
					t.Errorf("underpaid transaction remained in storage: %d", tx.id)
				} else if _, ok := outpriced[tx.id]; ok {
					t.Errorf("outpriced transaction remained in storage: %d", tx.id)
				} else if _, ok := exceeded[tx.id]; ok {
					t.Errorf("fully overdrafted transaction remained in storage: %d", tx.id)
				} else if _, ok := overdrafted[tx.id]; ok {
					t.Errorf("partially overdrafted transaction remained in storage: %d", tx.id)
				} else if _, ok := overcapped[tx.id]; ok {
					t.Errorf("overcapped transaction remained in storage: %d", tx.id)
				} else if _, ok := duplicated[tx.id]; ok {
					t.Errorf("duplicated transaction remained in storage: %d", tx.id)
				} else if _, ok := repeated[tx.id]; ok {
					t.Errorf("repeated nonce transaction remained in storage: %d", tx.id)
				} else {
					alive[tx.id] = struct{}{}
				}
			}
		}
	}
	// Verify that the rest of the transactions remained alive
	if len(alive) != len(valids) {
		t.Errorf("valid transaction count mismatch: have %d, want %d", len(alive), len(valids))
	}
	for id := range alive {
		if _, ok := valids[id]; !ok {
			t.Errorf("extra transaction %d", id)
		}
	}
	for id := range valids {
		if _, ok := alive[id]; !ok {
			t.Errorf("missing transaction %d", id)
		}
	}
	// Verify all the calculated pool internals. Interestingly, this is **not**
	// a duplication of the above checks, this actually validates the verifier
	// using the above already hard coded checks.
	//
	// Do not remove this, nor alter the above to be generic.
	verifyPoolInternals(t, pool)
}

// Tests that transactions loaded from disk are indexed correctly.
//
//   - 1. Transactions must be grouped by sender, sorted by nonce
//   - 2. Eviction thresholds are calculated correctly for the sequences
//   - 3. Balance usage of an account is totals across all transactions
func TestOpenIndex(t *testing.T) {
	//log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(os.Stderr, log.LevelTrace, true)))

	// Create a temporary folder for the persistent backend
	storage, _ := os.MkdirTemp("", "blobpool-")
	defer os.RemoveAll(storage)

	os.MkdirAll(filepath.Join(storage, pendingTransactionStore), 0700)
	store, _ := billy.Open(billy.Options{Path: filepath.Join(storage, pendingTransactionStore)}, newSlotter(testMaxBlobsPerBlock), nil)

	// Insert a sequence of transactions with varying price points to check that
	// the cumulative minimum will be maintained.
	var (
		key, _ = crypto.GenerateKey()
		addr   = crypto.PubkeyToAddress(key.PublicKey)

		txExecTipCaps = []uint64{10, 25, 5, 7, 1, 100}
		txExecFeeCaps = []uint64{100, 90, 200, 10, 80, 300}
		txBlobFeeCaps = []uint64{55, 66, 77, 33, 22, 11}

		//basefeeJumps = []float64{39.098, 38.204, 44.983, 19.549, 37.204, 48.426} // log 1.125 (exec fee cap)
		//blobfeeJumps = []float64{34.023, 35.570, 36.879, 29.686, 26.243, 20.358} // log 1.125 (blob fee cap)

		evictExecTipCaps  = []uint64{10, 10, 5, 5, 1, 1}
		evictExecFeeJumps = []float64{39.098, 38.204, 38.204, 19.549, 19.549, 19.549} //  min(log 1.125 (exec fee cap))
		evictBlobFeeJumps = []float64{34.023, 34.023, 34.023, 29.686, 26.243, 20.358} // min(log 1.125 (blob fee cap))

		totalSpent = uint256.NewInt(21000*(100+90+200+10+80+300) + blobSize*(55+66+77+33+22+11) + 100*6) // 21000 gas x price + 128KB x blobprice + value
	)
	for _, i := range []int{5, 3, 4, 2, 0, 1} { // Randomize the tx insertion order to force sorting on load
		tx := makeTx(uint64(i), txExecTipCaps[i], txExecFeeCaps[i], txBlobFeeCaps[i], key)
		blob, _ := rlp.EncodeToBytes(tx)
		store.Put(blob)
	}
	store.Close()

	// Create a blob pool out of the pre-seeded data
	statedb, _ := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	statedb.AddBalance(addr, uint256.NewInt(1_000_000_000), tracing.BalanceChangeUnspecified)
	statedb.Commit(0, true, false)

	chain := &testBlockChain{
		config:  params.MainnetChainConfig,
		basefee: uint256.NewInt(params.InitialBaseFee),
		blobfee: uint256.NewInt(params.BlobTxMinBlobGasprice),
		statedb: statedb,
	}
	pool := New(Config{Datadir: storage}, chain)
	if err := pool.Init(1, chain.CurrentBlock(), makeAddressReserver()); err != nil {
		t.Fatalf("failed to create blob pool: %v", err)
	}
	defer pool.Close()

	// Verify that the transactions have been sorted by nonce (case 1)
	for i := 0; i < len(pool.index[addr]); i++ {
		if pool.index[addr][i].nonce != uint64(i) {
			t.Errorf("tx %d nonce mismatch: have %d, want %d", i, pool.index[addr][i].nonce, uint64(i))
		}
	}
	// Verify that the cumulative fee minimums have been correctly calculated (case 2)
	for i, cap := range evictExecTipCaps {
		if !pool.index[addr][i].evictionExecTip.Eq(uint256.NewInt(cap)) {
			t.Errorf("eviction tip cap %d mismatch: have %d, want %d", i, pool.index[addr][i].evictionExecTip, cap)
		}
	}
	for i, jumps := range evictExecFeeJumps {
		if math.Abs(pool.index[addr][i].evictionExecFeeJumps-jumps) > 0.001 {
			t.Errorf("eviction fee cap jumps %d mismatch: have %f, want %f", i, pool.index[addr][i].evictionExecFeeJumps, jumps)
		}
	}
	for i, jumps := range evictBlobFeeJumps {
		if math.Abs(pool.index[addr][i].evictionBlobFeeJumps-jumps) > 0.001 {
			t.Errorf("eviction blob fee cap jumps %d mismatch: have %f, want %f", i, pool.index[addr][i].evictionBlobFeeJumps, jumps)
		}
	}
	// Verify that the balance usage has been correctly calculated (case 3)
	if !pool.spent[addr].Eq(totalSpent) {
		t.Errorf("expenditure mismatch: have %d, want %d", pool.spent[addr], totalSpent)
	}
	// Verify all the calculated pool internals. Interestingly, this is **not**
	// a duplication of the above checks, this actually validates the verifier
	// using the above already hard coded checks.
	//
	// Do not remove this, nor alter the above to be generic.
	verifyPoolInternals(t, pool)
}

// Tests that after indexing all the loaded transactions from disk, a price heap
// is correctly constructed based on the head basefee and blobfee.
func TestOpenHeap(t *testing.T) {
	//log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(os.Stderr, log.LevelTrace, true)))

	// Create a temporary folder for the persistent backend
	storage, _ := os.MkdirTemp("", "blobpool-")
	defer os.RemoveAll(storage)

	os.MkdirAll(filepath.Join(storage, pendingTransactionStore), 0700)
	store, _ := billy.Open(billy.Options{Path: filepath.Join(storage, pendingTransactionStore)}, newSlotter(testMaxBlobsPerBlock), nil)

	// Insert a few transactions from a few accounts. To remove randomness from
	// the heap initialization, use a deterministic account/tx/priority ordering.
	var (
		key1, _ = crypto.GenerateKey()
		key2, _ = crypto.GenerateKey()
		key3, _ = crypto.GenerateKey()

		addr1 = crypto.PubkeyToAddress(key1.PublicKey)
		addr2 = crypto.PubkeyToAddress(key2.PublicKey)
		addr3 = crypto.PubkeyToAddress(key3.PublicKey)
	)
	if bytes.Compare(addr1[:], addr2[:]) > 0 {
		key1, addr1, key2, addr2 = key2, addr2, key1, addr1
	}
	if bytes.Compare(addr1[:], addr3[:]) > 0 {
		key1, addr1, key3, addr3 = key3, addr3, key1, addr1
	}
	if bytes.Compare(addr2[:], addr3[:]) > 0 {
		key2, addr2, key3, addr3 = key3, addr3, key2, addr2
	}
	var (
		tx1 = makeTx(0, 1, 1000, 90, key1)
		tx2 = makeTx(0, 1, 800, 70, key2)
		tx3 = makeTx(0, 1, 1500, 110, key3)

		blob1, _ = rlp.EncodeToBytes(tx1)
		blob2, _ = rlp.EncodeToBytes(tx2)
		blob3, _ = rlp.EncodeToBytes(tx3)

		heapOrder = []common.Address{addr2, addr1, addr3}
		heapIndex = map[common.Address]int{addr2: 0, addr1: 1, addr3: 2}
	)
	store.Put(blob1)
	store.Put(blob2)
	store.Put(blob3)
	store.Close()

	// Create a blob pool out of the pre-seeded data
	statedb, _ := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	statedb.AddBalance(addr1, uint256.NewInt(1_000_000_000), tracing.BalanceChangeUnspecified)
	statedb.AddBalance(addr2, uint256.NewInt(1_000_000_000), tracing.BalanceChangeUnspecified)
	statedb.AddBalance(addr3, uint256.NewInt(1_000_000_000), tracing.BalanceChangeUnspecified)
	statedb.Commit(0, true, false)

	chain := &testBlockChain{
		config:  params.MainnetChainConfig,
		basefee: uint256.NewInt(1050),
		blobfee: uint256.NewInt(105),
		statedb: statedb,
	}
	pool := New(Config{Datadir: storage}, chain)
	if err := pool.Init(1, chain.CurrentBlock(), makeAddressReserver()); err != nil {
		t.Fatalf("failed to create blob pool: %v", err)
	}
	defer pool.Close()

	// Verify that the heap's internal state matches the expectations
	for i, addr := range pool.evict.addrs {
		if addr != heapOrder[i] {
			t.Errorf("slot %d mismatch: have %v, want %v", i, addr, heapOrder[i])
		}
	}
	for addr, i := range pool.evict.index {
		if i != heapIndex[addr] {
			t.Errorf("index for %v mismatch: have %d, want %d", addr, i, heapIndex[addr])
		}
	}
	// Verify all the calculated pool internals. Interestingly, this is **not**
	// a duplication of the above checks, this actually validates the verifier
	// using the above already hard coded checks.
	//
	// Do not remove this, nor alter the above to be generic.
	verifyPoolInternals(t, pool)
}

// Tests that after the pool's previous state is loaded back, any transactions
// over the new storage cap will get dropped.
func TestOpenCap(t *testing.T) {
	//log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(os.Stderr, log.LevelTrace, true)))

	// Create a temporary folder for the persistent backend
	storage, _ := os.MkdirTemp("", "blobpool-")
	defer os.RemoveAll(storage)

	os.MkdirAll(filepath.Join(storage, pendingTransactionStore), 0700)
	store, _ := billy.Open(billy.Options{Path: filepath.Join(storage, pendingTransactionStore)}, newSlotter(testMaxBlobsPerBlock), nil)

	// Insert a few transactions from a few accounts
	var (
		key1, _ = crypto.GenerateKey()
		key2, _ = crypto.GenerateKey()
		key3, _ = crypto.GenerateKey()

		addr1 = crypto.PubkeyToAddress(key1.PublicKey)
		addr2 = crypto.PubkeyToAddress(key2.PublicKey)
		addr3 = crypto.PubkeyToAddress(key3.PublicKey)

		tx1 = makeTx(0, 1, 1000, 100, key1)
		tx2 = makeTx(0, 1, 800, 70, key2)
		tx3 = makeTx(0, 1, 1500, 110, key3)

		blob1, _ = rlp.EncodeToBytes(tx1)
		blob2, _ = rlp.EncodeToBytes(tx2)
		blob3, _ = rlp.EncodeToBytes(tx3)

		keep = []common.Address{addr1, addr3}
		drop = []common.Address{addr2}
		size = uint64(2 * (txAvgSize + blobSize))
	)
	store.Put(blob1)
	store.Put(blob2)
	store.Put(blob3)
	store.Close()

	// Verify pool capping twice: first by reducing the data cap, then restarting
	// with a high cap to ensure everything was persisted previously
	for _, datacap := range []uint64{2 * (txAvgSize + blobSize), 100 * (txAvgSize + blobSize)} {
		// Create a blob pool out of the pre-seeded data, but cap it to 2 blob transaction
		statedb, _ := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
		statedb.AddBalance(addr1, uint256.NewInt(1_000_000_000), tracing.BalanceChangeUnspecified)
		statedb.AddBalance(addr2, uint256.NewInt(1_000_000_000), tracing.BalanceChangeUnspecified)
		statedb.AddBalance(addr3, uint256.NewInt(1_000_000_000), tracing.BalanceChangeUnspecified)
		statedb.Commit(0, true, false)

		chain := &testBlockChain{
			config:  params.MainnetChainConfig,
			basefee: uint256.NewInt(1050),
			blobfee: uint256.NewInt(105),
			statedb: statedb,
		}
		pool := New(Config{Datadir: storage, Datacap: datacap}, chain)
		if err := pool.Init(1, chain.CurrentBlock(), makeAddressReserver()); err != nil {
			t.Fatalf("failed to create blob pool: %v", err)
		}
		// Verify that enough transactions have been dropped to get the pool's size
		// under the requested limit
		if len(pool.index) != len(keep) {
			t.Errorf("tracked account count mismatch: have %d, want %d", len(pool.index), len(keep))
		}
		for _, addr := range keep {
			if _, ok := pool.index[addr]; !ok {
				t.Errorf("expected account %v missing from pool", addr)
			}
		}
		for _, addr := range drop {
			if _, ok := pool.index[addr]; ok {
				t.Errorf("unexpected account %v present in pool", addr)
			}
		}
		if pool.stored != size {
			t.Errorf("pool stored size mismatch: have %v, want %v", pool.stored, size)
		}
		// Verify all the calculated pool internals. Interestingly, this is **not**
		// a duplication of the above checks, this actually validates the verifier
		// using the above already hard coded checks.
		//
		// Do not remove this, nor alter the above to be generic.
		verifyPoolInternals(t, pool)

		pool.Close()
	}
}

// TestChangingSlotterSize attempts to mimic a scenario where the max blob count
// of the pool is increased. This would happen during a client release where a
// new fork is added with a max blob count higher than the previous fork. We
// want to make sure transactions a persisted between those runs.
func TestChangingSlotterSize(t *testing.T) {
	//log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(os.Stderr, log.LevelTrace, true)))

	// Create a temporary folder for the persistent backend
	storage, _ := os.MkdirTemp("", "blobpool-")
	defer os.RemoveAll(storage)

	os.MkdirAll(filepath.Join(storage, pendingTransactionStore), 0700)
	store, _ := billy.Open(billy.Options{Path: filepath.Join(storage, pendingTransactionStore)}, newSlotter(6), nil)

	// Create transactions from a few accounts.
	var (
		key1, _ = crypto.GenerateKey()
		key2, _ = crypto.GenerateKey()
		key3, _ = crypto.GenerateKey()

		addr1 = crypto.PubkeyToAddress(key1.PublicKey)
		addr2 = crypto.PubkeyToAddress(key2.PublicKey)
		addr3 = crypto.PubkeyToAddress(key3.PublicKey)

		tx1 = makeMultiBlobTx(0, 1, 1000, 100, 6, key1)
		tx2 = makeMultiBlobTx(0, 1, 800, 70, 6, key2)
		tx3 = makeMultiBlobTx(0, 1, 800, 110, 24, key3)

		blob1, _ = rlp.EncodeToBytes(tx1)
		blob2, _ = rlp.EncodeToBytes(tx2)
	)

	// Write the two safely sized txs to store. note: although the store is
	// configured for a blob count of 6, it can also support around ~1mb of call
	// data - all this to say that we aren't using the absolute largest shelf
	// available.
	store.Put(blob1)
	store.Put(blob2)
	store.Close()

	// Mimic a blobpool with max blob count of 6 upgrading to a max blob count of 24.
	for _, maxBlobs := range []int{6, 24} {
		statedb, _ := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
		statedb.AddBalance(addr1, uint256.NewInt(1_000_000_000), tracing.BalanceChangeUnspecified)
		statedb.AddBalance(addr2, uint256.NewInt(1_000_000_000), tracing.BalanceChangeUnspecified)
		statedb.AddBalance(addr3, uint256.NewInt(1_000_000_000), tracing.BalanceChangeUnspecified)
		statedb.Commit(0, true, false)

		// Make custom chain config where the max blob count changes based on the loop variable.
		cancunTime := uint64(0)
		config := &params.ChainConfig{
			ChainID:     big.NewInt(1),
			LondonBlock: big.NewInt(0),
			BerlinBlock: big.NewInt(0),
			CancunTime:  &cancunTime,
			BlobScheduleConfig: &params.BlobScheduleConfig{
				Cancun: &params.BlobConfig{
					Target:         maxBlobs / 2,
					Max:            maxBlobs,
					UpdateFraction: params.DefaultCancunBlobConfig.UpdateFraction,
				},
			},
		}
		chain := &testBlockChain{
			config:  config,
			basefee: uint256.NewInt(1050),
			blobfee: uint256.NewInt(105),
			statedb: statedb,
		}
		pool := New(Config{Datadir: storage}, chain)
		if err := pool.Init(1, chain.CurrentBlock(), makeAddressReserver()); err != nil {
			t.Fatalf("failed to create blob pool: %v", err)
		}

		// Try to add the big blob tx. In the initial iteration it should overflow
		// the pool. On the subsequent iteration it should be accepted.
		errs := pool.Add([]*types.Transaction{tx3}, true)
		if _, ok := pool.index[addr3]; ok && maxBlobs == 6 {
			t.Errorf("expected insert of oversized blob tx to fail: blobs=24, maxBlobs=%d, err=%v", maxBlobs, errs[0])
		} else if !ok && maxBlobs == 10 {
			t.Errorf("expected insert of oversized blob tx to succeed: blobs=24, maxBlobs=%d, err=%v", maxBlobs, errs[0])
		}

		// Verify the regular two txs are always available.
		if got := pool.Get(tx1.Hash()); got == nil {
			t.Errorf("expected tx %s from %s in pool", tx1.Hash(), addr1)
		}
		if got := pool.Get(tx2.Hash()); got == nil {
			t.Errorf("expected tx %s from %s in pool", tx2.Hash(), addr2)
		}

		// Verify all the calculated pool internals. Interestingly, this is **not**
		// a duplication of the above checks, this actually validates the verifier
		// using the above already hard coded checks.
		//
		// Do not remove this, nor alter the above to be generic.
		verifyPoolInternals(t, pool)

		pool.Close()
	}
}

// Tests that adding transaction will correctly store it in the persistent store
// and update all the indices.
//
// Note, this tests mostly checks the pool transaction shuffling logic or things
// specific to the blob pool. It does not do an exhaustive transaction validity
// check.
func TestAdd(t *testing.T) {
	//log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(os.Stderr, log.LevelTrace, true)))

	// seed is a helper tuple to seed an initial state db and pool
	type seed struct {
		balance uint64
		nonce   uint64
		txs     []*types.BlobTx
	}
	// addtx is a helper sender/tx tuple to represent a new tx addition
	type addtx struct {
		from string
		tx   *types.BlobTx
		err  error
	}

	tests := []struct {
		seeds map[string]seed
		adds  []addtx
		block []addtx
	}{
		// Transactions from new accounts should be accepted if their initial
		// nonce matches the expected one from the statedb. Higher or lower must
		// be rejected.
		{
			seeds: map[string]seed{
				"alice":  {balance: 21100 + blobSize},
				"bob":    {balance: 21100 + blobSize, nonce: 1},
				"claire": {balance: 21100 + blobSize},
				"dave":   {balance: 21100 + blobSize, nonce: 1},
			},
			adds: []addtx{
				{ // New account, no previous txs: accept nonce 0
					from: "alice",
					tx:   makeUnsignedTx(0, 1, 1, 1),
					err:  nil,
				},
				{ // Old account, 1 tx in chain, 0 pending: accept nonce 1
					from: "bob",
					tx:   makeUnsignedTx(1, 1, 1, 1),
					err:  nil,
				},
				{ // New account, no previous txs: reject nonce 1
					from: "claire",
					tx:   makeUnsignedTx(1, 1, 1, 1),
					err:  core.ErrNonceTooHigh,
				},
				{ // Old account, 1 tx in chain, 0 pending: reject nonce 0
					from: "dave",
					tx:   makeUnsignedTx(0, 1, 1, 1),
					err:  core.ErrNonceTooLow,
				},
				{ // Old account, 1 tx in chain, 0 pending: reject nonce 2
					from: "dave",
					tx:   makeUnsignedTx(2, 1, 1, 1),
					err:  core.ErrNonceTooHigh,
				},
			},
		},
		// Transactions from already pooled accounts should only be accepted if
		// the nonces are contiguous (ignore prices for now, will check later)
		{
			seeds: map[string]seed{
				"alice": {
					balance: 1000000,
					txs: []*types.BlobTx{
						makeUnsignedTxWithTestBlob(0, 1, 1, 1, 0),
					},
				},
				"bob": {
					balance: 1000000,
					nonce:   1,
					txs: []*types.BlobTx{
						makeUnsignedTxWithTestBlob(1, 1, 1, 1, 1),
					},
				},
			},
			adds: []addtx{
				{ // New account, 1 tx pending: reject duplicate nonce 0
					from: "alice",
					tx:   makeUnsignedTxWithTestBlob(0, 1, 1, 1, 0),
					err:  txpool.ErrAlreadyKnown,
				},
				{ // New account, 1 tx pending: reject replacement nonce 0 (ignore price for now)
					from: "alice",
					tx:   makeUnsignedTx(0, 1, 1, 2),
					err:  txpool.ErrReplaceUnderpriced,
				},
				{ // New account, 1 tx pending: accept nonce 1
					from: "alice",
					tx:   makeUnsignedTx(1, 1, 1, 1),
					err:  nil,
				},
				{ // New account, 2 txs pending: reject nonce 3
					from: "alice",
					tx:   makeUnsignedTx(3, 1, 1, 1),
					err:  core.ErrNonceTooHigh,
				},
				{ // New account, 2 txs pending: accept nonce 2
					from: "alice",
					tx:   makeUnsignedTx(2, 1, 1, 1),
					err:  nil,
				},
				{ // New account, 3 txs pending: accept nonce 3 now
					from: "alice",
					tx:   makeUnsignedTx(3, 1, 1, 1),
					err:  nil,
				},
				{ // Old account, 1 tx in chain, 1 tx pending: reject duplicate nonce 1
					from: "bob",
					tx:   makeUnsignedTxWithTestBlob(1, 1, 1, 1, 1),
					err:  txpool.ErrAlreadyKnown,
				},
				{ // Old account, 1 tx in chain, 1 tx pending: accept nonce 2 (ignore price for now)
					from: "bob",
					tx:   makeUnsignedTx(2, 1, 1, 1),
					err:  nil,
				},
			},
		},
		// Transactions should only be accepted into the pool if the cumulative
		// expenditure doesn't overflow the account balance
		{
			seeds: map[string]seed{
				"alice": {balance: 63299 + 3*blobSize}, // 3 tx - 1 wei
			},
			adds: []addtx{
				{ // New account, no previous txs: accept nonce 0 with 21100 wei spend
					from: "alice",
					tx:   makeUnsignedTx(0, 1, 1, 1),
					err:  nil,
				},
				{ // New account, 1 pooled tx with 21100 wei spent: accept nonce 1 with 21100 wei spend
					from: "alice",
					tx:   makeUnsignedTx(1, 1, 1, 1),
					err:  nil,
				},
				{ // New account, 2 pooled tx with 42200 wei spent: reject nonce 2 with 21100 wei spend (1 wei overflow)
					from: "alice",
					tx:   makeUnsignedTx(2, 1, 1, 1),
					err:  core.ErrInsufficientFunds,
				},
			},
		},
		// Transactions should only be accepted into the pool if the total count
		// from the same account doesn't overflow the pool limits
		{
			seeds: map[string]seed{
				"alice": {balance: 10000000},
			},
			adds: []addtx{
				{ // New account, no previous txs, 16 slots left: accept nonce 0
					from: "alice",
					tx:   makeUnsignedTx(0, 1, 1, 1),
					err:  nil,
				},
				{ // New account, 1 pooled tx, 15 slots left: accept nonce 1
					from: "alice",
					tx:   makeUnsignedTx(1, 1, 1, 1),
					err:  nil,
				},
				{ // New account, 2 pooled tx, 14 slots left: accept nonce 2
					from: "alice",
					tx:   makeUnsignedTx(2, 1, 1, 1),
					err:  nil,
				},
				{ // New account, 3 pooled tx, 13 slots left: accept nonce 3
					from: "alice",
					tx:   makeUnsignedTx(3, 1, 1, 1),
					err:  nil,
				},
				{ // New account, 4 pooled tx, 12 slots left: accept nonce 4
					from: "alice",
					tx:   makeUnsignedTx(4, 1, 1, 1),
					err:  nil,
				},
				{ // New account, 5 pooled tx, 11 slots left: accept nonce 5
					from: "alice",
					tx:   makeUnsignedTx(5, 1, 1, 1),
					err:  nil,
				},
				{ // New account, 6 pooled tx, 10 slots left: accept nonce 6
					from: "alice",
					tx:   makeUnsignedTx(6, 1, 1, 1),
					err:  nil,
				},
				{ // New account, 7 pooled tx, 9 slots left: accept nonce 7
					from: "alice",
					tx:   makeUnsignedTx(7, 1, 1, 1),
					err:  nil,
				},
				{ // New account, 8 pooled tx, 8 slots left: accept nonce 8
					from: "alice",
					tx:   makeUnsignedTx(8, 1, 1, 1),
					err:  nil,
				},
				{ // New account, 9 pooled tx, 7 slots left: accept nonce 9
					from: "alice",
					tx:   makeUnsignedTx(9, 1, 1, 1),
					err:  nil,
				},
				{ // New account, 10 pooled tx, 6 slots left: accept nonce 10
					from: "alice",
					tx:   makeUnsignedTx(10, 1, 1, 1),
					err:  nil,
				},
				{ // New account, 11 pooled tx, 5 slots left: accept nonce 11
					from: "alice",
					tx:   makeUnsignedTx(11, 1, 1, 1),
					err:  nil,
				},
				{ // New account, 12 pooled tx, 4 slots left: accept nonce 12
					from: "alice",
					tx:   makeUnsignedTx(12, 1, 1, 1),
					err:  nil,
				},
				{ // New account, 13 pooled tx, 3 slots left: accept nonce 13
					from: "alice",
					tx:   makeUnsignedTx(13, 1, 1, 1),
					err:  nil,
				},
				{ // New account, 14 pooled tx, 2 slots left: accept nonce 14
					from: "alice",
					tx:   makeUnsignedTx(14, 1, 1, 1),
					err:  nil,
				},
				{ // New account, 15 pooled tx, 1 slots left: accept nonce 15
					from: "alice",
					tx:   makeUnsignedTx(15, 1, 1, 1),
					err:  nil,
				},
				{ // New account, 16 pooled tx, 0 slots left: accept nonce 15 replacement
					from: "alice",
					tx:   makeUnsignedTx(15, 10, 10, 10),
					err:  nil,
				},
				{ // New account, 16 pooled tx, 0 slots left: reject nonce 16 with overcap
					from: "alice",
					tx:   makeUnsignedTx(16, 1, 1, 1),
					err:  txpool.ErrAccountLimitExceeded,
				},
			},
		},
		// Previously existing transactions should be allowed to be replaced iff
		// the new cumulative expenditure can be covered by the account and the
		// prices are bumped all around (no percentage check here).
		{
			seeds: map[string]seed{
				"alice": {balance: 2*100 + 5*21000 + 3*blobSize},
			},
			adds: []addtx{
				{ // New account, no previous txs: reject nonce 0 with 341172 wei spend
					from: "alice",
					tx:   makeUnsignedTx(0, 1, 20, 1),
					err:  core.ErrInsufficientFunds,
				},
				{ // New account, no previous txs: accept nonce 0 with 173172 wei spend
					from: "alice",
					tx:   makeUnsignedTx(0, 1, 2, 1),
					err:  nil,
				},
				{ // New account, 1 pooled tx with 173172 wei spent: accept nonce 1 with 152172 wei spend
					from: "alice",
					tx:   makeUnsignedTx(1, 1, 1, 1),
					err:  nil,
				},
				{ // New account, 2 pooled tx with 325344 wei spent: reject nonce 0 with 599684 wei spend (173072 extra) (would overflow balance at nonce 1)
					from: "alice",
					tx:   makeUnsignedTx(0, 2, 5, 2),
					err:  core.ErrInsufficientFunds,
				},
				{ // New account, 2 pooled tx with 325344 wei spent: reject nonce 0 with no-gastip-bump
					from: "alice",
					tx:   makeUnsignedTx(0, 1, 3, 2),
					err:  txpool.ErrReplaceUnderpriced,
				},
				{ // New account, 2 pooled tx with 325344 wei spent: reject nonce 0 with no-gascap-bump
					from: "alice",
					tx:   makeUnsignedTx(0, 2, 2, 2),
					err:  txpool.ErrReplaceUnderpriced,
				},
				{ // New account, 2 pooled tx with 325344 wei spent: reject nonce 0 with no-blobcap-bump
					from: "alice",
					tx:   makeUnsignedTx(0, 2, 4, 1),
					err:  txpool.ErrReplaceUnderpriced,
				},
				{ // New account, 2 pooled tx with 325344 wei spent: accept nonce 0 with 84100 wei spend (42000 extra)
					from: "alice",
					tx:   makeUnsignedTx(0, 2, 4, 2),
					err:  nil,
				},
			},
		},
		// Previously existing transactions should be allowed to be replaced iff
		// the new prices are bumped by a sufficient amount.
		{
			seeds: map[string]seed{
				"alice": {balance: 100 + 8*21000 + 4*blobSize},
			},
			adds: []addtx{
				{ // New account, no previous txs: accept nonce 0
					from: "alice",
					tx:   makeUnsignedTx(0, 2, 4, 2),
					err:  nil,
				},
				{ // New account, 1 pooled tx: reject nonce 0 with low-gastip-bump
					from: "alice",
					tx:   makeUnsignedTx(0, 3, 8, 4),
					err:  txpool.ErrReplaceUnderpriced,
				},
				{ // New account, 1 pooled tx: reject nonce 0 with low-gascap-bump
					from: "alice",
					tx:   makeUnsignedTx(0, 4, 6, 4),
					err:  txpool.ErrReplaceUnderpriced,
				},
				{ // New account, 1 pooled tx: reject nonce 0 with low-blobcap-bump
					from: "alice",
					tx:   makeUnsignedTx(0, 4, 8, 3),
					err:  txpool.ErrReplaceUnderpriced,
				},
				{ // New account, 1 pooled tx: accept nonce 0 with all-bumps
					from: "alice",
					tx:   makeUnsignedTx(0, 4, 8, 4),
					err:  nil,
				},
			},
		},
		// Blob transactions that don't meet the min blob gas price should be rejected
		{
			seeds: map[string]seed{
				"alice": {balance: 10000000},
			},
			adds: []addtx{
				{ // New account, no previous txs, nonce 0, but blob fee cap too low
					from: "alice",
					tx:   makeUnsignedTx(0, 1, 1, 0),
					err:  txpool.ErrUnderpriced,
				},
				{ // Same as above but blob fee cap equals minimum, should be accepted
					from: "alice",
					tx:   makeUnsignedTx(0, 1, 1, params.BlobTxMinBlobGasprice),
					err:  nil,
				},
			},
		},
		// Tests issue #30518 where a refactor broke internal state invariants,
		// causing included transactions not to be properly accounted and thus
		// account states going our of sync with the chain.
		{
			seeds: map[string]seed{
				"alice": {
					balance: 1000000,
					txs: []*types.BlobTx{
						makeUnsignedTx(0, 1, 1, 1),
					},
				},
			},
			block: []addtx{
				{
					from: "alice",
					tx:   makeUnsignedTx(0, 1, 1, 1),
				},
			},
		},
	}
	for i, tt := range tests {
		// Create a temporary folder for the persistent backend
		storage, _ := os.MkdirTemp("", "blobpool-")
		defer os.RemoveAll(storage) // late defer, still ok

		os.MkdirAll(filepath.Join(storage, pendingTransactionStore), 0700)
		store, _ := billy.Open(billy.Options{Path: filepath.Join(storage, pendingTransactionStore)}, newSlotter(testMaxBlobsPerBlock), nil)

		// Insert the seed transactions for the pool startup
		var (
			keys  = make(map[string]*ecdsa.PrivateKey)
			addrs = make(map[string]common.Address)
		)
		statedb, _ := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
		for acc, seed := range tt.seeds {
			// Generate a new random key/address for the seed account
			keys[acc], _ = crypto.GenerateKey()
			addrs[acc] = crypto.PubkeyToAddress(keys[acc].PublicKey)

			// Seed the state database with this account
			statedb.AddBalance(addrs[acc], new(uint256.Int).SetUint64(seed.balance), tracing.BalanceChangeUnspecified)
			statedb.SetNonce(addrs[acc], seed.nonce, tracing.NonceChangeUnspecified)

			// Sign the seed transactions and store them in the data store
			for _, tx := range seed.txs {
				signed := types.MustSignNewTx(keys[acc], types.LatestSigner(params.MainnetChainConfig), tx)
				blob, _ := rlp.EncodeToBytes(signed)
				store.Put(blob)
			}
		}
		statedb.Commit(0, true, false)
		store.Close()

		// Create a blob pool out of the pre-seeded dats
		chain := &testBlockChain{
			config:  params.MainnetChainConfig,
			basefee: uint256.NewInt(1050),
			blobfee: uint256.NewInt(105),
			statedb: statedb,
		}
		pool := New(Config{Datadir: storage}, chain)
		if err := pool.Init(1, chain.CurrentBlock(), makeAddressReserver()); err != nil {
			t.Fatalf("test %d: failed to create blob pool: %v", i, err)
		}
		verifyPoolInternals(t, pool)

		// Add each transaction one by one, verifying the pool internals in between
		for j, add := range tt.adds {
			signed, _ := types.SignNewTx(keys[add.from], types.LatestSigner(params.MainnetChainConfig), add.tx)
			if err := pool.add(signed); !errors.Is(err, add.err) {
				t.Errorf("test %d, tx %d: adding transaction error mismatch: have %v, want %v", i, j, err, add.err)
			}
			verifyPoolInternals(t, pool)
		}
		verifyPoolInternals(t, pool)

		// If the test contains a chain head event, run that and gain verify the internals
		if tt.block != nil {
			// Fake a header for the new set of transactions
			header := &types.Header{
				Number:     big.NewInt(int64(chain.CurrentBlock().Number.Uint64() + 1)),
				Difficulty: common.Big0,
				BaseFee:    chain.CurrentBlock().BaseFee, // invalid, but nothing checks it, yolo
			}
			// Inject the fake block into the chain
			txs := make([]*types.Transaction, len(tt.block))
			for j, inc := range tt.block {
				txs[j] = types.MustSignNewTx(keys[inc.from], types.LatestSigner(params.MainnetChainConfig), inc.tx)
			}
			chain.blocks = map[uint64]*types.Block{
				header.Number.Uint64(): types.NewBlockWithHeader(header).WithBody(types.Body{
					Transactions: txs,
				}),
			}
			// Apply the nonce updates to the state db
			for _, tx := range txs {
				sender, _ := types.Sender(types.LatestSigner(params.MainnetChainConfig), tx)
				chain.statedb.SetNonce(sender, tx.Nonce()+1, tracing.NonceChangeUnspecified)
			}
			pool.Reset(chain.CurrentBlock(), header)
			verifyPoolInternals(t, pool)
		}
		// Close down the test
		pool.Close()
	}
}

// fakeBilly is a billy.Database implementation which just drops data on the floor.
type fakeBilly struct {
	billy.Database
	count uint64
}

func (f *fakeBilly) Put(data []byte) (uint64, error) {
	f.count++
	return f.count, nil
}

var _ billy.Database = (*fakeBilly)(nil)

// Benchmarks the time it takes to assemble the lazy pending transaction list
// from the pool contents.
func BenchmarkPoolPending100Mb(b *testing.B) { benchmarkPoolPending(b, 100_000_000) }
func BenchmarkPoolPending1GB(b *testing.B)   { benchmarkPoolPending(b, 1_000_000_000) }
func BenchmarkPoolPending10GB(b *testing.B) {
	if testing.Short() {
		b.Skip("Skipping in short-mode")
	}
	benchmarkPoolPending(b, 10_000_000_000)
}

func benchmarkPoolPending(b *testing.B, datacap uint64) {
	// Calculate the maximum number of transaction that would fit into the pool
	// and generate a set of random accounts to seed them with.
	capacity := datacap / params.BlobTxBlobGasPerBlob

	var (
		basefee    = uint64(1050)
		blobfee    = uint64(105)
		signer     = types.LatestSigner(params.MainnetChainConfig)
		statedb, _ = state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
		chain      = &testBlockChain{
			config:  params.MainnetChainConfig,
			basefee: uint256.NewInt(basefee),
			blobfee: uint256.NewInt(blobfee),
			statedb: statedb,
		}
		pool = New(Config{Datadir: ""}, chain)
	)

	if err := pool.Init(1, chain.CurrentBlock(), makeAddressReserver()); err != nil {
		b.Fatalf("failed to create blob pool: %v", err)
	}
	// Make the pool not use disk (just drop everything). This test never reads
	// back the data, it just iterates over the pool in-memory items
	pool.store = &fakeBilly{pool.store, 0}
	// Avoid validation - verifying all blob proofs take significant time
	// when the capacity is large. The purpose of this bench is to measure assembling
	// the lazies, not the kzg verifications.
	pool.txValidationFn = func(tx *types.Transaction, head *types.Header, signer types.Signer, opts *txpool.ValidationOptions) error {
		return nil // accept all
	}
	// Fill the pool up with one random transaction from each account with the
	// same price and everything to maximize the worst case scenario
	for i := 0; i < int(capacity); i++ {
		blobtx := makeUnsignedTx(0, 10, basefee+10, blobfee)
		blobtx.R = uint256.NewInt(1)
		blobtx.S = uint256.NewInt(uint64(100 + i))
		blobtx.V = uint256.NewInt(0)
		tx := types.NewTx(blobtx)
		addr, err := types.Sender(signer, tx)
		if err != nil {
			b.Fatal(err)
		}
		statedb.AddBalance(addr, uint256.NewInt(1_000_000_000), tracing.BalanceChangeUnspecified)
		pool.add(tx)
	}
	statedb.Commit(0, true, false)
	defer pool.Close()

	// Benchmark assembling the pending
	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		p := pool.Pending(txpool.PendingFilter{
			MinTip:  uint256.NewInt(1),
			BaseFee: chain.basefee,
			BlobFee: chain.blobfee,
		})
		if len(p) != int(capacity) {
			b.Fatalf("have %d want %d", len(p), capacity)
		}
	}
}
