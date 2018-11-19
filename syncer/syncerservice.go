package syncer

import (
	"github.com/aergoio/aergo-lib/log"
	cfg "github.com/aergoio/aergo/config"
	"github.com/aergoio/aergo/pkg/component"

	"fmt"
	"github.com/aergoio/aergo-actor/actor"
	"github.com/aergoio/aergo/message"
	"github.com/aergoio/aergo/types"
	"github.com/pkg/errors"
	"reflect"
)

type Syncer struct {
	*component.BaseComponent

	cfg   *cfg.Config
	chain types.ChainAccessor

	isstartning bool
	ctx         *types.SyncContext

	finder       *Finder
	hashFetcher  *HashFetcher
	blockFetcher *BlockFetcher
}

var (
	logger             = log.NewLogger("syncer")
	NameHashFetcher    = "HashFetcher"
	NameBlockFetcher   = "BlockFetcher"
	NameBlockProcessor = "BlockProcessor"
)

var (
	ErrFinderInternal = errors.New("error finder internal")
)

type ErrSyncMsg struct {
	msg interface{}
	str string
}

func (ec *ErrSyncMsg) Error() string {
	return fmt.Sprintf("Error sync message: type=%T, desc=%s", ec.msg, ec.str)
}

func NewSyncer(cfg *cfg.Config, chain types.ChainAccessor) *Syncer {
	syncer := &Syncer{cfg: cfg}

	syncer.BaseComponent = component.NewBaseComponent(message.SyncerSvc, syncer, logger)

	syncer.chain = chain

	logger.Info().Msg("Syncer started")

	return syncer
}

// BeforeStart initialize chain database and generate empty genesis block if necessary
func (syncer *Syncer) BeforeStart() {
}

// AfterStart ... do nothing
func (syncer *Syncer) AfterStart() {

}

func (syncer *Syncer) BeforeStop() {
}

func (syncer *Syncer) Reset() {
	if syncer.isstartning {
		syncer.finder.stop()
		syncer.hashFetcher.stop()
		syncer.blockFetcher.stop()

		syncer.finder = nil
		syncer.hashFetcher = nil
		syncer.blockFetcher = nil
		syncer.isstartning = false
		syncer.ctx = nil
	}

	logger.Info().Msg("syncer stopped")
}

// Receive actor message
func (syncer *Syncer) Receive(context actor.Context) {
	//drop garbage message
	if !syncer.isstartning {
		switch context.Message().(type) {
		case *message.GetSyncAncestorRsp:
			return
		case *message.FinderResult:
			return
		case *message.GetHashesRsp:
			return
		case *message.GetBlockChunksRsp:
			return
		case *message.AddBlockRsp:
			return
		case *message.SyncStop:
			return
		}
	}

	switch msg := context.Message().(type) {
	case *message.SyncStart:
		err := syncer.handleSyncStart(msg)
		if err != nil {
			logger.Error().Err(err).Msg("SyncStart failed")
		}
	case *message.GetSyncAncestorRsp:
		syncer.handleAncestorRsp(msg)

	case *message.FinderResult:
		err := syncer.handleFinderResult(msg)
		if err != nil {
			syncer.Reset()
			logger.Error().Err(err).Msg("FinderResult failed")
		}
	case *message.GetHashesRsp:
		syncer.hashFetcher.GetHahsesRsp(msg)

	case *message.GetBlockChunksRsp:
		err := syncer.blockFetcher.handleBlockRsp(msg)
		if err != nil {
			syncer.Reset()
			logger.Error().Err(err).Msg("GetBlockChunksRsp failed")
		}
	case *message.AddBlockRsp:
		err := syncer.blockFetcher.handleBlockRsp(msg)
		if err != nil {
			syncer.Reset()
			logger.Error().Err(err).Msg("AddBlockRsp failed")
		}
	case *message.SyncStop:
		if msg.Err == nil {
			logger.Info().Str("from", msg.FromWho).Err(msg.Err).Msg("Syncer succeed")
		} else {
			logger.Info().Str("from", msg.FromWho).Err(msg.Err).Msg("Syncer finished by error")
		}
		syncer.Reset()
	case *message.CloseFetcher:
		if msg.FromWho == NameHashFetcher {
			syncer.hashFetcher.stop()
		} else if msg.FromWho == NameBlockFetcher {
			syncer.blockFetcher.stop()
		} else {
			logger.Error().Msg("invalid closing module message to syncer")
		}
	case actor.SystemMessage,
		actor.AutoReceiveMessage,
		actor.NotInfluenceReceiveTimeout:
		str := fmt.Sprintf("Received message. (%v) %s", reflect.TypeOf(msg), msg)
		logger.Debug().Msg(str)

	default:
		str := fmt.Sprintf("Missed message. (%v) %s", reflect.TypeOf(msg), msg)
		logger.Debug().Msg(str)
	}
}

