/**
 *  @file
 *  @copyright defined in aergo/LICENSE.txt
 */

package chain

import (
	"bytes"
	"container/list"
	"errors"
	"fmt"
	"math"

	"github.com/aergoio/aergo/consensus"
	"github.com/aergoio/aergo/contract"
	"github.com/aergoio/aergo/internal/enc"
	"github.com/aergoio/aergo/message"
	"github.com/aergoio/aergo/state"
	"github.com/aergoio/aergo/types"
	"github.com/libp2p/go-libp2p-peer"
)

var (
	ErrTxInvalidNonce        = errors.New("invalid nonce")
	ErrTxInsufficientBalance = errors.New("insufficient balance")
	ErrTxInvalidType         = errors.New("invalid type")
	ErrorNoAncestor          = errors.New("not found ancestor")
	ErrBlockOrphan           = errors.New("block is ohphan, so not connected in chain")
)

type ErrBlock struct {
	err   error
	block *types.BlockInfo
}

func (ec *ErrBlock) Error() string {
	return fmt.Sprintf("Error:%s. block(%s, %d)", ec.err.Error(), enc.ToString(ec.block.Hash), ec.block.No)
}

type ErrTx struct {
	err error
	tx  *types.Tx
}

func (ec *ErrTx) Error() string {
	return fmt.Sprintf("error executing tx:%s, tx=%s", ec.err.Error(), enc.ToString(ec.tx.GetHash()))
}

func (cs *ChainService) getBestBlockNo() types.BlockNo {
	return cs.cdb.getBestBlockNo()
}

func (cs *ChainService) GetBestBlock() (*types.Block, error) {
	return cs.cdb.GetBestBlock()
}

func (cs *ChainService) getBlockByNo(blockNo types.BlockNo) (*types.Block, error) {
	return cs.cdb.GetBlockByNo(blockNo)
}

func (cs *ChainService) GetBlock(blockHash []byte) (*types.Block, error) {
	return cs.getBlock(blockHash)
}

func (cs *ChainService) getBlock(blockHash []byte) (*types.Block, error) {
	return cs.cdb.getBlock(blockHash)
}

func (cs *ChainService) GetHashByNo(blockNo types.BlockNo) ([]byte, error) {
	return cs.getHashByNo(blockNo)
}

func (cs *ChainService) getHashByNo(blockNo types.BlockNo) ([]byte, error) {
	return cs.cdb.getHashByNo(blockNo)
}

func (cs *ChainService) getTx(txHash []byte) (*types.Tx, *types.TxIdx, error) {
	tx, txidx, err := cs.cdb.getTx(txHash)
	if err != nil {
		return nil, nil, err
	}
	block, err := cs.cdb.getBlock(txidx.BlockHash)
	blockInMainChain, err := cs.cdb.GetBlockByNo(block.Header.BlockNo)
	if !bytes.Equal(block.BlockHash(), blockInMainChain.BlockHash()) {
		return tx, nil, errors.New("tx is not in the main chain")
	}
	return tx, txidx, err
}

func (cs *ChainService) getReceipt(txHash []byte) (*types.Receipt, error) {
	_, i, err := cs.cdb.getTx(txHash)
	if err != nil {
		return nil, err
	}

	block, err := cs.cdb.getBlock(i.BlockHash)
	blockInMainChain, err := cs.cdb.GetBlockByNo(block.Header.BlockNo)
	if !bytes.Equal(block.BlockHash(), blockInMainChain.BlockHash()) {
		return nil, errors.New("cannot find a receipt")
	}

	return cs.cdb.getReceipt(block.GetHash(), block.GetHeader().BlockNo, i.Idx)
}

type chainProcessor struct {
	*ChainService
	block     *types.Block // starting block
	lastBlock *types.Block
	state     *state.BlockState
	mainChain *list.List

	add func(blk *types.Block) error
}

