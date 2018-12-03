package backend

import (
	"bytes"
	"errors"
	"sync"

	"bitbucket.org/cpchain/chain/commons/log"
	"bitbucket.org/cpchain/chain/consensus"
	"bitbucket.org/cpchain/chain/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/hashicorp/golang-lru"
)

//Type enumerator for FSM action
type action uint8

const (
	noAction action = iota
	broadcastMsgAction
	insertBlockAction
	broadcastAndInsertBlockAction
)

//Type enumerator for FSM output
type dataType uint8

const (
	noType dataType = iota
	headerType
	blockType
	impeachBlockType
)

//Type enumerator for FSM message type
type msgCode uint8

const (
	noMsgCode msgCode = iota
	preprepareMsgCode
	prepareMsgCode
	commitMsgCode
	validateMsgCode
	impeachPreprepareMsgCode
	impeachPrepareMsgCode
	impeachCommitMsgCode
	impeachValidateMsgCode
)

var (
	errBlockTooOld                     = errors.New("the block is too old")
	errFsmWrongDataType                = errors.New("an unexpected FSM input data type")
	errFsmFaultyBlock                  = errors.New("the newly proposed block is faulty")
	errFsmWrongIdleIpunt               = errors.New("not a proper input for idle state")
	errFsmWrongPrepreparedInput        = errors.New("not a proper input for pre-prepared state")
	errFsmWrongPreparedInput           = errors.New("not a proper input for prepared state")
	errFsmWrongImpeachPrepreparedInput = errors.New("not a proper input for impeach pre-prepared state")
	errFsmWrongImpeachPreparedInput    = errors.New("not a proper input for impeach prepared state")
	errBlockNotExist                   = errors.New("the block does not exist")
)

// address -> blockSigItem -> (hash, sig)
type sigState map[common.Address]*blockSigItem

type blockSigItem struct {
	hash common.Hash         // the block's hash
	sig  types.DporSignature // signature of the block
}

const cacheSize = 10

//DporSm is a struct containing variables used for state transition in FSM
type DporSm struct {
	lock      sync.RWMutex
	stateLock sync.RWMutex

	state consensus.State

	service         DporService
	prepareSigState sigState
	commitSigState  sigState
	f               uint64        // f is the parameter of 3f+1 nodes in Byzantine
	bcache          *lru.ARCCache // block cache
	lastHeight      uint64
}

func NewDporSm(service DporService, f uint64) *DporSm {
	bc, _ := lru.NewARC(cacheSize)

	return &DporSm{
		state:           consensus.Idle,
		service:         service,
		prepareSigState: make(map[common.Address]*blockSigItem),
		commitSigState:  make(map[common.Address]*blockSigItem),
		f:               f,
		bcache:          bc,
		lastHeight:      0,
	}
}

// State returns current dpor state
func (sm *DporSm) State() consensus.State {
	sm.stateLock.RLock()
	defer sm.stateLock.RUnlock()

	return sm.state
}

// SetState sets dpor pbft state
func (sm *DporSm) SetState(state consensus.State) {
	sm.stateLock.Lock()
	defer sm.stateLock.Unlock()

	sm.state = state
}

// refreshWhenNewerHeight resets a signature state for a renewed block number(height)
func (sm *DporSm) refreshWhenNewerHeight(height uint64) {
	sm.lock.Lock()
	defer sm.lock.Unlock()

	if height > sm.lastHeight {
		sm.lastHeight = height
		sm.prepareSigState = make(map[common.Address]*blockSigItem)
		sm.commitSigState = make(map[common.Address]*blockSigItem)
	}
}

// verifyBlock is a func to verify whether the block is legal
func (sm *DporSm) verifyBlock(block *types.Block) bool {
	sm.lock.RLock()
	defer sm.lock.RUnlock()

	return sm.service.ValidateBlock(block) == nil
}

