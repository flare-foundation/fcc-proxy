package voting

import (
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/flare-foundation/fcc-node/pkg/fdc"
	"github.com/flare-foundation/fcc-node/pkg/types"
	"github.com/flare-foundation/go-flare-common/pkg/tee/instruction"
	"github.com/flare-foundation/go-flare-common/pkg/tee/op"
	"github.com/flare-foundation/go-flare-common/pkg/tee/structs/fdc2"
	"github.com/stretchr/testify/require"

	"github.com/flare-foundation/fcc-proxy/internal/testutil"
	"github.com/flare-foundation/fcc-proxy/pkg/config"
	"github.com/flare-foundation/fcc-proxy/pkg/instruction/meta"
)

var (
	key1 = crypto.PubkeyToAddress(testutil.PrivKey1.PublicKey) // weight 1 of 7
	key2 = crypto.PubkeyToAddress(testutil.PrivKey2.PublicKey) // weight 3 of 7
	key3 = crypto.PubkeyToAddress(testutil.PrivKey3.PublicKey) // weight 3 of 7
)

var decline = hexutil.Bytes(meta.Decline)

// keyVote is a KEY_GENERATE vote; decline as its response makes it a decline.
func keyVote(id string, response []byte) *instruction.Data {
	return &instruction.Data{
		InstructionID:             crypto.Keccak256Hash([]byte(id)),
		TeeID:                     common.HexToAddress("dead"),
		Timestamp:                 uint64(time.Now().Unix()),
		RewardEpochID:             1,
		OPType:                    op.Wallet.Hash(),
		OPCommand:                 op.KeyGenerate.Hash(),
		OriginalMessage:           []byte("request"),
		AdditionalFixedMessage:    response,
		AdditionalVariableMessage: hexutil.Bytes{},
	}
}

// fdcVote is an FDC2 PROVE vote for a request with thresholdBIPS.
func fdcVote(t *testing.T, id string, thresholdBIPS uint16, response []byte) *instruction.Data {
	t.Helper()

	req, err := fdc.EncodeRequest(fdc2.IFdc2HubFdc2AttestationRequest{
		Header: fdc2.IFdc2HubFdc2RequestHeader{
			AttestationType: [32]byte{1},
			SourceId:        [32]byte{2},
			ThresholdBIPS:   thresholdBIPS,
		},
		RequestBody: []byte("body"),
	})
	require.NoError(t, err)

	d := keyVote(id, response)
	d.OPType = op.FDC2.Hash()
	d.OPCommand = op.Prove.Hash()
	d.OriginalMessage = req

	return d
}

// newDeclineStorage stores a round for testutil.TestSigningPolicy: total weight 7, threshold 3.
func newDeclineStorage(t *testing.T, m meta.Meta, expiration time.Duration) *Storage {
	t.Helper()

	s := NewStorage(t.Context(), &config.Voting{
		ProposalExpiration:  expiration,
		MaxPendingRequests:  10,
		HistorySize:         3,
		FinalizedBufferSize: 3,
	}, m, nil)
	s.StoreNewRound(testutil.TestSigningPolicy)

	return s
}

func addVotes(t *testing.T, s *Storage, data *instruction.Data, voters ...common.Address) {
	t.Helper()

	for _, v := range voters {
		_, err := s.AddVote(data, v, v.Bytes())
		require.NoError(t, err)
	}
}

func TestDeclineThreshold(t *testing.T) {
	keyGen := keyVote("threshold", decline)
	prove := &instruction.DataFixed{OPType: op.FDC2.Hash(), OPCommand: op.Prove.Hash()}

	tests := []struct {
		name         string
		total, t     uint16
		data         *instruction.DataFixed
		wantDeclined uint16
	}{
		{name: "policy", total: 7, t: 3, data: &keyGen.DataFixed, wantDeclined: 3},
		{name: "FDC2 minimum below policy", total: 100, t: 55, data: prove, wantDeclined: 59},
		{name: "FDC2 policy below minimum", total: 100, t: 30, data: prove, wantDeclined: 69},
		{name: "threshold at total", total: 100, t: 100, data: &keyGen.DataFixed, wantDeclined: 0},
		{name: "zero threshold", total: 100, t: 0, data: &keyGen.DataFixed, wantDeclined: 99},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.wantDeclined, declineThreshold(tc.total, tc.t, tc.data))
		})
	}
}