func newChainProcessor(block *types.Block, state *state.BlockState, cs *ChainService) (*chainProcessor, error) {
	var isMainChain bool
	var err error

	if isMainChain, err = cs.cdb.isMainChain(block); err != nil {
		return nil, err
	}

	cp := &chainProcessor{
		ChainService: cs,
		block:        block,
		state:        state,
	}

	if isMainChain {
		cp.mainChain = list.New()
		cp.add = func(blk *types.Block) error {
			if err := cp.addCommon(blk); err != nil {
				return err
			}
			// blk must be executed later if it belongs to the main chain.
			cp.mainChain.PushBack(blk)

			return nil
		}
	} else {
		cp.add = cp.addCommon
	}

	return cp, nil
}

func (cp *chainProcessor) addCommon(blk *types.Block) error {
	dbTx := cp.cdb.store.NewTx()
	defer dbTx.Discard()

	if err := cp.cdb.addBlock(&dbTx, blk); err != nil {
		return err
	}

	dbTx.Commit()

	if logger.IsDebugEnabled() {
		logger.Debug().Bool("isMainChain", cp.isMain()).
			Uint64("latest", cp.cdb.latest).
			Uint64("blockNo", blk.BlockNo()).
			Str("hash", blk.ID()).
			Str("prev_hash", enc.ToString(blk.GetHeader().GetPrevBlockHash())).
			Msg("block added to the block indices")
	}
	cp.lastBlock = blk

	return nil
}

func (cp *chainProcessor) prepare() error {
	var err error

	blk := cp.block
	for blk != nil {
		// Add blk to the corresponding block chain.
		if err := cp.add(blk); err != nil {
			return err
		}

		// Remove a block depnding on blk from the orphan cache.
		if blk, err = cp.resolveOrphan(blk); err != nil {
			return err
		}
	}

	return nil
}

func (cp *chainProcessor) isMain() bool {
	return cp.mainChain != nil
}

func (cp *chainProcessor) executeBlock(block *types.Block) error {
	err := cp.ChainService.executeBlock(cp.state, block)
	cp.state = nil
	return err
}

func (cp *chainProcessor) execute() error {
	if !cp.isMain() {
		return nil
	}
	logger.Debug().Int("blocks to execute", cp.mainChain.Len()).Msg("start to execute")

	var err error
	for e := cp.mainChain.Front(); e != nil; e = e.Next() {
		block := e.Value.(*types.Block)

		err = cp.executeBlock(block)
		if err != nil {
			logger.Error().Str("error", err.Error()).Str("hash", block.ID()).
				Msg("failed to execute block")
			return err
		}
		//SyncWithConsensus :ga
		// 	After executing MemPoolDel in the chain service, MemPoolGet must be executed on the consensus.
		// 	To do this, cdb.setLatest() must be executed after MemPoolDel.
		//	In this case, messages of mempool is synchronized in actor message queue.
		var oldLatest types.BlockNo
		if oldLatest, err = cp.connectToChain(block); err != nil {
			return err
		}
		cp.notifyBlock(block)
		blockNo := block.BlockNo()
		if logger.IsDebugEnabled() {
			logger.Debug().
				Uint64("old latest", oldLatest).
				Uint64("new latest", blockNo).
				Str("hash", block.ID()).
				Str("prev_hash", enc.ToString(block.GetHeader().GetPrevBlockHash())).
				Msg("block executed")
		}
	}

	return nil
}

func (cp *chainProcessor) connectToChain(block *types.Block) (types.BlockNo, error) {
	dbTx := cp.cdb.store.NewTx()
	defer dbTx.Discard()

	oldLatest := cp.cdb.connectToChain(&dbTx, block)

	if err := cp.cdb.addTxsOfBlock(&dbTx, block.GetBody().GetTxs(), block.BlockHash()); err != nil {
		return 0, err
	}

	dbTx.Commit()

	return oldLatest, nil
}

func (cp *chainProcessor) reorganize() {
	// - Reorganize if new bestblock then process Txs
	// - Add block if new bestblock then update context connect next orphan
	if cp.needReorg(cp.lastBlock) {
		err := cp.reorg(cp.lastBlock)
		if e, ok := err.(consensus.ErrorConsensus); ok {
			logger.Info().Err(e).Msg("stop reorganization")
			return
		}

		if err != nil {
			panic(err)
		}
	}
}