// commitCertificate is true if the validator has collected 2f+1 commit messages
func (sm *DporSm) commitCertificate(h *types.Header) bool {
	sm.lock.RLock()
	defer sm.lock.RUnlock()

	hash := h.Hash()
	var count uint64 = 0
	for _, item := range sm.commitSigState {
		if bytes.Equal(item.hash[:], hash[:]) {
			// TODO: @AC it had better to check whether the signature is valid for safety, consider add the check in future
			count++
		}
	}
	return count >= 2*sm.f+1
}

// composeValidateMsg is to return the validate message, which is the proposed block or impeach block
func (sm *DporSm) composeValidateMsg(h *types.Header) (*types.Block, error) {
	sm.lock.RLock()
	defer sm.lock.RUnlock()

	hash := h.Hash()
	b, got := sm.bcache.Get(hash)
	if !got {
		log.Warn("failed to retrieve block from cache", "hash", hash)
		return nil, errBlockNotExist
	}
	theBlock := b.(*types.Block)

	// make up the all signatures if missing
	validators := h.Dpor.Validators
	for i, v := range validators {
		if theBlock.Dpor().Sigs[i].IsEmpty() { // if the sig is empty, try make up it
			// try to find the sig in cache
			state := sm.commitSigState[v]
			if state.hash == hash { // if the validator signed the block, use its signature
				copy(theBlock.Dpor().Sigs[i][:], state.sig[:])
			}
		}
	}

	return theBlock, nil
}

// commitMsgPlus merge the signatures of commit messages
func (sm *DporSm) commitMsgPlus(h *types.Header) error {
	sm.lock.Lock()
	defer sm.lock.Unlock()

	sm.refreshWhenNewerHeight(h.Number.Uint64())

	// retrieve signers for checking
	signers, sigs, err := sm.service.EcrecoverSigs(h, consensus.Prepared)
	if err != nil {
		log.Warn("failed to recover signatures of committing phase", "error", err)
		return err
	}

	// check the signers are validators
	validators, _ := sm.service.ValidatorsOf(h.Number.Uint64())
	var checkErr error = nil
	for _, s := range signers {
		isValidator := false
		for _, v := range validators {
			if s == v {
				isValidator = true
			}
		}
		if !isValidator {
			log.Warn("a signer is not in validator committee", "signer", s.Hex())
			checkErr = consensus.ErrInvalidSigners
		}
	}
	if checkErr != nil {
		return checkErr
	}

	// merge signature to state
	hash := h.Hash()
	for i, s := range signers {
		sm.commitSigState[s] = &blockSigItem{
			hash: hash,
			sig:  sigs[i],
		}
	}
	return nil
}

func (sm *DporSm) composeCommitMsg(h *types.Header) (*types.Header, error) {
	if sm.lastHeight > h.Number.Uint64() {
		return nil, errBlockTooOld
	}

	sm.refreshWhenNewerHeight(h.Number.Uint64())

	// validator signs the block, update final sigs cache first
	for v, item := range sm.commitSigState {
		sm.service.UpdateFinalSigsCache(v, item.hash, item.sig)
	}
	sm.service.SignHeader(h, consensus.Prepared)
	log.Info("sign block by validator at commit msg", "blocknum", sm.lastHeight, "sigs", h.Dpor.SigsFormatText())

	return h, nil
}

//prepareCertificate is true if the validator has collects 2f+1 prepare messages
func (sm *DporSm) prepareCertificate(h *types.Header) bool {
	sm.lock.RLock()
	defer sm.lock.RUnlock()

	hash := h.Hash()
	var count uint64 = 0
	for _, item := range sm.prepareSigState {
		if bytes.Equal(item.hash[:], hash[:]) {
			// TODO: @AC it had better to check whether the signature is valid for safety, consider add the check in future
			count++
		}
	}
	return count >= 2*sm.f+1
}

