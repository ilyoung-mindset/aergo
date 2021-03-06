package contract

// helper functions
import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io/ioutil"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/aergoio/aergo-lib/db"
	luac_util "github.com/aergoio/aergo/cmd/aergoluac/util"
	"github.com/aergoio/aergo/state"
	"github.com/aergoio/aergo/types"
	"github.com/minio/sha256-simd"
)

type DummyChain struct {
	sdb           *state.ChainStateDB
	bestBlock     *types.Block
	cBlock        *types.Block
	bestBlockNo   types.BlockNo
	bestBlockId   types.BlockID
	blockIds      []types.BlockID
	blocks        []*types.Block
	testReceiptDB db.DB
}

func LoadDummyChain() (*DummyChain, error) {
	bc := &DummyChain{sdb: state.NewChainStateDB()}
	dataPath, err := ioutil.TempDir("", "data")
	if err != nil {
		return nil, err
	}

	err = bc.sdb.Init(string(db.BadgerImpl), dataPath, nil, false)
	if err != nil {
		return nil, err
	}
	genesis := types.GetTestGenesis()
	bc.sdb.SetGenesis(genesis)
	bc.bestBlockNo = genesis.Block().BlockNo()
	bc.bestBlockId = genesis.Block().BlockID()
	bc.blockIds = append(bc.blockIds, bc.bestBlockId)
	bc.blocks = append(bc.blocks, genesis.Block())
	bc.testReceiptDB = db.NewDB(db.BadgerImpl, path.Join(dataPath, "receiptDB"))
	LoadDatabase(dataPath) // sql database

	return bc, nil
}

func (bc *DummyChain) BestBlockNo() uint64 {
	return bc.bestBlockNo
}

func (bc *DummyChain) newBState() *state.BlockState {
	b := types.Block{
		Header: &types.BlockHeader{
			PrevBlockHash: []byte(bc.bestBlockId.String()),
			BlockNo:       bc.bestBlockNo + 1,
			Timestamp:     time.Now().Unix(),
		},
	}
	bc.cBlock = &b
	// blockInfo := types.NewBlockInfo(b.BlockNo(), b.BlockID(), bc.bestBlockId)
	return state.NewBlockState(bc.sdb.OpenNewStateDB(bc.sdb.GetRoot()))
}

func (bc *DummyChain) BeginReceiptTx() db.Transaction {
	return bc.testReceiptDB.NewTx()
}

func (bc *DummyChain) GetABI(contract string) (*types.ABI, error) {
	cState, err := bc.sdb.GetStateDB().OpenContractStateAccount(types.ToAccountID(strHash(contract)))
	if err != nil {
		return nil, err
	}
	return GetABI(cState)
}

func (bc *DummyChain) getReceipt(txHash []byte) *types.Receipt {
	r := new(types.Receipt)
	r.UnmarshalBinary(bc.testReceiptDB.Get(txHash))
	return r
}

func (bc *DummyChain) GetAccountState(name string) (*types.State, error) {
	return bc.sdb.GetStateDB().GetAccountState(types.ToAccountID(strHash(name)))
}

type luaTx interface {
	run(bs *state.BlockState, blockNo uint64, ts int64, receiptTx db.Transaction) error
}

type luaTxAccount struct {
	name    []byte
	balance uint64
}

func NewLuaTxAccount(name string, balance uint64) *luaTxAccount {
	return &luaTxAccount{
		name:    strHash(name),
		balance: balance,
	}
}

func (l *luaTxAccount) run(bs *state.BlockState, blockNo uint64, ts int64,
	receiptTx db.Transaction) error {

	id := types.ToAccountID(l.name)
	accountState, err := bs.GetAccountState(id)
	if err != nil {
		return err
	}
	updatedAccountState := types.State(*accountState)
	updatedAccountState.Balance = l.balance
	bs.PutState(id, &updatedAccountState)
	return nil
}

type luaTxSend struct {
	sender   []byte
	receiver []byte
	balance  uint64
}

func NewLuaTxSend(sender, receiver string, balance uint64) *luaTxSend {
	return &luaTxSend{
		sender:   strHash(sender),
		receiver: strHash(receiver),
		balance:  balance,
	}
}