func (cs *ChainService) addBlock(newBlock *types.Block, usedBstate *state.BlockState, peerID peer.ID) error {
	logger.Debug().Str("hash", newBlock.ID()).Msg("add block")

	var bestBlock *types.Block
	var err error

	if bestBlock, err = cs.cdb.GetBestBlock(); err != nil {
		return err
	}

	// Check consensus header validity
	if err := cs.IsBlockValid(newBlock, bestBlock); err != nil {
		return err
	}

	// handle orphan
	if cs.isOrphan(newBlock) {
		if usedBstate != nil {
			return fmt.Errorf("block received from BP can not be orphan")
		}
		err := cs.handleOrphan(newBlock, bestBlock, peerID)
		if err == nil {
			return ErrBlockOrphan
		} else {
			return err
		}
	}

	cp, err := newChainProcessor(newBlock, usedBstate, cs)
	if err != nil {
		return err
	}

	if err := cp.prepare(); err != nil {
		return err
	}
	if err := cp.execute(); err != nil {
		return err
	}

	// TODO: reorganization should be done before chain execution to avoid an
	// unnecessary chain execution & rollback.
	cp.reorganize()

	logger.Info().Uint64("best", cs.cdb.getBestBlockNo()).Msg("added block successfully. ")

	return nil
}

func (cs *ChainService) CountTxsInChain() int {
	var txCount int

	blk, err := cs.GetBestBlock()
	if err != nil {
		return -1
	}

	var no uint64
	for {
		no = blk.GetHeader().GetBlockNo()
		if no == 0 {
			break
		}

		txCount += len(blk.GetBody().GetTxs())

		blk, err = cs.getBlock(blk.GetHeader().GetPrevBlockHash())
		if err != nil {
			txCount = -1
			break
		}
	}

	return txCount
}

type TxExecFn func(bState *state.BlockState, tx *types.Tx) error
type ValidatePostFn func() error

type blockExecutor struct {
	*state.BlockState
	sdb              *state.ChainStateDB
	execTx           TxExecFn
	txs              []*types.Tx
	validatePost     ValidatePostFn
	coinbaseAcccount []byte
	commitOnly       bool
}

func newBlockExecutor(cs *ChainService, bState *state.BlockState, block *types.Block) (*blockExecutor, error) {
	var exec TxExecFn

	commitOnly := false

	// The DPoS block factory excutes transactions during block generation. In
	// such a case it send block with block state so that bState != nil. On the
	// contrary, the block propagated from the network is not half-executed.
	// Hence we need a new block state and tx executor (execTx).
	if bState == nil {
		if err := cs.validator.ValidateBlock(block); err != nil {
			return nil, err
		}

		bState = state.NewBlockState(cs.sdb.OpenNewStateDB(cs.sdb.GetRoot()))

		exec = NewTxExecutor(block.BlockNo(), block.GetHeader().GetTimestamp(), contract.ChainService)
	} else {
		logger.Debug().Uint64("block no", block.BlockNo()).Msg("received block from block factory")
		// In this case (bState != nil), the transactions has already been
		// executed by the block factory.
		commitOnly = true
	}

	return &blockExecutor{
		BlockState:       bState,
		sdb:              cs.sdb,
		execTx:           exec,
		txs:              block.GetBody().GetTxs(),
		coinbaseAcccount: block.GetHeader().GetCoinbaseAccount(),
		validatePost: func() error {
			return cs.validator.ValidatePost(bState.GetRoot(), bState.Receipts(), block)
		},
		commitOnly: commitOnly,
	}, nil
}

