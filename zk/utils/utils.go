package utils

import (
	"fmt"

	"github.com/gateway-fm/cdk-erigon-lib/common"
	libcommon "github.com/gateway-fm/cdk-erigon-lib/common"
	"github.com/gateway-fm/cdk-erigon-lib/kv"
	"github.com/ledgerwatch/erigon/chain"
	"github.com/ledgerwatch/erigon/core/rawdb"
	"github.com/ledgerwatch/erigon/core/state"
	"github.com/ledgerwatch/erigon/core/systemcontracts"
	eritypes "github.com/ledgerwatch/erigon/core/types"
	"github.com/ledgerwatch/erigon/eth/stagedsync/stages"
	"github.com/ledgerwatch/erigon/zk/constants"
	"github.com/ledgerwatch/erigon/zk/hermez_db"
	zktx "github.com/ledgerwatch/erigon/zk/tx"
	"github.com/ledgerwatch/log/v3"
)

// if current sync is before verified batch - short circuit to verified batch, otherwise to enx of next batch
// if there is no new fully downloaded batch - do not short circuit
// returns (shouldShortCircuit, blockNumber, error)
func ShouldShortCircuitExecution(tx kv.RwTx, logPrefix string) (bool, uint64, error) {
	hermezDb := hermez_db.NewHermezDb(tx)

	// get highest verified batch
	highestVerifiedBatchNo, err := stages.GetStageProgress(tx, stages.L1VerificationsBatchNo)
	if err != nil {
		return false, 0, err
	}

	// get highest executed batch
	executedBlock, err := stages.GetStageProgress(tx, stages.Execution)
	if err != nil {
		return false, 0, err
	}

	executedBatch, err := hermezDb.GetBatchNoByL2Block(executedBlock)
	if err != nil {
		return false, 0, err
	}

	downloadedBatch, err := hermezDb.GetLatestDownloadedBatchNo()
	if err != nil {
		return false, 0, err
	}

	var shortCircuitBatch, shortCircuitBlock, cycle uint64

	// this is so empty batches work
	for shortCircuitBlock == 0 {
		cycle++
		// if executed lower than verified, short curcuit up to verified
		if executedBatch < highestVerifiedBatchNo {
			if downloadedBatch < highestVerifiedBatchNo {
				shortCircuitBatch = downloadedBatch
			} else {
				shortCircuitBatch = highestVerifiedBatchNo
			}
		} else if executedBatch+cycle <= downloadedBatch { // else short circuit up to next downloaded batch
			shortCircuitBatch = executedBatch + cycle
		} else { // if we don't have at least one more full downlaoded batch, don't short circuit and just execute to latest block
			return false, 0, nil
		}

		// we've got the highest batch to execute to, now get it's highest block
		shortCircuitBlock, err = hermezDb.GetHighestBlockInBatch(shortCircuitBatch)
		if err != nil {
			return false, 0, err
		}
	}

	log.Info(fmt.Sprintf("[%s] Short circuit", logPrefix), "batch", shortCircuitBatch, "block", shortCircuitBlock)

	return true, shortCircuitBlock, nil
}

type ForkReader interface {
	GetLowestBatchByFork(forkId uint64) (uint64, error)
	GetLowestBlockInBatch(batchNo uint64) (blockNo uint64, found bool, err error)
}

type ForkConfigWriter interface {
	SetForkIdBlock(forkId constants.ForkId, blockNum uint64) error
}

type DbReader interface {
	GetHighestBlockInBatch(batchNo uint64) (uint64, error)
}

