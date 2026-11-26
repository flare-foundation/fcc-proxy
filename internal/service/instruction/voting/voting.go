package voting

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/flare-foundation/fcc-node/pkg/types"
	"github.com/flare-foundation/fcc-proxy/internal/metrics"
	"github.com/flare-foundation/fcc-proxy/internal/service/instruction/voting/limiter"
	"github.com/flare-foundation/fcc-proxy/pkg/config"
	"github.com/flare-foundation/fcc-proxy/pkg/instruction/meta"
	"github.com/flare-foundation/fcc-proxy/pkg/instruction/voting"
	"github.com/flare-foundation/fcc-proxy/pkg/status"
	"github.com/flare-foundation/go-flare-common/pkg/logger"
	"github.com/flare-foundation/go-flare-common/pkg/policy"
	"github.com/flare-foundation/go-flare-common/pkg/storage"
	"github.com/flare-foundation/go-flare-common/pkg/voters"

	"github.com/ethereum/go-ethereum/common"
	"github.com/flare-foundation/go-flare-common/pkg/tee/instruction"
)

const maxBIPS = 10000

// restoreThresholdDelay lets a finalized restore collect more key shares before its threshold action.
const restoreThresholdDelay = 10 * time.Minute

// AddVote rejection sentinels classified by RejectReason; each wraps its HTTP status.
var (
	errNoRound          = fmt.Errorf("%w: no round", status.HTTP[404])
	errInconsistentData = fmt.Errorf("%w: inconsistent data", status.HTTP[400])
)

type voterGroup uint8

const (
	invalidVoter voterGroup = 0
	provider     voterGroup = 0b01
	cosigner     voterGroup = 0b10
	both         voterGroup = 0b11
)

// isCosigner member from a voter group is a cosigner.
func (vg voterGroup) isCosigner() bool {
	return vg&cosigner != 0
}

// Declined is a finalized decline voting: its voters leave the instruction unable to reach its threshold.
type Declined struct {
	InstructionID common.Hash
	OPType        common.Hash
	OPCommand     common.Hash
	Voters        []common.Address // in vote order
}

// Storage holds one voting Round per signing policy in a cyclic buffer; storing a new round at
// ID%size overwrites the previous occupant.
type Storage struct {
	*storage.Cyclic[uint32, *Round]

	config config.Voting

	// Metadata for the voting process.
	meta meta.Meta

	// Channel for actions created by voting.
	Out chan *types.Action
	// Channel for finalized decline votings, which never reach the node.
	Declined chan *Declined

	// Service-lifetime ctx; per-box goroutines outlive the request that spawns them.
	ctx context.Context //nolint:containedctx // service-lifetime ctx, see comment

	thresholdDelay time.Duration // restoreThresholdDelay; tests shorten it

	metrics           *metrics.Metrics
	collectVoters     bool
	currentEpoch      atomic.Uint32 // latest stored reward epoch, for the active-voter gauge
	maxConsensusEpoch atomic.Uint32 // highest reward epoch of any voting that reached consensus
}

// NewStorage creates a voting Storage backed by a cyclic buffer sized by config.HistorySize.
// ctx is the service-lifetime context that bounds per-box goroutines created during AddVote.
// m may be nil or disabled.
func NewStorage(ctx context.Context, config *config.Voting, meta meta.Meta, m *metrics.Metrics) *Storage {
	out := make(chan *types.Action, config.FinalizedBufferSize)

	return &Storage{
		Cyclic:         storage.New[uint32, *Round](config.HistorySize),
		config:         *config,
		meta:           meta,
		Out:            out,
		Declined:       make(chan *Declined, config.FinalizedBufferSize),
		ctx:            ctx,
		thresholdDelay: restoreThresholdDelay,
		metrics:        m,
		collectVoters:  m.ActiveVotersEnabled(),
	}
}

// StoreNewRound creates and stores a round for the policy, overwriting the previous occupant
// at the same cyclic slot. A no-op if a round for this policy already exists.
func (s *Storage) StoreNewRound(policy *policy.SigningPolicy) {
	_, exists := s.Get(policy.RewardEpochID)
	if exists {
		return
	}

	r := createRound(policy, s.config.MaxPendingRequests, s.collectVoters)

	s.Store(policy.RewardEpochID, r)

	if policy.RewardEpochID > s.currentEpoch.Load() {
		s.currentEpoch.Store(policy.RewardEpochID)
	}
}

// recordConsensusEpoch raises the max-consensus-epoch watermark to epoch if it is larger.
func (s *Storage) recordConsensusEpoch(epoch uint32) {
	for {
		old := s.maxConsensusEpoch.Load()
		if epoch <= old {
			return
		}
		if s.maxConsensusEpoch.CompareAndSwap(old, epoch) {
			return
		}
	}
}