// NewTxExecutor returns a new TxExecFn.
func NewTxExecutor(blockNo types.BlockNo, ts int64, preLoadService int) TxExecFn {
	return func(bState *state.BlockState, tx *types.Tx) error {
		if bState == nil {
			logger.Error().Msg("bstate is nil in txexec")
			return ErrGatherChain
		}
		snapshot := bState.Snapshot()

		err := executeTx(bState, tx, blockNo, ts, preLoadService)
		if err != nil {
			logger.Error().Err(err).Str("hash", enc.ToString(tx.GetHash())).Msg("tx failed")
			bState.Rollback(snapshot)
			return err
		}
		return nil
	}
}

func (e *blockExecutor) execute() error {
	// Receipt must be committed unconditionally.
	if !e.commitOnly {
		var preLoadTx *types.Tx
		nCand := len(e.txs)
		for i, tx := range e.txs {
			if i != nCand-1 {
				preLoadTx = e.txs[i+1]
				contract.PreLoadRequest(e.BlockState, preLoadTx, contract.ChainService)
			}
			if err := e.execTx(e.BlockState, tx); err != nil {
				//FIXME maybe system error. restart or panic
				// all txs have executed successfully in BP node
				return err
			}
			contract.SetPreloadTx(preLoadTx, contract.ChainService)
		}

		if err := SendRewardCoinbase(e.BlockState, e.coinbaseAcccount); err != nil {
			return err
		}

		if err := contract.SaveRecoveryPoint(e.BlockState); err != nil {
			return err
		}

		if err := e.Update(); err != nil {
			return err
		}
	}

	if err := e.validatePost(); err != nil {
		return err
	}

	// TODO: sync status of bstate and cdb what to do if cdb.commit fails after

	if err := e.commit(); err != nil {
		return err
	}

	logger.Debug().Msg("executed block")
	return nil
}

func (e *blockExecutor) commit() error {
	if err := e.BlockState.Commit(); err != nil {
		return err
	}

	//TODO: after implementing BlockRootHash, remove statedb.lastest
	if err := e.sdb.UpdateRoot(e.BlockState); err != nil {
		return err
	}

	return nil
}

//TODO Refactoring: batch
func (cs *ChainService) executeBlock(bstate *state.BlockState, block *types.Block) error {
	ex, err := newBlockExecutor(cs, bstate, block)
	if err != nil {
		return err
	}

	// contract & state DB update is done during execution.
	if err := ex.execute(); err != nil {
		// FIXME: is that enough?
		logger.Error().Err(err).Str("hash", block.ID()).Msg("failed to execute block")

		return err
	}

	cs.cdb.writeReceipts(block.BlockHash(), block.BlockNo(), ex.BlockState.Receipts())

	cs.RequestTo(message.MemPoolSvc, &message.MemPoolDel{
		Block: block,
	})

	cs.Update(block)

	return nil
}

func executeTx(bs *state.BlockState, tx *types.Tx, blockNo uint64, ts int64, preLoadService int) error {
	err := tx.Validate()
	if err != nil {
		return err
	}
	txBody := tx.GetBody()

	sender, err := bs.GetAccountStateV(txBody.Account)
	if err != nil {
		return err
	}

	err = tx.ValidateWithSenderState(sender.State())
	if err != nil {
		return err
	}

	recipient := txBody.Recipient
	var receiver *state.V
	if len(recipient) > 0 {
		receiver, err = bs.GetAccountStateV(recipient)
	} else {
		receiver, err = bs.CreateAccountStateV(contract.CreateContractID(txBody.Account, txBody.Nonce))
	}
	if err != nil {
		return err
	}

	var txFee uint64
	var rv string
	switch txBody.Type {
	case types.TxType_NORMAL:
		txFee = CoinbaseFee
		sender.SubBalance(txFee)
		rv, err = contract.Execute(bs, tx, blockNo, ts, sender, receiver, preLoadService)
	case types.TxType_GOVERNANCE:
		err = executeGovernanceTx(&bs.StateDB, txBody, sender, receiver, blockNo)
		if err != nil {
			logger.Warn().Err(err).Str("txhash", enc.ToString(tx.GetHash())).Msg("governance tx Error")
		}
	}

	if err != nil {
		if _, ok := err.(contract.VmError); ok {
			sender.Reset()
			sender.SubBalance(txFee)
			sender.SetNonce(txBody.Nonce)
			sErr := sender.PutState()
			if sErr != nil {
				return sErr
			}
			bs.BpReward += txFee
			bs.AddReceipt(types.NewReceipt(receiver.ID(), err.Error(), ""))
			return nil
		}
		return err
	}

	sender.SetNonce(txBody.Nonce)
	err = sender.PutState()
	if err != nil {
		return err
	}
	if sender.AccountID() != receiver.AccountID() {
		err = receiver.PutState()
		if err != nil {
			return err
		}
	}

	bs.BpReward += txFee

	if receiver.IsNew() && txBody.Recipient == nil {
		bs.AddReceipt(types.NewReceipt(receiver.ID(), "CREATED", rv))
		return nil

	}
	bs.AddReceipt(types.NewReceipt(receiver.ID(), "SUCCESS", rv))
	return nil
}