func TestBuildVoteBoxDecline(t *testing.T) {
	weights := []uint16{50, 30, 20}

	t.Run("ignores the vote's threshold", func(t *testing.T) {
		round := createRound(policyWithThreshold(t, weights, 55), 10, false)

		box, err := buildVoteBox(fdcVote(t, "bips", 9999, decline), key1, round, &bipsMeta{9999}, time.Minute)
		require.NoError(t, err)
		require.True(t, box.proposal.declined)
		require.Equal(t, uint16(59), box.proposal.threshold)
	})

	t.Run("cosigners vote but never count", func(t *testing.T) {
		round := createRound(policyWithThreshold(t, weights, 55), 10, false)
		m := &fixedThresholdMeta{
			cosigners:          map[common.Address]bool{common.HexToAddress("c1"): true, common.HexToAddress("c2"): true},
			cosignersThreshold: 2,
		}

		box, err := buildVoteBox(keyVote("cosigners", decline), key1, round, m, time.Minute)
		require.NoError(t, err)
		require.Len(t, box.proposal.cosigners, 2)
		require.Zero(t, box.proposal.cosignerThreshold)
	})

	t.Run("a response keeps the thresholds", func(t *testing.T) {
		round := createRound(policyWithThreshold(t, weights, 55), 10, false)

		box, err := buildVoteBox(fdcVote(t, "response", 0, []byte("response")), key1, round, &bipsMeta{-1}, time.Minute)
		require.NoError(t, err)
		require.False(t, box.proposal.declined)
		require.Equal(t, uint16(55), box.proposal.threshold)
	})
}

func TestCheckSizeDecline(t *testing.T) {
	require.NoError(t, checkSize(keyVote("size", decline), 0))
	require.ErrorIs(t, checkSize(keyVote("size", []byte("other")), 0), errAdditionalMessageTooBig)
}

func TestDeclinedVotingFinalizes(t *testing.T) {
	s := newDeclineStorage(t, &testMeta{}, time.Minute)
	data := keyVote("finalizes", decline)

	// 3 of 7 leaves 4 > 3 for the instruction
	addVotes(t, s, data, key2)
	require.Empty(t, s.Declined)

	// 4 of 7 leaves 3, which is not above the threshold
	addVotes(t, s, data, key1)
	require.Len(t, s.Declined, 1)
	require.Empty(t, s.Out, "nothing goes to the node")

	d := <-s.Declined
	require.Equal(t, &Declined{
		InstructionID: data.InstructionID,
		OPType:        op.Wallet.Hash(),
		OPCommand:     op.KeyGenerate.Hash(),
		Voters:        []common.Address{key2, key1},
	}, d)

	r, ok := s.Get(1)
	require.True(t, ok)
	hash, err := data.HashFixed()
	require.NoError(t, err)
	r.Voting.RLock()
	boxes := r.Voting.M[data.InstructionID]
	r.Voting.RUnlock()
	require.Equal(t, hash, boxes.FinalizedHash)
	require.True(t, boxes.M[hash].Status().Declined)
}

func TestDeclinedVotingCosignersDoNotCount(t *testing.T) {
	c1, c2 := common.HexToAddress("c1"), common.HexToAddress("c2")
	s := newDeclineStorage(t, &fixedThresholdMeta{cosigners: map[common.Address]bool{c1: true, c2: true}, cosignersThreshold: 2}, time.Minute)
	data := keyVote("cosigners", decline)

	addVotes(t, s, data, key1, c1, c2)
	require.Empty(t, s.Declined)

	addVotes(t, s, data, key2)
	require.Len(t, s.Declined, 1)
}