// Add one to the counter of prepare messages
func (sm *DporSm) prepareMsgPlus(h *types.Header) error {
	sm.lock.Lock()
	defer sm.lock.Unlock()

	sm.refreshWhenNewerHeight(h.Number.Uint64())

	// retrieve signers for checking
	signers, sigs, err := sm.service.EcrecoverSigs(h, consensus.Prepared)
	if err != nil {
		log.Warn("failed to recover signatures of preparing phase", "error", err)
		return err
	}

	// check the signers are validators
	validators, _ := sm.service.ValidatorsOf(h.Number.Uint64())
	var checkErr error = nil
	for _, s := range signers {
		isValidator := false
		for _, v := range validators {
			if s == v {
				isValidator = true
			}
		}
		if !isValidator {
			log.Warn("a signer is not in validator committee", "signer", s.Hex())
			checkErr = consensus.ErrInvalidSigners
		}
	}
	if checkErr != nil {
		return checkErr
	}

	// merge signature to state
	hash := h.Hash()
	for i, s := range signers {
		sm.prepareSigState[s] = &blockSigItem{
			hash: hash,
			sig:  sigs[i],
		}
	}
	return nil
}

// It is used to compose prepare message given a newly proposed block
func (sm *DporSm) composePrepareMsg(b *types.Block) (*types.Header, error) {
	if sm.lastHeight >= b.NumberU64() {
		return nil, errBlockTooOld
	}

	sm.refreshWhenNewerHeight(b.NumberU64())
	sm.bcache.Add(b.Hash(), b) // add to cache
	// validator signs the block
	for v, item := range sm.prepareSigState {
		sm.service.UpdatePrepareSigsCache(v, item.hash, item.sig)
	}
	sm.service.SignHeader(b.RefHeader(), consensus.Preprepared)
	log.Info("sign block by validator at prepare msg", "blocknum", sm.lastHeight, "sigs", b.RefHeader().Dpor.SigsFormatText())

	return b.Header(), nil
}

//It is used to propose an impeach block
func (sm *DporSm) proposeImpeachBlock() *types.Block {
	b, e := sm.service.CreateImpeachBlock()
	if e != nil {
		log.Warn("creating impeachment block failed", "error", e)
		return nil
	}

	sm.service.SignHeader(b.RefHeader(), consensus.Preprepared)
	log.Info("proposed a impeachment block", "hash", b.Hash().Hex(), "sigs", b.Header().Dpor.SigsFormatText())
	return b
}

func (sm *DporSm) impeachCommitCertificate(h *types.Header) bool {
	return sm.commitCertificate(h)
}

func (sm *DporSm) composeImpeachValidateMsg(h *types.Header) (*types.Block, error) {
	return sm.composeValidateMsg(h)
}

func (sm *DporSm) impeachCommitMsgPlus(h *types.Header) error {
	return sm.commitMsgPlus(h)
}

func (sm *DporSm) impeachPrepareCertificate(h *types.Header) bool {
	return sm.prepareCertificate(h)
}

func (sm *DporSm) impeachPrepareMsgPlus(h *types.Header) error {
	return sm.prepareMsgPlus(h)
}

// Fsm is the finite state machine for a validator, to output the correct state given on current state and inputs
// input is either a header or a block, referring to message or proposed (impeach) block
// inputType indicates the type of input
// msg indicates what type of message or block input is
// state is the current state of the validator
// the output interface is the message or block validator should handle
// the output action refers to what the validator should do with the output interface
// the output dataType indicates whether the output interface is block or header
// the output msgCode represents the type the output block or message
// the output consensus.State indicates the validator's next state
func (sm *DporSm) Fsm(input interface{}, inputType dataType, msg msgCode) (interface{}, action, dataType, msgCode, error) {
	state := sm.State()
	output, act, dtype, msg, state, err := sm.fsm(input, inputType, msg, state)
	sm.SetState(state)

	return output, act, dtype, msg, err
}