func SendRewardCoinbase(bState *state.BlockState, coinbaseAccount []byte) error {
	if bState.BpReward <= 0 || coinbaseAccount == nil {
		logger.Debug().Uint64("reward", bState.BpReward).Msg("coinbase is skipped")
		return nil
	}

	receiverID := types.ToAccountID(coinbaseAccount)
	receiverState, err := bState.GetAccountState(receiverID)
	if err != nil {
		return err
	}

	receiverChange := types.State(*receiverState)
	receiverChange.Balance = receiverChange.Balance + bState.BpReward

	err = bState.PutState(receiverID, &receiverChange)
	if err != nil {
		return err
	}

	logger.Debug().Uint64("reward", bState.BpReward).
		Uint64("newbalance", receiverChange.Balance).Msg("send reward to coinbase account")

	return nil
}

// find an orphan block which is the child of the added block
func (cs *ChainService) resolveOrphan(block *types.Block) (*types.Block, error) {
	hash := block.BlockHash()

	orphanID := types.ToBlockID(hash)
	orphan, exists := cs.op.cache[orphanID]
	if !exists {
		return nil, nil
	}

	orphanBlock := orphan.block

	if (block.GetHeader().GetBlockNo() + 1) != orphanBlock.GetHeader().GetBlockNo() {
		return nil, fmt.Errorf("invalid orphan block no (p=%d, c=%d)", block.GetHeader().GetBlockNo(),
			orphanBlock.GetHeader().GetBlockNo())
	}

	logger.Debug().Str("parentHash=", block.ID()).
		Str("orphanHash=", orphanBlock.ID()).
		Msg("connect orphan")

	cs.op.removeOrphan(orphanID)

	return orphanBlock, nil
}

func (cs *ChainService) isOrphan(block *types.Block) bool {
	prevhash := block.Header.PrevBlockHash
	_, err := cs.getBlock(prevhash)

	return err != nil
}

func (cs *ChainService) handleOrphan(block *types.Block, bestBlock *types.Block, peerID peer.ID) error {
	err := cs.addOrphan(block)
	if err != nil {
		// logging???
		logger.Debug().Str("hash", block.ID()).Msg("add Orphan Block failed")

		return err
	}

	if cs.cfg.Blockchain.UseFastSyncer {
		cs.RequestTo(message.SyncerSvc, &message.SyncStart{PeerID: peerID, TargetNo: block.GetHeader().GetBlockNo()})
	} else {
		// request missing
		orphanNo := block.GetHeader().GetBlockNo()
		bestNo := bestBlock.GetHeader().GetBlockNo()
		if block.GetHeader().GetBlockNo() < bestBlock.GetHeader().GetBlockNo()+1 {
			logger.Debug().Str("hash", block.ID()).Uint64("orphanNo", orphanNo).Uint64("bestNo", bestNo).
				Msg("skip sync with too old block")
			return nil
		}
		anchors := cs.getAnchorsFromHash(block.BlockHash())
		hashes := make([]message.BlockHash, 0)
		for _, a := range anchors {
			hashes = append(hashes, message.BlockHash(a))
		}
		cs.RequestTo(message.P2PSvc, &message.GetMissingBlocks{ToWhom: peerID, Hashes: hashes})
	}

	return nil
}

