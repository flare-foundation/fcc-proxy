package voting

import (
	"crypto/ecdsa"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/flare-foundation/fcc-node/pkg/types"
	"github.com/flare-foundation/go-flare-common/pkg/tee/instruction"
	"github.com/flare-foundation/go-flare-common/pkg/tee/op"
	"github.com/stretchr/testify/require"

	"github.com/flare-foundation/fcc-proxy/internal/testutil"
	"github.com/flare-foundation/fcc-proxy/pkg/config"
)

func restoreStorage(t *testing.T, proposalExpiration, restoreExpiration time.Duration) *Storage {
	t.Helper()

	s := NewStorage(t.Context(), &config.Voting{
		ProposalExpiration:  proposalExpiration,
		RestoreExpiration:   restoreExpiration,
		MaxPendingRequests:  10,
		HistorySize:         3,
		FinalizedBufferSize: 3,
	}, &testMeta{}, nil)
	s.StoreNewRound(testutil.TestSigningPolicy)

	return s
}

func walletInstruction(cmd op.Command, id string) *instruction.Data {
	return &instruction.Data{
		InstructionID:             crypto.Keccak256Hash([]byte(id)),
		TeeID:                     common.HexToAddress("dead"),
		Timestamp:                 uint64(time.Now().Unix()),
		RewardEpochID:             1,
		OPType:                    op.Wallet.Hash(),
		OPCommand:                 cmd.Hash(),
		OriginalMessage:           []byte("TODO"),
		AdditionalFixedMessage:    hexutil.Bytes{},
		AdditionalVariableMessage: hexutil.Bytes{},
	}
}

func addVote(t *testing.T, s *Storage, i *instruction.Data, key *ecdsa.PrivateKey) {
	t.Helper()

	h, err := i.HashForSigning(voteTestChainID)
	require.NoError(t, err)
	sig, err := instruction.SignInstructionHash(h, key)
	require.NoError(t, err)

	_, err = s.AddVote(i, crypto.PubkeyToAddress(key.PublicKey), sig)
	require.NoError(t, err)
}

func receiveAction(t *testing.T, out <-chan *types.Action) *types.Action {
	t.Helper()

	select {
	case a := <-out:
		return a
	case <-time.After(5 * time.Second):
		require.FailNow(t, "no action received")
		return nil
	}
}

func TestRestoreWindow(t *testing.T) {
	tests := []struct {
		cmd  op.Command
		want time.Duration
	}{
		{op.KeyDataProviderRestore, time.Hour},
		{op.KeyDataProviderRestoreTest, time.Minute},
		{op.KeyGenerate, time.Minute},
	}

	for _, tt := range tests {
		t.Run(string(tt.cmd), func(t *testing.T) {
			s := restoreStorage(t, time.Minute, time.Hour)
			i := walletInstruction(tt.cmd, string(tt.cmd))
			addVote(t, s, i, testutil.PrivKey1)

			round, ok := s.Get(1)
			require.True(t, ok)
			hash, err := i.HashFixed()
			require.NoError(t, err)

			round.Voting.RLock()
			box := round.Voting.M[i.InstructionID].M[hash]
			round.Voting.RUnlock()

			st := box.Status()
			require.Equal(t, uint64(tt.want/time.Second), st.End-st.Start)
		})
	}
}

func TestRestoreThresholdDelayed(t *testing.T) {
	const window = 2 * time.Second
	const delay = 300 * time.Millisecond

	s := restoreStorage(t, time.Minute, window)
	s.thresholdDelay = delay
	i := walletInstruction(op.KeyDataProviderRestore, "delayed")

	start := time.Now()
	addVote(t, s, i, testutil.PrivKey1)
	addVote(t, s, i, testutil.PrivKey2) // finalizes

	threshold := receiveAction(t, s.Out)
	require.Equal(t, types.Threshold, threshold.Data.SubmissionTag)
	require.GreaterOrEqual(t, time.Since(start), delay)
	require.Less(t, time.Since(start), window)
	require.Len(t, threshold.Signatures, 2)

	// a share after the threshold action still reaches the end action
	addVote(t, s, i, testutil.PrivKey3)

	end := receiveAction(t, s.Out)
	require.Equal(t, types.End, end.Data.SubmissionTag)
	require.GreaterOrEqual(t, time.Since(start), window)
	require.Len(t, end.Signatures, 3)
}

func TestRestoreThresholdCappedAtEnd(t *testing.T) {
	const window = 300 * time.Millisecond

	s := restoreStorage(t, time.Minute, window) // default delay outlasts the window
	i := walletInstruction(op.KeyDataProviderRestore, "capped")

	start := time.Now()
	addVote(t, s, i, testutil.PrivKey1)
	addVote(t, s, i, testutil.PrivKey2) // finalizes

	threshold := receiveAction(t, s.Out)
	require.Equal(t, types.Threshold, threshold.Data.SubmissionTag)
	require.GreaterOrEqual(t, time.Since(start), window)

	end := receiveAction(t, s.Out)
	require.Equal(t, types.End, end.Data.SubmissionTag)
}