func (sm *DporSm) fsm(input interface{}, inputType dataType, msg msgCode, state consensus.State) (interface{}, action, dataType, msgCode, consensus.State, error) {
	var inputHeader *types.Header
	var inputBlock *types.Block
	var err error

	// Determine the input is a header or a block by inputType
	switch inputType {
	case headerType:
		inputHeader = input.(*types.Header)
	case blockType:
		inputBlock = input.(*types.Block)
	case impeachBlockType:
		inputBlock = input.(*types.Block)
	default:
		err = errFsmWrongDataType
		return nil, noAction, noType, noMsgCode, consensus.Idle, err
	}

	switch state {
	// The case of consensus.Idle state
	case consensus.Idle:
		switch msg {
		// Stay in consensus.Idle state if receives validate message, and we should insert the block
		case validateMsgCode:
			return inputBlock, insertBlockAction, blockType, noMsgCode, consensus.Idle, nil

		// Stay in consensus.Idle state to committed state if receive 2f+1 commit messages
		case commitMsgCode:
			if sm.commitCertificate(inputHeader) {
				b, err := sm.composeValidateMsg(inputHeader)
				if err != nil {
					log.Warn("error when handling commitMsg on Idle state", "error", err)
					return nil, noAction, noType, noMsgCode, consensus.Idle, err
				}
				return b, broadcastAndInsertBlockAction, blockType, validateMsgCode, consensus.Idle, nil
			} else {
				// Add one to the counter of commit messages
				err := sm.commitMsgPlus(inputHeader)
				if err != nil {
					log.Warn("error when handling commitMsg on Idle state", "error", err)
					return nil, noAction, noType, noMsgCode, consensus.Idle, err
				}
				return input, noAction, noType, noMsgCode, consensus.Idle, nil
			}

		// Jump to consensus.Prepared state if receive 2f+1 prepare message
		case prepareMsgCode:
			if sm.prepareCertificate(inputHeader) {
				ret, err := sm.composeCommitMsg(inputHeader)
				if err != nil {
					return nil, noAction, noType, noMsgCode, consensus.Idle, err
				}
				return ret, broadcastMsgAction, headerType, commitMsgCode, consensus.Prepared, nil
			} else {
				// Add one to the counter of prepare messages
				err := sm.prepareMsgPlus(inputHeader)
				if err != nil {
					log.Warn("error when handling prepareMsg on Idle state", "error", err)
					return nil, noAction, noType, noMsgCode, consensus.Idle, err
				}
				return input, noAction, noType, noMsgCode, consensus.Idle, nil
			}

		// For the case that receive the newly proposes block or pre-prepare message
		case preprepareMsgCode:
			if sm.verifyBlock(inputBlock) {
				ret, err := sm.composePrepareMsg(inputBlock)
				if err != nil {
					log.Warn("error when handling preprepareMsg on Idle state", "error", err)
					return nil, noAction, noType, noMsgCode, consensus.Idle, err
				}
				return ret, broadcastMsgAction, headerType, prepareMsgCode, consensus.Preprepared, nil
			} else {
				err = errFsmFaultyBlock
				return sm.proposeImpeachBlock(), insertBlockAction, blockType, impeachPrepareMsgCode, consensus.ImpeachPreprepared, err
			}

		// Stay in consensus.Idle state and insert an impeachment block when receiving an impeach validate message
		case impeachValidateMsgCode:
			return inputBlock, insertBlockAction, blockType, noMsgCode, consensus.Idle, nil

		// Stay in consensus.Idle state if the validator collects 2f+1 impeach commit messages
		case impeachCommitMsgCode:
			if sm.impeachCommitCertificate(inputHeader) {
				b, err := sm.composeImpeachValidateMsg(inputHeader)
				if err != nil {
					log.Warn("error when handling impeachCommitMsg on Idle state", "error", err)
					return nil, noAction, noType, noMsgCode, consensus.Idle, err
				}
				return b, broadcastAndInsertBlockAction, blockType, impeachValidateMsgCode, consensus.Idle, nil
			} else {
				err := sm.impeachCommitMsgPlus(inputHeader)
				if err != nil {
					log.Warn("error when handling impeachCommitMsg on Idle state", "error", err)
					return nil, noAction, noType, noMsgCode, consensus.Idle, err
				}
				return inputHeader, noAction, noType, noMsgCode, consensus.Idle, nil
			}

		// Transit to impeach consensus.Prepared state if it collects 2f+1 impeach prepare messages
		case impeachPrepareMsgCode:
			if sm.impeachPrepareCertificate(inputHeader) {
				return inputHeader, broadcastMsgAction, headerType, impeachCommitMsgCode, consensus.ImpeachPrepared, nil
			} else {
				err := sm.impeachPrepareMsgPlus(inputHeader)
				if err != nil {
					log.Warn("error when handling impeachPrepareMsg on Idle state", "error", err)
					return nil, noAction, noType, noMsgCode, consensus.Idle, err
				}
				return input, noAction, noType, noMsgCode, consensus.Idle, nil
			}

		// Transit to impeach pre-prepared state if the timers expires (receiving a impeach pre-prepared message),
		// then generate the impeachment block and broadcast the impeach prepare massage
		case impeachPreprepareMsgCode:
			return sm.proposeImpeachBlock(), broadcastMsgAction, blockType, impeachPrepareMsgCode, consensus.ImpeachPreprepared, nil
		default:
			err = errFsmWrongIdleIpunt
		}

	// The case of pre-prepared state
	case consensus.Preprepared:
		switch msg {
		// Jump to committed state if receive a validate message
		case validateMsgCode:
			return inputBlock, insertBlockAction, blockType, noMsgCode, consensus.Idle, nil

		// Jump to committed state if receive 2f+1 commit messages
		case commitMsgCode:
			if sm.commitCertificate(inputHeader) {
				b, err := sm.composeValidateMsg(inputHeader)
				if err != nil {
					log.Warn("error when handling commitMsg on Preprepared state", "error", err)
					return nil, noAction, noType, noMsgCode, consensus.Idle, err
				}
				return b, broadcastAndInsertBlockAction, blockType, validateMsgCode, consensus.Idle, nil
			} else {
				// Add one to the counter of commit messages
				err := sm.commitMsgPlus(inputHeader)
				if err != nil {
					log.Warn("error when handling commitMsg on Preprepared state", "error", err)
					return nil, noAction, noType, noMsgCode, consensus.Idle, err
				}
				return input, noAction, noType, noMsgCode, consensus.Preprepared, nil
			}
		// Convert to consensus.Prepared state if collect prepare certificate
		case prepareMsgCode:
			if sm.prepareCertificate(inputHeader) {
				ret, err := sm.composeCommitMsg(inputHeader)
				if err != nil {
					return nil, noAction, noType, noMsgCode, consensus.Preprepared, err
				}
				return ret, broadcastMsgAction, headerType, commitMsgCode, consensus.Prepared, nil
			} else {
				// Add one to the counter of prepare messages
				err := sm.prepareMsgPlus(inputHeader)
				if err != nil {
					log.Warn("error when handling prepareMsg on Preprepared state", "error", err)
					return nil, noAction, noType, noMsgCode, consensus.Preprepared, err
				}
				return input, noAction, noType, noMsgCode, consensus.Preprepared, nil
			}
		case impeachValidateMsgCode:
			return inputBlock, insertBlockAction, blockType, noMsgCode, consensus.Idle, nil

		// Stay in consensus.Idle state to committed state if receive 2f+1 commit messages
		case impeachCommitMsgCode:
			if sm.impeachCommitCertificate(inputHeader) {
				b, err := sm.composeImpeachValidateMsg(inputHeader)
				if err != nil {
					log.Warn("error when handling impeachCommitMsg on Preprepared state", "error", err)
					return nil, noAction, noType, noMsgCode, consensus.Preprepared, err
				}
				return b, broadcastAndInsertBlockAction, blockType, impeachValidateMsgCode, consensus.Idle, nil
			} else {
				// Add one to the counter of commit messages
				err := sm.commitMsgPlus(inputHeader)
				if err != nil {
					log.Warn("error when handling impeachCommitMsg on Preprepared state", "error", err)
					return nil, noAction, noType, noMsgCode, consensus.Preprepared, err
				}
				return input, noAction, noType, noMsgCode, consensus.Idle, nil
			}

		// Transit to impeach consensus.Prepared state if it collects 2f+1 impeach prepare messages
		case impeachPrepareMsgCode:
			if sm.impeachPrepareCertificate(inputHeader) {
				return inputHeader, broadcastMsgAction, headerType, impeachCommitMsgCode, consensus.ImpeachPrepared, nil
			} else {
				err := sm.impeachPrepareMsgPlus(inputHeader)
				if err != nil {
					log.Warn("error when handling impeachPrepareMsg on Preprepared state", "error", err)
					return nil, noAction, noType, noMsgCode, consensus.Preprepared, err
				}
				return input, noAction, noType, noMsgCode, consensus.Preprepared, nil
			}

		case impeachPreprepareMsgCode:
			return sm.proposeImpeachBlock(), broadcastMsgAction, blockType, impeachPrepareMsgCode, consensus.ImpeachPreprepared, nil
		default:
			err = errFsmWrongPrepreparedInput
		}

	// The case of consensus.Prepared stage
	case consensus.Prepared:
		switch msg {
		// Jump to committed state if receive a validate message
		case validateMsgCode:
			return inputBlock, insertBlockAction, blockType, noMsgCode, consensus.Idle, nil

		// convert to committed state if collects commit certificate
		case commitMsgCode:
			if sm.commitCertificate(inputHeader) {
				b, err := sm.composeValidateMsg(inputHeader)
				if err != nil {
					log.Warn("error when handling commitMsg on Prepared state", "error", err)
					return nil, noAction, noType, noMsgCode, consensus.Prepared, err
				}
				return b, broadcastAndInsertBlockAction, blockType, validateMsgCode, consensus.Idle, nil
			} else {
				// Add one to the counter of commit messages
				err := sm.commitMsgPlus(inputHeader)
				if err != nil {
					log.Warn("error when handling commitMsg on Prepared state", "error", err)
					return nil, noAction, noType, noMsgCode, consensus.Prepared, err
				}
				return input, noAction, noType, noMsgCode, consensus.Preprepared, nil
			}

		// Transit to consensus.Idle state to insert impeach block
		case impeachValidateMsgCode:
			return inputBlock, insertBlockAction, blockType, noMsgCode, consensus.Idle, nil

		// Transit to consensus.Idle state to committed state if receive 2f+1 commit messages
		case impeachCommitMsgCode:
			if sm.impeachCommitCertificate(inputHeader) {
				b, err := sm.composeImpeachValidateMsg(inputHeader)
				if err != nil {
					log.Warn("error when handling impeachCommitMsg on Prepared state", "error", err)
					return nil, noAction, noType, noMsgCode, consensus.Prepared, err
				}
				return b, broadcastAndInsertBlockAction, blockType, impeachValidateMsgCode, consensus.Idle, nil
			} else {
				// Add one to the counter of commit messages
				err := sm.impeachCommitMsgPlus(inputHeader)
				if err != nil {
					log.Warn("error when handling impeachCommitMsg on Prepared state", "error", err)
					return nil, noAction, noType, noMsgCode, consensus.Prepared, err
				}
				return input, noAction, noType, noMsgCode, consensus.Prepared, nil
			}

		// Transit to impeach consensus.Prepared state if it collects 2f+1 impeach prepare messages
		case impeachPrepareMsgCode:
			if sm.impeachPrepareCertificate(inputHeader) {
				return inputHeader, broadcastMsgAction, headerType, impeachCommitMsgCode, consensus.ImpeachPrepared, nil
			} else {
				err := sm.impeachPrepareMsgPlus(inputHeader)
				if err != nil {
					log.Warn("error when handling impeachPrepareMsg on Prepared state", "error", err)
					return nil, noAction, noType, noMsgCode, consensus.Prepared, err
				}
				return input, noAction, noType, noMsgCode, consensus.Prepared, nil
			}

		case impeachPreprepareMsgCode:
			return sm.proposeImpeachBlock(), broadcastMsgAction, blockType, impeachPrepareMsgCode, consensus.ImpeachPreprepared, nil
		default:
			err = errFsmWrongPreparedInput

		}

	case consensus.ImpeachPreprepared:
		switch msg {
		// Transit to consensus.Idle state when receiving impeach validate message
		case impeachValidateMsgCode:
			return inputBlock, insertBlockAction, blockType, noMsgCode, consensus.Idle, nil

		// Stay in consensus.Idle state to committed state if receive 2f+1 commit messages
		case impeachCommitMsgCode:
			if sm.impeachCommitCertificate(inputHeader) {
				b, err := sm.composeImpeachValidateMsg(inputHeader)
				if err != nil {
					log.Warn("error when handling impeachCommitMsg on ImpeachPreprepared state", "error", err)
					return nil, noAction, noType, noMsgCode, consensus.ImpeachPreprepared, err
				}
				return b, broadcastAndInsertBlockAction, blockType, impeachValidateMsgCode, consensus.Idle, nil
			} else {
				// Add one to the counter of commit messages
				err := sm.impeachCommitMsgPlus(inputHeader)
				if err != nil {
					log.Warn("error when handling impeachCommitMsg on ImpeachPreprepared state", "error", err)
					return nil, noAction, noType, noMsgCode, consensus.ImpeachPreprepared, err
				}
				return input, noAction, noType, noMsgCode, consensus.ImpeachPreprepared, nil
			}

		// Transit to impeach consensus.Prepared state if it collects 2f+1 impeach prepare messages
		case impeachPrepareMsgCode:
			if sm.impeachPrepareCertificate(inputHeader) {
				return inputHeader, broadcastMsgAction, headerType, impeachCommitMsgCode, consensus.ImpeachPrepared, nil
			} else {
				err := sm.impeachPrepareMsgPlus(inputHeader)
				if err != nil {
					log.Warn("error when handling impeachPrepareMsg on ImpeachPreprepared state", "error", err)
					return nil, noAction, noType, noMsgCode, consensus.ImpeachPreprepared, err
				}
				return input, noAction, noType, noMsgCode, consensus.ImpeachPreprepared, nil
			}
		default:
			err = errFsmWrongImpeachPrepreparedInput
		}

	case consensus.ImpeachPrepared:
		switch msg {
		// Transit to consensus.Idle state when receiving impeach validate message
		case impeachValidateMsgCode:
			return inputBlock, insertBlockAction, blockType, noMsgCode, consensus.Idle, nil

		// Stay in consensus.Idle state to committed state if receive 2f+1 commit messages
		case impeachCommitMsgCode:
			if sm.impeachCommitCertificate(inputHeader) {
				b, err := sm.composeImpeachValidateMsg(inputHeader)
				if err != nil {
					log.Warn("error when handling impeachCommitMsg on ImpeachPrepared state", "error", err)
					return nil, noAction, noType, noMsgCode, consensus.ImpeachPrepared, err
				}
				return b, broadcastAndInsertBlockAction, blockType, impeachValidateMsgCode, consensus.Idle, nil
			} else {
				// Add one to the counter of commit messages
				err := sm.impeachCommitMsgPlus(inputHeader)
				if err != nil {
					log.Warn("error when handling impeachCommitMsg on ImpeachPrepared state", "error", err)
					return nil, noAction, noType, noMsgCode, consensus.ImpeachPrepared, err
				}
				return input, noAction, noType, noMsgCode, consensus.ImpeachPrepared, nil
			}
		default:
			err = errFsmWrongPreparedInput
		}

		// Broadcast a validate message and then go back to consensus.Idle state
		//case committed:
		///return sm.composeValidateMsg(inputHeader), broadcastAndInsertBlock, block, validateMsg, consensus.Idle, nil
		// Broadcast a validate message and then go back to consensus.Idle state
		//case committed:
		//	return composeValidateMsg(inputHeader), broadcastAndInsertBlock, block, validateMsg, consensus.Idle, nil

		// Insert the block and go back to consensus.Idle state
		//case inserting:
		//	return inputBlock, insertBlock, block, noMsg, consensus.Idle, nil
	}

	return nil, noAction, noType, noMsgCode, state, err
}
