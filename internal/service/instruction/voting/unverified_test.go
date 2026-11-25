package voting

import (
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/flare-foundation/go-flare-common/pkg/tee/instruction"
	"github.com/flare-foundation/go-flare-common/pkg/tee/op"
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

// fdcVote is an FDC2 PROVE vote; an empty response makes it unverified.
func fdcVote(id string, response []byte) *instruction.Data {
	return &instruction.Data{
		InstructionID:             crypto.Keccak256Hash([]byte(id)),
		TeeID:                     common.HexToAddress("dead"),
		Timestamp:                 uint64(time.Now().Unix()),
		RewardEpochID:             1,
		OPType:                    op.FDC2.Hash(),
		OPCommand:                 op.Prove.Hash(),
		OriginalMessage:           []byte("request"),
		AdditionalFixedMessage:    response,
		AdditionalVariableMessage: hexutil.Bytes{},
	}
}

// newUnverifiedStorage stores a round for testutil.TestSigningPolicy: total weight 7, threshold 3.
func newUnverifiedStorage(t *testing.T, m meta.Meta, expiration time.Duration) *Storage {
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

func TestUnverifiedThresholds(t *testing.T) {
	tests := []struct {
		name          string
		total, t      uint16
		n             int
		cT            uint64
		wantT         uint16
		wantCosigners uint64
	}{
		{name: "no cosigners", total: 7, t: 3, wantT: 3, wantCosigners: 0},
		{name: "cosigners", total: 100, t: 55, n: 3, cT: 2, wantT: 44, wantCosigners: 1},
		{name: "all cosigners required", total: 100, t: 60, n: 2, cT: 2, wantT: 39, wantCosigners: 0},
		{name: "no cosigner required", total: 100, t: 60, n: 2, cT: 0, wantT: 39, wantCosigners: 2},
		{name: "threshold at total", total: 100, t: 100, wantT: 0, wantCosigners: 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotT, gotCosigners := unverifiedThresholds(tc.total, tc.t, tc.n, tc.cT)
			require.Equal(t, tc.wantT, gotT)
			require.Equal(t, tc.wantCosigners, gotCosigners)
		})
	}
}

func TestBuildVoteBoxUnverified(t *testing.T) {
	data := fdcVote("build", nil)
	weights := []uint16{50, 30, 20}

	t.Run("policy threshold", func(t *testing.T) {
		round := createRound(policyWithThreshold(t, weights, 55), 10, false)

		box, err := buildVoteBox(data, key1, round, &bipsMeta{-1}, time.Minute)
		require.NoError(t, err)
		require.True(t, box.proposal.unverified)
		require.Equal(t, uint16(44), box.proposal.threshold)
	})

	t.Run("bips threshold", func(t *testing.T) {
		round := createRound(policyWithThreshold(t, weights, 55), 10, false)

		box, err := buildVoteBox(data, key1, round, &bipsMeta{6000}, time.Minute)
		require.NoError(t, err)
		require.Equal(t, uint16(39), box.proposal.threshold)
	})

	t.Run("cosigners", func(t *testing.T) {
		round := createRound(policyWithThreshold(t, weights, 55), 10, false)
		m := &fixedThresholdMeta{
			cosigners:          map[common.Address]bool{common.HexToAddress("c1"): true, common.HexToAddress("c2"): true, common.HexToAddress("c3"): true},
			cosignersThreshold: 2,
		}

		box, err := buildVoteBox(data, key1, round, m, time.Minute)
		require.NoError(t, err)
		require.Equal(t, uint16(1), box.proposal.cosignerThreshold)
	})

	t.Run("a response keeps the verified thresholds", func(t *testing.T) {
		round := createRound(policyWithThreshold(t, weights, 55), 10, false)

		box, err := buildVoteBox(fdcVote("build", []byte("response")), key1, round, &bipsMeta{-1}, time.Minute)
		require.NoError(t, err)
		require.False(t, box.proposal.unverified)
		require.Equal(t, uint16(55), box.proposal.threshold)
	})
}

func TestUnverifiedVotingFinalizes(t *testing.T) {
	s := newUnverifiedStorage(t, &testMeta{}, time.Minute)
	data := fdcVote("finalizes", nil)

	// 3 of 7 leaves 4 > 3 for the verified voting
	addVotes(t, s, data, key2)
	require.Empty(t, s.Unverified)

	// 4 of 7 leaves 3, which is not above the threshold
	addVotes(t, s, data, key1)
	require.Len(t, s.Unverified, 1)
	require.Empty(t, s.Out, "nothing goes to the node")

	u := <-s.Unverified
	require.Equal(t, data.InstructionID, u.InstructionID)
	require.Equal(t, []common.Address{key2, key1}, u.Voters)

	r, ok := s.Get(1)
	require.True(t, ok)
	hash, err := data.HashFixed()
	require.NoError(t, err)
	r.Voting.RLock()
	boxes := r.Voting.M[data.InstructionID]
	r.Voting.RUnlock()
	require.Equal(t, hash, boxes.FinalizedHash)
	require.True(t, boxes.M[hash].Status().Unverified)
}

func TestUnverifiedVotingCosignersBlock(t *testing.T) {
	c1, c2 := common.HexToAddress("c1"), common.HexToAddress("c2")
	cosigners := map[common.Address]bool{c1: true, c2: true}

	t.Run("one rejection blocks a threshold of all", func(t *testing.T) {
		s := newUnverifiedStorage(t, &fixedThresholdMeta{cosigners: cosigners, cosignersThreshold: 2}, time.Minute)

		data := fdcVote("all", nil)

		addVotes(t, s, data, key1) // only a provider may open a voting
		require.Empty(t, s.Unverified)

		addVotes(t, s, data, c1)
		require.Len(t, s.Unverified, 1)
	})

	t.Run("a threshold of one needs both", func(t *testing.T) {
		s := newUnverifiedStorage(t, &fixedThresholdMeta{cosigners: cosigners, cosignersThreshold: 1}, time.Minute)
		data := fdcVote("one", nil)

		addVotes(t, s, data, key1, c1)
		require.Empty(t, s.Unverified)

		addVotes(t, s, data, c2)
		require.Len(t, s.Unverified, 1)
	})
}

func TestUnverifiedFirstOutcomeWins(t *testing.T) {
	t.Run("verified first", func(t *testing.T) {
		s := newUnverifiedStorage(t, &testMeta{}, time.Minute)

		addVotes(t, s, fdcVote("verified-first", []byte("response")), key2, key3)
		require.Len(t, s.Out, 1)

		addVotes(t, s, fdcVote("verified-first", nil), key2, key1)
		require.Empty(t, s.Unverified)
	})

	t.Run("unverified first", func(t *testing.T) {
		s := newUnverifiedStorage(t, &testMeta{}, time.Minute)

		addVotes(t, s, fdcVote("unverified-first", nil), key2, key1)
		require.Len(t, s.Unverified, 1)

		addVotes(t, s, fdcVote("unverified-first", []byte("response")), key2, key3)
		require.Empty(t, s.Out)
	})
}

func TestUnverifiedVotingSendsNoEndAction(t *testing.T) {
	s := newUnverifiedStorage(t, &testMeta{}, 200*time.Millisecond)
	data := fdcVote("no-end", nil)

	addVotes(t, s, data, key2, key1)
	require.Len(t, s.Unverified, 1)

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