func UpdateZkEVMBlockCfg(cfg ForkConfigWriter, hermezDb ForkReader, logPrefix string) error {
	var lastSetBlockNum uint64 = 0
	var foundAny bool = false

	for _, forkId := range chain.ForkIdsOrdered {
		batch, err := hermezDb.GetLowestBatchByFork(uint64(forkId))
		if err != nil {
			return err
		}
		blockNum, found, err := hermezDb.GetLowestBlockInBatch(batch)
		if err != nil {
			return err
		}

		if found {
			lastSetBlockNum = blockNum
			foundAny = true
		} else if !foundAny {
			log.Trace(fmt.Sprintf("[%s] No block number found for fork id %v and no previous block number set", logPrefix, forkId))
			continue
		} else {
			log.Trace(fmt.Sprintf("[%s] No block number found for fork id %v, using last set block number: %v", logPrefix, forkId, lastSetBlockNum))
		}

		if err := cfg.SetForkIdBlock(forkId, lastSetBlockNum); err != nil {
			log.Error(fmt.Sprintf("[%s] Error setting fork id %v to block %v", logPrefix, forkId, lastSetBlockNum))
			return err
		}
	}

	return nil
}

func RecoverySetBlockConfigForks(blockNum uint64, forkId uint64, cfg ForkConfigWriter, logPrefix string) error {
	for _, fork := range chain.ForkIdsOrdered {
		if uint64(fork) <= forkId {
			if err := cfg.SetForkIdBlock(fork, blockNum); err != nil {
				log.Error(fmt.Sprintf("[%s] Error setting fork id %v to block %v", logPrefix, forkId, blockNum))
				return err
			}
		}
	}

	return nil
}

func GetBatchLocalExitRootFromSCStorageForLatestBlock(batchNo uint64, db DbReader, tx kv.Tx) (libcommon.Hash, error) {
	if batchNo > 0 {
		blockNo, err := db.GetHighestBlockInBatch(batchNo)
		if err != nil {
			return libcommon.Hash{}, err
		}

		return GetBatchLocalExitRootFromSCStorageByBlock(blockNo, db, tx)
	}

	return libcommon.Hash{}, nil

}

func GetBatchLocalExitRootFromSCStorageByBlock(blockNumber uint64, db DbReader, tx kv.Tx) (libcommon.Hash, error) {
	if blockNumber > 0 {
		stateReader := state.NewPlainState(tx, blockNumber+1, systemcontracts.SystemContractCodeLookup["hermez"])
		defer stateReader.Close()
		rawLer, err := stateReader.ReadAccountStorage(state.GER_MANAGER_ADDRESS, 1, &state.GLOBAL_EXIT_ROOT_POS_1)
		if err != nil {
			return libcommon.Hash{}, err
		}
		return libcommon.BytesToHash(rawLer), nil
	}

	return libcommon.Hash{}, nil
}

func GenerateBatchData(
	tx kv.Tx,
	hermezDb state.ReadOnlyHermezDb,
	batchBlocks []*eritypes.Block,
	forkId uint64,
) (batchL2Data []byte, err error) {
	lastBlockNoInPreviousBatch := uint64(0)
	firstBlockInBatch := batchBlocks[0]
	if firstBlockInBatch.NumberU64() != 0 {
		lastBlockNoInPreviousBatch = firstBlockInBatch.NumberU64() - 1
	}

	lastBlockInPreviousBatch, err := rawdb.ReadBlockByNumber(tx, lastBlockNoInPreviousBatch)
	if err != nil {
		return nil, err
	}

	batchL2Data = []byte{}
	for i := 0; i < len(batchBlocks); i++ {
		var dTs uint32
		if i == 0 {
			dTs = uint32(batchBlocks[i].Time() - lastBlockInPreviousBatch.Time())
		} else {
			dTs = uint32(batchBlocks[i].Time() - batchBlocks[i-1].Time())
		}
		iti, err := hermezDb.GetBlockL1InfoTreeIndex(batchBlocks[i].NumberU64())
		if err != nil {
			return nil, err
		}
		egTx := make(map[common.Hash]uint8)
		for _, txn := range batchBlocks[i].Transactions() {
			eg, err := hermezDb.GetEffectiveGasPricePercentage(txn.Hash())
			if err != nil {
				return nil, err
			}
			egTx[txn.Hash()] = eg
		}

		bl2d, err := zktx.GenerateBlockBatchL2Data(uint16(forkId), dTs, uint32(iti), batchBlocks[i].Transactions(), egTx)
		if err != nil {
			return nil, err
		}
		batchL2Data = append(batchL2Data, bl2d...)
	}

	return batchL2Data, err
}