func (l *luaTxSend) run(bs *state.BlockState, blockNo uint64, ts int64,
	receiptTx db.Transaction) error {

	senderID := types.ToAccountID(l.sender)
	receiverID := types.ToAccountID(l.receiver)

	if senderID == receiverID {
		return fmt.Errorf("sender and receiever cannot be same")
	}

	senderState, err := bs.GetAccountState(senderID)
	if err != nil {
		return err
	} else if senderState.GetBalance() < l.balance {
		return fmt.Errorf("insufficient balance to sender")
	}
	receiverState, err := bs.GetAccountState(receiverID)
	if err != nil {
		return err
	}

	updatedSenderState := types.State(*senderState)
	updatedSenderState.Balance = updatedSenderState.Balance - l.balance
	bs.PutState(senderID, &updatedSenderState)

	updatedReceiverState := types.State(*receiverState)
	updatedReceiverState.Balance = updatedReceiverState.Balance + l.balance
	bs.PutState(receiverID, &updatedReceiverState)

	return nil
}

type luaTxCommon struct {
	sender   []byte
	contract []byte
	amount   uint64
	code     []byte
	id       uint64
}

type luaTxDef struct {
	luaTxCommon
	cErr error
}

func NewLuaTxDef(sender, contract string, amount uint64, code string) *luaTxDef {
	b, err := luac_util.Compile(code)
	if err != nil {
		return &luaTxDef{cErr: err}
	}
	codeWithInit := make([]byte, 4+len(b))
	binary.LittleEndian.PutUint32(codeWithInit, uint32(4+len(b)))
	copy(codeWithInit[4:], b)
	return &luaTxDef{
		luaTxCommon: luaTxCommon{
			sender:   strHash(sender),
			contract: strHash(contract),
			code:     codeWithInit,
			amount:   amount,
			id:       newTxId(),
		},
		cErr: nil,
	}
}

func strHash(d string) []byte {
	h := sha256.New()
	h.Write([]byte(d))
	b := h.Sum(nil)
	b = append([]byte{0x0C}, b...)
	return b
}

var luaTxId uint64 = 0

func newTxId() uint64 {
	luaTxId++
	return luaTxId
}

func (l *luaTxDef) hash() []byte {
	h := sha256.New()
	h.Write([]byte(strconv.FormatUint(l.id, 10)))
	b := h.Sum(nil)
	b = append([]byte{0x0C}, b...)
	return b
}

func (l *luaTxDef) Constructor(args string) *luaTxDef {
	argsLen := len([]byte(args))
	if argsLen == 0 {
		return l
	}

	code := make([]byte, len(l.code)+argsLen)
	codeLen := copy(code[0:], l.code)
	binary.LittleEndian.PutUint32(code[0:], uint32(codeLen))
	copy(code[codeLen:], []byte(args))

	l.code = code

	return l
}

func contractFrame(l *luaTxCommon, bs *state.BlockState,
	run func(s, c *types.State, id types.AccountID, cs *state.ContractState) error) error {

	creatorId := types.ToAccountID(l.sender)
	creatorState, err := bs.GetAccountState(creatorId)
	if err != nil {
		return err
	}

	contractId := types.ToAccountID(l.contract)
	contractState, err := bs.GetAccountState(contractId)
	if err != nil {
		return err
	}

	uContractState := types.State(*contractState)
	eContractState, err := bs.OpenContractState(contractId, &uContractState)
	if err != nil {
		return err
	}

	err = run(creatorState, &uContractState, contractId, eContractState)
	if err != nil {
		return err
	}

	uCallerState := types.State(*creatorState)
	uCallerState.Balance -= l.amount
	uContractState.Balance += l.amount

	bs.PutState(creatorId, &uCallerState)
	bs.PutState(contractId, &uContractState)
	return nil

}

func (l *luaTxDef) run(bs *state.BlockState, blockNo uint64, ts int64,
	receiptTx db.Transaction) error {

	if l.cErr != nil {
		return l.cErr
	}

	return contractFrame(&l.luaTxCommon, bs,
		func(senderState, uContractState *types.State, contractId types.AccountID, eContractState *state.ContractState) error {
			uContractState.SqlRecoveryPoint = 1
			bcCtx := NewContext(bs, senderState, eContractState,
				types.EncodeAddress(l.sender), hex.EncodeToString(l.hash()), blockNo, ts,
				"", 1, types.EncodeAddress(l.contract),
				0, nil, uContractState.SqlRecoveryPoint, ChainService, l.luaTxCommon.amount)

			_, err := Create(eContractState, l.code, l.contract, bcCtx)
			if err != nil {
				return err
			}
			err = bs.StageContractState(eContractState)
			if err != nil {
				return err
			}
			return nil
		},
	)
}

type luaTxCall struct {
	luaTxCommon
	expectedErr string
}

func NewLuaTxCall(sender, contract string, amount uint64, code string) *luaTxCall {
	return &luaTxCall{
		luaTxCommon: luaTxCommon{
			sender:   strHash(sender),
			contract: strHash(contract),
			amount:   amount,
			code:     []byte(code),
			id:       newTxId(),
		},
	}
}

