package instruction

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/flare-foundation/go-flare-common/pkg/tee/op"
	"github.com/stretchr/testify/require"

	"github.com/flare-foundation/fcc-proxy/internal/service/instruction/voting"
	"github.com/flare-foundation/fcc-proxy/internal/testutil"
	"github.com/flare-foundation/fcc-proxy/pkg/instruction/meta"
)

type fakeDeclinedStore struct {
	mu    sync.Mutex
	calls []voting.Declined
}

func (f *fakeDeclinedStore) StoreDeclined(_ context.Context, id, opType, opCommand common.Hash, voters []common.Address) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, voting.Declined{InstructionID: id, OPType: opType, OPCommand: opCommand, Voters: voters})
	return nil
}

func (f *fakeDeclinedStore) stored() []voting.Declined {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]voting.Declined(nil), f.calls...)
}

func TestForwardStoresDeclined(t *testing.T) {
	teeID := common.HexToAddress("dead")
	mr, c, s := setupInstructionService(t, teeID, testutil.TestSigningPolicy)
	defer mr.Close()
	defer c.Close() //nolint:errcheck

	store := &fakeDeclinedStore{}
	s.results = store

	go s.Forward(t.Context()) //nolint:errcheck // ends with the test ctx

	iData := createBaseInstructionData("TestForwardStoresDeclined", teeID)
	iData.AdditionalFixedMessage = hexutil.Bytes(meta.Decline)

	// FDC2 declines need 7 - min(3, 2) = 5 of 7
	_, err := s.ServeInstruction(t.Context(), signInstruction(t, iData, testutil.PrivKey2))
	require.NoError(t, err)
	_, err = s.ServeInstruction(t.Context(), signInstruction(t, iData, testutil.PrivKey3))
	require.NoError(t, err)

	require.Eventually(t, func() bool { return len(store.stored()) == 1 }, time.Second, 10*time.Millisecond)
	require.Equal(t, voting.Declined{
		InstructionID: iData.InstructionID,
		OPType:        op.FDC2.Hash(),
		OPCommand:     op.Prove.Hash(),
		Voters: []common.Address{
			crypto.PubkeyToAddress(testutil.PrivKey2.PublicKey),
			crypto.PubkeyToAddress(testutil.PrivKey3.PublicKey),
		},
	}, store.stored()[0])
}
