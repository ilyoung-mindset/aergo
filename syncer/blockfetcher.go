package syncer

import (
	"bytes"
	"container/list"
	"errors"
	"fmt"
	"github.com/aergoio/aergo/internal/enc"
	"github.com/aergoio/aergo/message"
	"github.com/aergoio/aergo/pkg/component"
	"github.com/aergoio/aergo/types"
	"github.com/libp2p/go-libp2p-peer"
	"time"
)

type BlockFetcher struct {
	hub *component.ComponentHub //for communicate with other service

	ctx *types.SyncContext

	quitCh chan interface{}

	hfCh chan *HashSet

	curHashSet *HashSet

	runningQueue TaskQueue
	pendingQueue TaskQueue

	responseCh chan interface{} //BlockResponse, AddBlockResponse message
	peers      *PeerSet

	nextTask *FetchTask

	blockProcessor *BlockProcessor

	name string
}

type SyncPeer struct {
	No      int
	ID      peer.ID
	FailCnt int
}

type TaskQueue struct {
	list.List
}

func (tq *TaskQueue) Pop() *FetchTask {
	elem := tq.Front()
	if elem == nil {
		return nil
	}

	return elem.Value.(*FetchTask)
}

type FetchTask struct {
	count  int
	hashes []message.BlockHash

	syncPeer *SyncPeer

	started time.Time
}

func (task *FetchTask) isTimeOut(now time.Time) bool {
	if now.Sub(task.started) > fetchTimeOut {
		logger.Info().Str("peer", task.syncPeer.ID.String()).Str("start", enc.ToString(task.hashes[0])).Int("cout", task.count).Msg("FetchTask peer timeouted")
		return true
	}

	return false
}

func (task *FetchTask) isMatched(peerID peer.ID, blocks []*types.Block, count int) bool {
	startHash, endHash := blocks[0].GetHash(), blocks[len(blocks)-1].GetHash()

	if task.count != count ||
		task.syncPeer.ID != peerID ||
		bytes.Compare(task.hashes[0], startHash) != 0 ||
		bytes.Compare(task.hashes[len(task.hashes)-1], endHash) != 0 {
		return false
	}

	for i, block := range blocks {
		if bytes.Compare(task.hashes[i], block.GetHash()) != 0 {
			logger.Info().Str("peer", task.syncPeer.ID.String()).Str("hash", enc.ToString(task.hashes[0])).Int("idx", i).Msg("task mismatch")
			return false
		}
	}

	logger.Info().Msg("task matched")

	return true
}

type PeerSet struct {
	total int
	free  int
	bad   int

	freePeers *list.List
	badPeers  *list.List
}

var (
	schedTick        = time.Millisecond * 100
	fetchTimeOut     = time.Second * 100
	MaxFetchTask     = 16
	MaxPeerFailCount = 1
)

func newBlockFetcher(ctx *types.SyncContext, hub *component.ComponentHub) *BlockFetcher {
	bf := &BlockFetcher{ctx: ctx, hub: hub, name: "BlockFetcher"}

	bf.quitCh = make(chan interface{})
	bf.hfCh = make(chan *HashSet)

	bf.peers = newPeerSet()

	bf.blockProcessor = &BlockProcessor{
		hub:           hub,
		blockFetcher:  bf,
		prevBlock:     &types.Block{Hash: ctx.CommonAncestor.Hash},
		targetBlockNo: ctx.TargetNo,
		name:          "BlockProducer",
	}
	bf.blockProcessor.pendingConnect = make([]*ConnectRequest, 0, 16)
	return bf
}

func (bf *BlockFetcher) Start() {
	schedTicker := time.NewTicker(schedTick)

	run := func() {
		if err := bf.init(); err != nil {
			stopSyncer(bf.hub, bf.name, err)
			return
		}

		for {
			select {
			case <-schedTicker.C:
				bf.checkTaskTimeout()

			case msg := <-bf.responseCh:
				err := bf.blockProcessor.run(msg)
				if err != nil {
					logger.Error().Err(err).Msg("invalid block response message")
					stopSyncer(bf.hub, bf.name, err)
					return
				}

			case <-bf.quitCh:
				logger.Info().Msg("BlockFetcher exited")
				return
			}

			if err := bf.schedule(); err != nil {
				logger.Error().Msg("BlockFetcher schedule failed & stopped")
				stopSyncer(bf.hub, bf.name, err)
				return
			}
		}
	}

	go run()
}