func (cs *ChainService) addOrphan(block *types.Block) error {
	return cs.op.addOrphan(block)
}

// TODO adhoc flag refactor it
const HashNumberUnknown = math.MaxUint64

//
func (cs *ChainService) handleMissing(stopHash []byte, Hashes [][]byte) (message.BlockHash, types.BlockNo, types.BlockNo) {
	// 1. check endpoint is on main chain (or, return nil)
	logger.Debug().Str("stop_hash", enc.ToString(stopHash)).Int("len", len(Hashes)).Msg("handle missing")
	var stopBlock *types.Block
	var err error
	if stopHash == nil {
		stopBlock, err = cs.GetBestBlock()
	} else {
		stopBlock, err = cs.cdb.getBlock(stopHash)
	}
	if err != nil {
		return nil, HashNumberUnknown, HashNumberUnknown
	}

	var mainhash []byte
	var mainblock *types.Block
	// 2. get the highest block of Hashes hash on main chain
	for _, hash := range Hashes {
		// need to be short
		mainblock, err = cs.cdb.getBlock(hash)
		if err != nil {
			continue
		}
		// get main hash with same block height
		mainhash, err = cs.cdb.getHashByNo(
			types.BlockNo(mainblock.GetHeader().GetBlockNo()))
		if err != nil {
			continue
		}

		if bytes.Equal(mainhash, mainblock.BlockHash()) {
			break
		}
		mainblock = nil
	}

	// TODO: handle the case that can't find the hash in main chain
	if mainblock == nil {
		logger.Debug().Msg("Can't search same ancestor")
		return nil, HashNumberUnknown, HashNumberUnknown
	}

	return mainblock.GetHash(), mainblock.GetHeader().GetBlockNo(), stopBlock.GetHeader().GetBlockNo()
}

func (cs *ChainService) findAncestor(Hashes [][]byte) (*types.BlockInfo, error) {
	// 1. check endpoint is on main chain (or, return nil)
	logger.Debug().Int("len", len(Hashes)).Msg("find ancestor")

	var mainhash []byte
	var mainblock *types.Block
	var err error
	// 2. get the highest block of Hashes hash on main chain
	for _, hash := range Hashes {
		// need to be short
		mainblock, err = cs.cdb.getBlock(hash)
		if err != nil {
			continue
		}
		// get main hash with same block height
		mainhash, err = cs.cdb.getHashByNo(
			types.BlockNo(mainblock.GetHeader().GetBlockNo()))
		if err != nil {
			continue
		}

		if bytes.Equal(mainhash, mainblock.BlockHash()) {
			break
		}
		mainblock = nil
	}

	// TODO: handle the case that can't find the hash in main chain
	if mainblock == nil {
		logger.Debug().Msg("Can't search same ancestor")
		return nil, ErrorNoAncestor
	}

	return &types.BlockInfo{Hash: mainblock.GetHash(), No: mainblock.GetHeader().GetBlockNo()}, nil
}

func (cs *ChainService) checkBlockHandshake(peerID peer.ID, remoteBestHeight uint64, remoteBestHash []byte) {
	myBestBlock, err := cs.GetBestBlock()
	if err != nil {
		logger.Error().Err(err).Msg("Failed to get best block")
		return
	}
	sameBestHash := bytes.Equal(myBestBlock.Hash, remoteBestHash)
	if sameBestHash {
		// two node has exact best block.
		// TODO: myBestBlock.GetHeader().BlockNo == remoteBestHeight
		logger.Debug().Str("peer", peerID.Pretty()).Msg("peer is in sync status")
	} else if !sameBestHash && myBestBlock.GetHeader().BlockNo < remoteBestHeight {
		cs.ChainSync(peerID, remoteBestHash)
	}

	return
}