// MaxConsensusEpoch returns the highest reward epoch reached by consensus, or 0 if none yet.
func (s *Storage) MaxConsensusEpoch() uint32 {
	return s.maxConsensusEpoch.Load()
}

// activeParticipantEpochs returns the newest resident reward epoch and its predecessor,
// the rounds an unfinalized proposal can be in during the policy-adoption overlap.
func (s *Storage) activeParticipantEpochs() []uint32 {
	cur := s.currentEpoch.Load()
	if cur == 0 {
		return []uint32{0}
	}
	return []uint32{cur, cur - 1}
}

// participantRound returns the round all scalar active-voter gauges report: the newest resident one,
// or its predecessor while the newest has no accepted provider votes; nil when neither is resident.
func (s *Storage) participantRound() *Round {
	cur := s.currentEpoch.Load()
	r, ok := s.Get(cur)
	if ok && r.ProviderVoterCount() > 0 {
		return r
	}
	if cur > 0 {
		if prev, okPrev := s.Get(cur - 1); okPrev {
			return prev
		}
	}
	return r
}

// CurrentRoundProviderVoterCount returns the distinct data-provider-voter count of participantRound, or 0.
func (s *Storage) CurrentRoundProviderVoterCount() int {
	r := s.participantRound()
	if r == nil {
		return 0
	}
	return r.ProviderVoterCount()
}

// CurrentRoundInitiatorCount returns the distinct initiator count of participantRound, or 0.
func (s *Storage) CurrentRoundInitiatorCount() int {
	r := s.participantRound()
	if r == nil {
		return 0
	}
	return r.ProposerCount()
}

// CurrentVotedWeightBips returns the combined weight of participantRound's data-provider voters
// in BIPS of the policy total, or 0.
func (s *Storage) CurrentVotedWeightBips() float64 {
	r := s.participantRound()
	if r == nil {
		return 0
	}
	return r.VotedWeightBips()
}

// CurrentMaxVotingWeightBips returns the highest provider weight a single voting in participantRound
// accumulated, in BIPS of the policy total, or 0.
func (s *Storage) CurrentMaxVotingWeightBips() float64 {
	r := s.participantRound()
	if r == nil {
		return 0
	}
	return r.MaxVotingWeightBips()
}

// CurrentVotingThresholdBips returns participantRound's finalization threshold in BIPS of the policy total, or 0.
func (s *Storage) CurrentVotingThresholdBips() float64 {
	r := s.participantRound()
	if r == nil {
		return 0
	}
	return r.ThresholdBips()
}

// CurrentRoundTopPending returns up to n voters with the most unfinalized proposals across
// activeParticipantEpochs, taking each provider's max, not sum; nil if no round is stored.
func (s *Storage) CurrentRoundTopPending(n int) []limiter.VoterPending {
	merged := map[common.Address]uint{}
	for _, e := range s.activeParticipantEpochs() {
		r, ok := s.Get(e)
		if !ok {
			continue
		}
		for _, vp := range r.TopPendingProposals(-1) { // -1 returns all pending voters
			merged[vp.Address] = max(merged[vp.Address], vp.Pending)
		}
	}
	if len(merged) == 0 {
		return nil
	}
	out := make([]limiter.VoterPending, 0, len(merged))
	for addr, p := range merged {
		out = append(out, limiter.VoterPending{Address: addr, Pending: p})
	}
	return limiter.SortTopPending(out, n)
}

// OldestStoredEpoch returns the oldest reward epoch with a resident round; ok is false when none is stored.
func (s *Storage) OldestStoredEpoch() (uint32, bool) {
	top := s.currentEpoch.Load()
	var oldest uint32
	var ok bool
	for i := range uint32(s.Size()) {
		if i > top {
			break
		}
		e := top - i
		if _, present := s.Get(e); present {
			oldest = e
			ok = true
		}
		if e == 0 {
			break
		}
	}
	return oldest, ok
}