func (syncer *Syncer) handleSyncStart(msg *message.SyncStart) error {
	var err error
	var bestBlock *types.Block

	logger.Debug().Uint64("targetNo", msg.TargetNo).Msg("sync requested")

	if syncer.isstartning {
		logger.Debug().Uint64("targetNo", msg.TargetNo).Msg("skipped syncer is running")
		return nil
	}

	//TODO skip sync in reorgnizing
	bestBlock, _ = syncer.chain.GetBestBlock()
	if err != nil {
		logger.Error().Err(err).Msg("error getting block in syncer")
		return err
	}

	bestBlockNo := bestBlock.GetHeader().BlockNo

	if msg.TargetNo <= bestBlockNo {
		logger.Debug().Uint64("targetNo", msg.TargetNo).Uint64("bestNo", bestBlockNo).
			Msg("skipped syncer. requested no is too low")
		return nil
	}

	logger.Info().Uint64("targetNo", msg.TargetNo).Uint64("bestNo", bestBlockNo).Msg("sync started")

	//TODO BP stop
	syncer.ctx = types.NewSyncCtx(msg.PeerID, msg.TargetNo, bestBlockNo)
	syncer.isstartning = true

	syncer.finder = newFinder(syncer.ctx, syncer.Hub())

	return err
}

func (syncer *Syncer) handleAncestorRsp(msg *message.GetSyncAncestorRsp) {
	logger.Debug().Msg("syncer received ancestor response")

	//set ancestor in types.SyncContext
	syncer.finder.lScanCh <- msg.Ancestor
}

func (syncer *Syncer) handleFinderResult(msg *message.FinderResult) error {
	logger.Debug().Msg("syncer received finder result message")

	if msg.Err != nil {
		logger.Error().Err(msg.Err).Msg("Find Ancestor failed")
		return ErrFinderInternal
	}

	ancestor, err := syncer.chain.GetBlock(msg.Ancestor.Hash)
	if err != nil {
		logger.Error().Err(err).Msg("error getting ancestor block in syncer")
		return err
	}

	//set ancestor in types.SyncContext
	syncer.ctx.SetAncestor(ancestor)

	syncer.finder.stop()
	syncer.finder = nil

	syncer.blockFetcher = newBlockFetcher(syncer.ctx, syncer.Hub(), DfltBlockFetchSize, DfltBlockFetchTasks, MaxBlockPendingTasks)
	syncer.hashFetcher = newHashFetcher(syncer.ctx, syncer.Hub(), syncer.blockFetcher.hfCh, DfltHashReqSize)

	syncer.blockFetcher.Start()
	syncer.hashFetcher.Start()

	return nil
}

func (syncer *Syncer) Statistics() *map[string]interface{} {
	var start, end, total, added, blockfetched uint64

	if syncer.ctx != nil {
		end = syncer.ctx.TargetNo
		if syncer.ctx.CommonAncestor != nil {
			total = syncer.ctx.TotalCnt
			start = syncer.ctx.CommonAncestor.BlockNo()
		}
	}

	if syncer.blockFetcher != nil {
		lastblock := syncer.blockFetcher.stat.getLastAddBlock()
		added = lastblock.BlockNo()
		if syncer.blockFetcher.stat.getMaxChunkRsp() != nil {
			blockfetched = syncer.blockFetcher.stat.getMaxChunkRsp().BlockNo()
		}
	}

	return &map[string]interface{}{
		"startning":    syncer.isstartning,
		"total":        total,
		"start":        start,
		"end":          end,
		"added":        added,
		"blockfetched": blockfetched,
	}
}

func stopSyncer(hub component.ICompRequester, who string, err error) {
	logger.Info().Str("who", who).Err(err).Msg("request syncer stop")

	hub.Tell(message.SyncerSvc, &message.SyncStop{FromWho: who, Err: err})
}

func closeFetcher(hub component.ICompRequester, who string) {
	hub.Tell(message.SyncerSvc, &message.CloseFetcher{FromWho: who})
}
