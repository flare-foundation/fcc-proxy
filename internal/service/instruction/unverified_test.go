package instruction

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/require"

	"github.com/flare-foundation/fcc-proxy/internal/testutil"
)

type unverifiedCall struct {
	id     common.Hash
	voters []common.Address
}

type fakeUnverifiedStore struct {
	mu    sync.Mutex
	calls []unverifiedCall
}

func (f *fakeUnverifiedStore) StoreUnverified(_ context.Context, id common.Hash, voters []common.Address) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, unverifiedCall{id: id, voters: voters})
	return nil
}

func (f *fakeUnverifiedStore) stored() []unverifiedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]unverifiedCall(nil), f.calls...)
}

func TestForwardStoresUnverified(t *testing.T) {
	teeID := common.HexToAddress("dead")
	mr, c, s := setupInstructionService(t, teeID, testutil.TestSigningPolicy)
	defer mr.Close()
	defer c.Close() //nolint:errcheck

	store := &fakeUnverifiedStore{}
	s.results = store

	go s.Forward(t.Context()) //nolint:errcheck // ends with the test ctx

	iData := createBaseInstructionData("TestForwardStoresUnverified", teeID)
	iData.AdditionalFixedMessage = hexutil.Bytes{}

	_, err := s.ServeInstruction(t.Context(), signInstruction(t, iData, testutil.PrivKey2))
	require.NoError(t, err)
	_, err = s.ServeInstruction(t.Context(), signInstruction(t, iData, testutil.PrivKey1))
	require.NoError(t, err)

	require.Eventually(t, func() bool { return len(store.stored()) == 1 }, time.Second, 10*time.Millisecond)
	require.Equal(t, iData.InstructionID, store.stored()[0].id)
	require.Equal(t, []common.Address{
		crypto.PubkeyToAddress(testutil.PrivKey2.PublicKey),
		crypto.PubkeyToAddress(testutil.PrivKey1.PublicKey),
	}, store.stored()[0].voters)
}