func (bf *BlockFetcher) init() error {
	setPeers := func() error {
		result, err := bf.hub.RequestFuture(message.P2PSvc, &message.GetPeers{}, dfltTimeout, "BlockFetcher init").Result()
		if err != nil {
			logger.Error().Err(err).Msg("failed to get peers information")
			return err
		}

		for i, peerElem := range result.(message.GetPeersRsp).Peers {
			state := result.(message.GetPeersRsp).States[i]
			if state.Get() == types.RUNNING {
				bf.peers.addNew(peer.ID(peerElem.PeerID))
			}
		}

		if bf.peers.freePeers.Len() != bf.peers.free {
			panic(fmt.Sprintf("free peer len mismatch %d,%d", bf.peers.freePeers.Len(), bf.peers.free))
		}

		return nil
	}

	if err := setPeers(); err != nil {
		return err
	}

	return nil
}

func (bf *BlockFetcher) schedule() error {
	task, err := bf.setNextTask()
	if err != nil {
		logger.Error().Err(err).Msg("error to get next task")
		return err
	}
	if task == nil {
		return nil
	}

	freePeer, err := bf.popFreePeer()
	if err != nil {
		logger.Error().Err(err).Msg("error to get free peer")
		return err
	}
	if freePeer == nil {
		return nil
	}

	bf.runTask(task, freePeer)

	return nil
}

var (
	ErrQuit = errors.New("BlockFetcher: stopped by Quit runTask")
)

func (bf *BlockFetcher) checkTaskTimeout() {
	now := time.Now()
	var next *list.Element
	for e := bf.runningQueue.Front(); e != nil; e = next {
		// do something with e.Value
		task := e.Value.(*FetchTask)
		if !task.isTimeOut(now) {
			continue
		}

		next = e.Next()

		bf.runningQueue.Remove(e)

		failPeer := task.syncPeer
		bf.peers.processPeerFail(failPeer)

		task.syncPeer = nil
		bf.pendingQueue.PushFront(task)
		logger.Debug().Str("start", enc.ToString(task.hashes[0])).Int("cout", task.count).
			Msg("timeouted task pushed to pending queue")
	}
}

func (bf *BlockFetcher) setNextTask() (*FetchTask, error) {
	getNewHashSet := func() (*HashSet, error) {
		if bf.curHashSet == nil {
			logger.Info().Msg("BlockFetcher waiting first hashset")

			select {
			case hashSet := <-bf.hfCh:
				return hashSet, nil
			case <-bf.quitCh:
				return nil, ErrQuit
			}
		} else {
			select {
			case hashSet := <-bf.hfCh:
				logger.Debug().Str("start", enc.ToString(hashSet.Hashes[0])).Int("count", hashSet.Count).
					Msg("BlockFetcher got hashset")

				return hashSet, nil
			case <-bf.quitCh:
				return nil, ErrQuit
			default:
				return nil, nil
			}
		}
	}

	addNewTasks := func(hashSet *HashSet) {
		start, end := 0, 0
		count := hashSet.Count

		logger.Debug().Str("start", enc.ToString(hashSet.Hashes[0])).Int("count", hashSet.Count).Msg("addNew fetchtasks from HashSet")

		for start < count {
			end = start + MaxFetchTask
			if end > count {
				end = count
			}

			task := &FetchTask{count: end - start, hashes: hashSet.Hashes[start:end]}

			logger.Debug().Int("startNo", start).Int("end", end).Msg("addNew fetchtask")

			bf.pendingQueue.PushBack(task)

			start = end
		}
	}

	if bf.nextTask != nil {
		return bf.nextTask, nil
	}

	if bf.pendingQueue.Len() == 0 {
		hashSet, err := getNewHashSet()
		if err != nil {
			return nil, err
		}

		if hashSet == nil {
			logger.Debug().Msg("BlockFetcher no hashSet")
			return nil, nil
		}

		bf.curHashSet = hashSet
		addNewTasks(hashSet)
	}

	//newTask = nil or task
	newTask := bf.pendingQueue.Pop()

	bf.nextTask = newTask
	return newTask, nil
}

var (
	ErrAllPeerBad = errors.New("BlockFetcher: error no avaliable peers")
)

func (bf *BlockFetcher) popFreePeer() (*SyncPeer, error) {
	freePeer, err := bf.peers.popFree()
	if err != nil {
		return nil, err
	}

	bf.nextTask.syncPeer = freePeer

	return freePeer, nil
}

func (bf *BlockFetcher) pushFreePeer(syncPeer *SyncPeer) {
	bf.peers.pushFree(syncPeer)
}

func (bf *BlockFetcher) runTask(task *FetchTask, peer *SyncPeer) {
	task.started = time.Now()
	bf.runningQueue.PushBack(task)
	bf.nextTask = nil

	bf.hub.Tell(message.P2PSvc, &message.GetBlockChunks{GetBlockInfos: message.GetBlockInfos{ToWhom: peer.ID, Hashes: task.hashes}, TTL: fetchTimeOut})
}

