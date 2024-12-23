package fakebeacon

import (
	"context"
	"sort"
	"strconv"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto/kzg4844"
	"github.com/ethereum/go-ethereum/internal/ethapi"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rpc"
)

// spec: https://ethereum.github.io/beacon-APIs/#/Beacon/getBlobSidecars
type BlobSidecar struct {
	Blob          kzg4844.Blob       `json:"blob"`
	Index         Uint64String       `json:"index"`
	KZGCommitment kzg4844.Commitment `json:"kzg_commitment"`
	KZGProof      kzg4844.Proof      `json:"kzg_proof"`
}

type APIGetBlobSidecarsResponse struct {
	Data []*BlobSidecar `json:"data"`
}

type ReducedGenesisData struct {
	GenesisTime string `json:"genesis_time"`
}

type APIGenesisResponse struct {
	Data ReducedGenesisData `json:"data"`
}

type ReducedConfigData struct {
	SecondsPerSlot string `json:"SECONDS_PER_SLOT"`
}

type IndexedBlobHash struct {
	Index int         // absolute index in the block, a.k.a. position in sidecar blobs array
	Hash  common.Hash // hash of the blob, used for consistency checks
}

// Uint64String is a decimal string representation of an uint64, for usage in the Beacon API JSON encoding
type Uint64String uint64

func (v Uint64String) MarshalText() (out []byte, err error) {
	out = strconv.AppendUint(out, uint64(v), 10)
	return
}

func (v *Uint64String) UnmarshalText(b []byte) error {
	n, err := strconv.ParseUint(string(b), 0, 64)
	if err != nil {
		return err
	}
	*v = Uint64String(n)
	return nil
}

func configSpec() ReducedConfigData {
	return ReducedConfigData{SecondsPerSlot: "1"}
}

func beaconGenesis() APIGenesisResponse {
	return APIGenesisResponse{Data: ReducedGenesisData{GenesisTime: "0"}}
}

func beaconBlobSidecars(ctx context.Context, backend ethapi.Backend, slot uint64, indices []int) (APIGetBlobSidecarsResponse, error) {
	var blockNrOrHash rpc.BlockNumberOrHash
	header, err := fetchBlockNumberByTime(ctx, int64(slot), backend)
	if err != nil {
		log.Error("Error fetching block number", "slot", slot, "indices", indices)
		return APIGetBlobSidecarsResponse{}, err
	}
	sideCars, err := backend.GetBlobSidecars(ctx, header.Hash())
	if err != nil {
		log.Error("Error fetching Sidecars", "blockNrOrHash", blockNrOrHash, "err", err)
		return APIGetBlobSidecarsResponse{}, err
	}
	sort.Ints(indices)
	fullBlob := len(indices) == 0
	res := APIGetBlobSidecarsResponse{}
	idx := 0
	curIdx := 0
	for _, sideCar := range sideCars {
		for i := 0; i < len(sideCar.Blobs); i++ {
			//hash := kZGToVersionedHash(sideCar.Commitments[i])
			if !fullBlob && curIdx >= len(indices) {
				break
			}
			if fullBlob || idx == indices[curIdx] {
				res.Data = append(res.Data, &BlobSidecar{
					Index:         Uint64String(idx),
					Blob:          sideCar.Blobs[i],
					KZGCommitment: sideCar.Commitments[i],
					KZGProof:      sideCar.Proofs[i],
				})
				curIdx++
			}
			idx++
		}
	}

	return res, nil
}