// TestDeclineForgery guards that one provider cannot finalize a decline whose own fields lower its threshold.
func TestDeclineForgery(t *testing.T) {
	t.Run("declared cosigners", func(t *testing.T) {
		s := newDeclineStorage(t, meta.New(nil, 14), time.Minute)
		data := fdcVote(t, "forged-cosigners", 0, decline)
		data.Cosigners = []common.Address{key1}
		data.CosignersThreshold = 1

		addVotes(t, s, data, key1)
		require.Empty(t, s.Declined)
	})

	t.Run("declared threshold", func(t *testing.T) {
		s := newDeclineStorage(t, meta.New(nil, 14), time.Minute)
		data := fdcVote(t, "forged-bips", 9999, decline)

		// FDC2 declines need 7 - min(3, 2) = 5 of 7
		addVotes(t, s, data, key1, key2)
		require.Empty(t, s.Declined)

		addVotes(t, s, data, key3)
		require.Len(t, s.Declined, 1)
	})
}

func TestDeclineYieldsToInstruction(t *testing.T) {
	t.Run("instruction first", func(t *testing.T) {
		s := newDeclineStorage(t, &testMeta{}, time.Minute)

		addVotes(t, s, keyVote("instruction-first", nil), key2, key3)
		require.Len(t, s.Out, 1)

		addVotes(t, s, keyVote("instruction-first", decline), key2, key1)
		require.Empty(t, s.Declined)
	})

	t.Run("decline first", func(t *testing.T) {
		s := newDeclineStorage(t, &testMeta{}, 300*time.Millisecond)
		data := keyVote("decline-first", nil)

		addVotes(t, s, keyVote("decline-first", decline), key2, key1)
		require.Len(t, s.Declined, 1)

		addVotes(t, s, data, key2, key3)
		require.Len(t, s.Out, 1)
		require.Equal(t, types.Threshold, (<-s.Out).Data.SubmissionTag)

		r, ok := s.Get(1)
		require.True(t, ok)
		hash, err := data.HashFixed()
		require.NoError(t, err)
		r.Voting.RLock()
		boxes := r.Voting.M[data.InstructionID]
		r.Voting.RUnlock()
		boxes.RLock()
		require.Equal(t, hash, boxes.FinalizedHash)
		boxes.RUnlock()

		// one threshold per ID still holds after a decline yields
		other := keyVote("decline-first", nil)
		other.OriginalMessage = []byte("other request")
		addVotes(t, s, other, key3, key1)
		require.Empty(t, s.Out)

		// the instruction's end action goes out, the decline's does not
		select {
		case a := <-s.Out:
			require.Equal(t, types.End, a.Data.SubmissionTag)
			require.Equal(t, data.InstructionID, a.Data.ID)
		case <-time.After(2 * time.Second):
			require.FailNow(t, "no end action")
		}
		require.Never(t, func() bool { return len(s.Out) > 0 }, 100*time.Millisecond, 10*time.Millisecond)
	})
}

func TestDeclinedVotingSendsNoEndAction(t *testing.T) {
	s := newDeclineStorage(t, &testMeta{}, 200*time.Millisecond)
	data := keyVote("no-end", decline)

	addVotes(t, s, data, key2, key1)
	require.Len(t, s.Declined, 1)

	r, ok := s.Get(1)
	require.True(t, ok)
	hash, err := data.HashFixed()
	require.NoError(t, err)
	r.Voting.RLock()
	boxes := r.Voting.M[data.InstructionID]
	r.Voting.RUnlock()
	boxes.RLock()
	box := boxes.M[hash]
	boxes.RUnlock()

	require.Eventually(t, func() bool {
		box.RLock()
		defer box.RUnlock()
		return box.deleted
	}, 2*time.Second, 10*time.Millisecond)
	require.Never(t, func() bool { return len(s.Out) > 0 }, 100*time.Millisecond, 10*time.Millisecond)
}