func (l *luaTxCall) hash() []byte {
	h := sha256.New()
	h.Write([]byte(strconv.FormatUint(l.id, 10)))
	b := h.Sum(nil)
	b = append([]byte{0x0C}, b...)
	return b
}

func (l *luaTxCall) fail(expectedErr string) *luaTxCall {
	l.expectedErr = expectedErr
	return l
}

func (l *luaTxCall) run(bs *state.BlockState, blockNo uint64, ts int64, receiptTx db.Transaction) error {
	err := contractFrame(&l.luaTxCommon, bs,
		func(senderState, uContractState *types.State, contractId types.AccountID, eContractState *state.ContractState) error {
			bcCtx := NewContext(bs, senderState, eContractState,
				types.EncodeAddress(l.sender), hex.EncodeToString(l.hash()), blockNo, ts,
				"", 1, types.EncodeAddress(l.contract),
				0, nil, uContractState.SqlRecoveryPoint, ChainService, l.luaTxCommon.amount)
			rv, err := Call(eContractState, l.code, l.contract, bcCtx)
			if err != nil {
				return err
			}
			err = bs.StageContractState(eContractState)
			if err != nil {
				r := types.NewReceipt(l.contract, err.Error(), "")
				b, _ := r.MarshalBinary()
				receiptTx.Set(l.hash(), b)
				return err
			}
			r := types.NewReceipt(l.contract, "SUCCESS", rv)
			b, _ := r.MarshalBinary()
			receiptTx.Set(l.hash(), b)
			return nil
		},
	)
	if l.expectedErr != "" {
		if err == nil || !strings.Contains(err.Error(), l.expectedErr) {
			return err
		}
		return nil
	}
	return err
}

func (bc *DummyChain) ConnectBlock(txs ...luaTx) error {
	blockState := bc.newBState()
	tx := bc.BeginReceiptTx()
	defer tx.Commit()

	for _, x := range txs {
		if err := x.run(blockState, bc.cBlock.Header.BlockNo, bc.cBlock.Header.Timestamp, tx); err != nil {
			return err
		}
	}
	err := SaveRecoveryPoint(blockState)
	if err != nil {
		return err
	}
	err = bc.sdb.Apply(blockState)
	if err != nil {
		return err
	}
	//FIXME newblock must be created after sdb.apply()
	bc.cBlock.SetBlocksRootHash(bc.sdb.GetRoot())
	bc.bestBlockNo = bc.bestBlockNo + 1
	bc.bestBlockId = types.ToBlockID(bc.cBlock.BlockHash())
	bc.blockIds = append(bc.blockIds, bc.bestBlockId)
	bc.blocks = append(bc.blocks, bc.cBlock)

	return nil
}

func (bc *DummyChain) DisConnectBlock() error {
	if len(bc.blockIds) == 1 {
		return errors.New("genesis block")
	}
	bc.bestBlockNo--
	bc.blockIds = bc.blockIds[0 : len(bc.blockIds)-1]
	bc.blocks = bc.blocks[0 : len(bc.blocks)-1]
	bc.bestBlockId = bc.blockIds[len(bc.blockIds)-1]

	bestBlock := bc.blocks[len(bc.blocks)-1]

	var sroot []byte
	if bestBlock != nil {
		sroot = bestBlock.GetHeader().GetBlocksRootHash()
	}
	return bc.sdb.Rollback(sroot)
}

func (bc *DummyChain) Query(contract, queryInfo, expectedErr string, expectedRvs ...string) error {
	cState, err := bc.sdb.GetStateDB().OpenContractStateAccount(types.ToAccountID(strHash(contract)))
	if err != nil {
		return err
	}
	rv, err := Query(strHash(contract), bc.newBState(), cState, []byte(queryInfo))
	if expectedErr != "" {
		if err == nil || !strings.Contains(err.Error(), expectedErr) {
			return err
		}
		return nil
	}
	if err != nil {
		return err
	}

	for _, ev := range expectedRvs {
		if ev != string(rv) {
			err = fmt.Errorf("expected: %s, but got: %s", ev, string(rv))
		} else {
			return nil
		}
	}
	return err
}

func (bc *DummyChain) QueryOnly(contract, queryInfo string) (string, error) {
	cState, err := bc.sdb.GetStateDB().OpenContractStateAccount(types.ToAccountID(strHash(contract)))
	if err != nil {
		return "", err
	}
	rv, err := Query(strHash(contract), bc.newBState(), cState, []byte(queryInfo))

	if err != nil {
		return "", err
	}

	return string(rv), nil
}

func StrToAddress(name string) string {
	return types.EncodeAddress(strHash(name))
}