// AddVote records a vote and returns a signed receipt; an unseen instruction hash opens a new box
// if the proposer is under its limiter cap.
func (s *Storage) AddVote(data *instruction.Data, signer common.Address, signature []byte) (*voting.Receipt, error) {
	id := data.InstructionID

	err := checkSize(data, s.config.MaxProviderVote)
	if err != nil {
		return nil, err
	}

	hash, err := data.HashFixed()
	if err != nil {
		return nil, err
	}

	round, exists := s.Get(data.RewardEpochID)
	if !exists {
		return nil, fmt.Errorf("%w %d", errNoRound, data.RewardEpochID)
	}

	err = s.meta.CheckConsistency(data, signer)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errInconsistentData, err)
	}

	var receipt voting.Receipt
	var actionToSend *types.Action
	var declined *Declined
	var reached bool
	var startedBox bool
	var isProvider bool
	var votedWeight uint16
	var buildErr bool
	var thresholdDuration time.Duration

	// Hold locks only inside this closure; the channel send below stays unlocked.
	err = func() error {
		round.Voting.Lock()
		boxes, existsBs := round.Voting.M[id]
		if !existsBs {
			boxes = newVoteBoxes()
			defer round.Voting.Unlock() // we only save it at the end if no errors are returned
		} else {
			round.Voting.Unlock()
		}

		boxes.Lock()
		defer boxes.Unlock()

		box, existsB := boxes.M[hash]
		if !existsB {
			expiration := s.config.ProposalExpiration
			if isRestore(&data.DataFixed) {
				expiration = s.config.RestoreExpiration
			}

			var err error
			box, err = startVoteBox(data, signer, round, s.meta, expiration)
			if err != nil {
				return err
			}
		}

		box.Lock()
		defer box.Unlock()

		if box.deleted {
			return fmt.Errorf("%w: %s", errVotingEnded, id)
		}

		// read cosigners under box.Lock: scheduleEnd's delete clears them
		vg, weight := voterGroupCheck(signer, round.policy.Voters.VoterDataMap, box.proposal.cosigners)
		isProvider = vg&provider != 0

		r, finalized, err := box.addVote(signer, weight, signature, data.AdditionalVariableMessage, vg)
		if err != nil {
			return fmt.Errorf("adding vote from %s to %v: %w", signer, id, err)
		}
		receipt = r
		votedWeight = box.weight // read under box.Lock

		if !existsB {
			boxes.M[hash] = box
			go box.schedule(s.ctx, s.Out, boxes, s.thresholdDelay, s.metrics)
			startedBox = true
		}

		if !existsBs {
			round.Voting.M[id] = boxes
		}

		if finalized {
			reached = true
			thresholdDuration = time.Since(box.StartTime)
			round.limiter.Decrement(box.Proposer)

			// The node accepts one threshold per instruction ID, so only the first finalized hash is sent;
			// a finalized decline yields to an instruction finalized after it.
			supersedes := boxes.declined && !box.proposal.declined
			if boxes.FinalizedHash != (common.Hash{}) && boxes.FinalizedHash != hash && !supersedes {
				logger.Warnf("instruction id %v already finalized with %v, not sending threshold action for %v", box.iID, boxes.FinalizedHash, hash)
				return nil
			}
			if supersedes {
				logger.Warnf("instruction id %v finalized with %v after its decline %v, sending threshold action", box.iID, hash, boxes.FinalizedHash)
			}
			boxes.FinalizedHash = hash
			boxes.declined = box.proposal.declined

			switch {
			case box.proposal.declined:
				declined = &Declined{InstructionID: id, OPType: data.OPType, OPCommand: data.OPCommand, Voters: box.voters()}
			case box.finalizedAt != nil:
				// never blocks: buffered, sent once
				box.finalizedAt <- time.Now()
			default:
				a, err := box.Action(types.Threshold)
				if err != nil {
					logger.Errorf("failed creating threshold action for %v, %v: %v", id, hash, err)
					buildErr = true
				} else {
					actionToSend = a
				}
			}
		}

		return nil
	}()

	if err != nil {
		return nil, err
	}

	// Record metrics outside the locked critical section above.
	if startedBox {
		s.metrics.VotingStarted()
	}
	if s.collectVoters {
		if isProvider {
			round.markProviderVoter(signer)
			// outside box.Lock to keep a box.Lock -> votersMu edge out of the lock graph
			round.recordVotingWeight(votedWeight)
		}
		if startedBox {
			round.markProposer(signer)
		}
	}
	if reached {
		s.metrics.VotingThresholdReached(thresholdDuration)
		s.recordConsensusEpoch(data.RewardEpochID)
	}
	if buildErr {
		s.metrics.FinalizedActionLost("build_error")
	}

	if actionToSend != nil {
		select {
		case s.Out <- actionToSend:
		case <-s.ctx.Done():
			s.metrics.FinalizedActionLost("send_cancelled")
			return nil, fmt.Errorf("forwarding finalized action: %w", s.ctx.Err())
		}
	}

	if declined != nil {
		select {
		case s.Declined <- declined:
		case <-s.ctx.Done():
			s.metrics.FinalizedActionLost("send_cancelled")
			return nil, fmt.Errorf("forwarding declined voting: %w", s.ctx.Err())
		}
	}

	return &receipt, nil
}

func voterGroupCheck(signer common.Address, voterDataMap map[common.Address]voters.VoterData, cosigners map[common.Address]bool) (voterGroup, uint16) {
	var vg voterGroup = 0
	weight := uint16(0)

	vd, exists := voterDataMap[signer]
	if exists {
		vg |= provider
		weight = vd.Weight
	}

	if cosigners[signer] {
		vg |= cosigner
	}

	return vg, weight
}