func (bf *BlockFetcher) stop() {
	if bf == nil {
		return
	}

	if bf.quitCh != nil {
		close(bf.quitCh)
		bf.quitCh = nil

		close(bf.hfCh)
		bf.hfCh = nil
	}
}

func newPeerSet() *PeerSet {
	ps := &PeerSet{}

	ps.freePeers = list.New()
	ps.badPeers = list.New()

	return ps
}

func (ps *PeerSet) isAllBad() bool {
	if ps.total == ps.badPeers.Len() {
		return true
	}

	return false
}

func (ps *PeerSet) addNew(peerID peer.ID) {
	ps.pushFree(&SyncPeer{No: ps.total, ID: peerID})
	ps.total++

	logger.Info().Str("peer", peerID.String()).Int("no", ps.total).Msg("new peer added")
}

func (ps *PeerSet) pushFree(freePeer *SyncPeer) {
	ps.freePeers.PushBack(freePeer)
	ps.free++

	logger.Info().Int("no", freePeer.No).Int("free", ps.free).Msg("free peer added")
}

func (ps *PeerSet) popFree() (*SyncPeer, error) {
	if ps.isAllBad() {
		return nil, ErrAllPeerBad
	}

	elem := ps.freePeers.Front()
	if elem == nil {
		return nil, nil
	}

	ps.freePeers.Remove(elem)
	ps.free--

	if ps.freePeers.Len() != ps.free {
		panic(fmt.Sprintf("free peer len mismatch %d,%d", ps.freePeers.Len(), ps.free))
	}

	freePeer := elem.Value.(*SyncPeer)
	logger.Debug().Str("peer", freePeer.ID.String()).Int("no", freePeer.No).Msg("pop free peer")
	return freePeer, nil
}

func (ps *PeerSet) processPeerFail(failPeer *SyncPeer) {
	//TODO handle connection closed
	failPeer.FailCnt++
	if failPeer.FailCnt > MaxPeerFailCount {
		ps.badPeers.PushBack(failPeer)
		ps.bad++

		if ps.badPeers.Len() != ps.bad {
			panic(fmt.Sprintf("bad peer len mismatch %d,%d", ps.badPeers.Len(), ps.bad))
		}
	}
}

func (bf *BlockFetcher) findFinished(msg *message.BlockInfosResponse) (*FetchTask, error) {
	count := len(msg.Blocks)

	if count == 0 || msg.FromWhom == "" {
		return nil, &ErrSyncMsg{msg: msg}
	}

	var next *list.Element
	for e := bf.runningQueue.Front(); e != nil; e = next {
		// do something with e.Value
		task := e.Value.(*FetchTask)
		next = e.Next()

		if task.isMatched(msg.FromWhom, msg.Blocks, count) {
			bf.runningQueue.Remove(e)

			logger.Debug().Str("start", enc.ToString(task.hashes[0])).Int("cout", task.count).
				Msg("timeouted task pushed to pending queue")

			return task, nil
		}
	}

	return nil, &ErrSyncMsg{msg: msg}
}

func (bf *BlockFetcher) handleBlockRsp(msg interface{}) error {
	if err := bf.isValidResponse(msg); err != nil {
		return err
	}

	bf.responseCh <- msg
	return nil
}

func (bf *BlockFetcher) isValidResponse(msg interface{}) error {
	validateBlockChunksRsp := func(msg *message.GetBlockChunksRsp) error {
		var prev []byte
		blocks := msg.Blocks

		if blocks == nil || len(blocks) == 0 {
			return &ErrSyncMsg{msg: msg, str: "blocks is empty"}
		}

		for _, block := range blocks {
			if prev != nil && !bytes.Equal(prev, block.GetHeader().GetPrevBlockHash()) {
				return &ErrSyncMsg{msg: msg, str: "blocks hash not matched"}
			}

			prev = block.GetHash()
		}
		return nil
	}

	validateAddBlockRsp := func(msg *message.AddBlockRsp) error {
		if msg.BlockHash == nil {
			return &ErrSyncMsg{msg: msg, str: "invalid add block resonse"}
		}

		return nil
	}

	switch msg.(type) {
	case *message.GetBlockChunksRsp:
		if err := validateBlockChunksRsp(msg.(*message.GetBlockChunksRsp)); err != nil {
			return err
		}

	case *message.AddBlockRsp:
		if err := validateAddBlockRsp(msg.(*message.AddBlockRsp)); err != nil {
			return err
		}

	default:
		return fmt.Errorf("invalid msg type:%T", msg)
	}

	return nil
}