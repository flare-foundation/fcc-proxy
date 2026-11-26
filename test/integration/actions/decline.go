package actions

import (
	"crypto/ecdsa"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/flare-foundation/fcc-node/pkg/fdc"
	"github.com/flare-foundation/fcc-node/pkg/types"
	"github.com/flare-foundation/go-flare-common/pkg/policy"
	"github.com/flare-foundation/go-flare-common/pkg/random"
	"github.com/flare-foundation/go-flare-common/pkg/tee/instruction"
	"github.com/flare-foundation/go-flare-common/pkg/tee/op"
	"github.com/flare-foundation/go-flare-common/pkg/tee/structs/fdc2"
	"github.com/flare-foundation/go-flare-common/pkg/tee/structs/payments"
	"github.com/stretchr/testify/require"

	"github.com/flare-foundation/fcc-proxy/test/integration/utils"
)

const (
	// declineMarker is fcc-relay-client's instructions.Decline, which the proxy matches byte for byte.
	declineMarker = "DECLINE"

	statusDeclined = 255
)

// FDCDecline declines a fresh FDC2 PROVE from just enough providers to finalize the decline voting
// and checks the proxy's declined result.
func FDCDecline(t *testing.T, pc *utils.ProxyConfig, sp *policy.SigningPolicy, providerPrivKeys, cosignerPrivKeys []*ecdsa.PrivateKey) {
	t.Helper()

	_, request := fdcRequest(t)
	cosigners, _ := CosignerAddressesAndProvider(cosignerPrivKeys, providerPrivKeys)
	iData := utils.BuildInstructionData(t, op.FDC2, op.Prove, request, uint64(time.Now().Unix()), []byte(declineMarker), nil, cosigners, uint64(len(cosigners)), pc.TeeID, sp.RewardEpochID)

	n := prefixAbove(t, sp, providerPrivKeys, declineThreshold(sp, true))
	end := time.NewTimer(pc.Vc.ProposalExpiration)
	defer end.Stop()
	receipts := utils.SignAndSendInstructions(t, iData, providerPrivKeys[:n], pc.ExtPort)
	utils.VerifyReceipts(t, receipts, iData)

	requireDeclined(t, pc, iData.InstructionID, op.FDC2, op.Prove, addresses(providerPrivKeys[:n]))

	statuses := utils.GetVotingStatuses(t, pc, sp.RewardEpochID, iData.InstructionID)
	require.Len(t, statuses.Status, 1)
	require.True(t, statuses.Status[0].Declined)
	require.True(t, statuses.Status[0].Finalized)
	require.Equal(t, declineThreshold(sp, true), statuses.Status[0].Threshold)

	// a declined instruction gets no end result
	<-end.C
	require.Never(t, func() bool {
		code, err := utils.ActionResponseCode(pc.ExtPort, iData.InstructionID, types.End)
		return err != nil || code != http.StatusNotFound
	}, time.Second, utils.TestTimeConfig.Interval)
}

// FDCProveWithDeclines proves a fresh FDC2 PROVE while a minority declines it first, and enough
// providers decline it after it finalizes to finalize the decline voting too; the node's result stays.
func FDCProveWithDeclines(t *testing.T, pc *utils.ProxyConfig, sp *policy.SigningPolicy, providerPrivKeys, cosignerPrivKeys []*ecdsa.PrivateKey) {
	t.Helper()

	iData, variableMessages, privKeys := fdcProof(t, pc, sp, providerPrivKeys, cosignerPrivKeys)
	decline := declineOf(iData)

	minority := prefixAtMost(sp, providerPrivKeys, sp.Voters.TotalWeight/4)
	majority := prefixAbove(t, sp, providerPrivKeys, declineThreshold(sp, true))
	require.Less(t, minority, majority)

	end := time.NewTimer(pc.Vc.ProposalExpiration)
	defer end.Stop()

	utils.SignAndSendInstructions(t, decline, providerPrivKeys[:minority], pc.ExtPort)
	// the last cosigner's vote finalizes the instruction before the declines below
	receipts, instructions := utils.SignAndSendInstructionsWithAddVarMsgs(t, iData, variableMessages[minority:], privKeys[minority:], pc.ExtPort)
	utils.SignAndSendInstructions(t, decline, providerPrivKeys[minority:majority], pc.ExtPort)

	utils.VerifyReceiptsForMultipleInstructions(t, receipts, instructions)
	utils.FetchAndVerifyActionResponse(t, pc.ExtPort, iData.InstructionID, types.Threshold, op.FDC2, op.Prove, pc.TeeID, 1)

	<-end.C
	statuses := utils.GetVotingStatuses(t, pc, sp.RewardEpochID, iData.InstructionID)
	require.Len(t, statuses.Status, 2)
	for _, s := range statuses.Status {
		require.True(t, s.Finalized)
	}
	require.NotEqual(t, statuses.Status[0].Declined, statuses.Status[1].Declined)

	utils.FetchAndVerifyRewardingData(t, pc, iData.InstructionID, op.FDC2, op.Prove, receipts)
	utils.FetchAndVerifyActionResponse(t, pc.ExtPort, iData.InstructionID, types.Threshold, op.FDC2, op.Prove, pc.TeeID, 1)
}

// FDCDeclineThenProve declines a fresh FDC2 PROVE, then proves it with providers that also declined it;
// the node's result replaces the declined one.
func FDCDeclineThenProve(t *testing.T, pc *utils.ProxyConfig, sp *policy.SigningPolicy, providerPrivKeys, cosignerPrivKeys []*ecdsa.PrivateKey) {
	t.Helper()

	iData, variableMessages, privKeys := fdcProof(t, pc, sp, providerPrivKeys, cosignerPrivKeys)
	id := iData.InstructionID

	n := prefixAbove(t, sp, providerPrivKeys, declineThreshold(sp, true))
	utils.SignAndSendInstructions(t, declineOf(iData), providerPrivKeys[:n], pc.ExtPort)
	requireDeclined(t, pc, id, op.FDC2, op.Prove, addresses(providerPrivKeys[:n]))

	end := time.NewTimer(pc.Vc.ProposalExpiration)
	defer end.Stop()
	receipts, instructions := utils.SignAndSendInstructionsWithAddVarMsgs(t, iData, variableMessages, privKeys, pc.ExtPort)
	utils.VerifyReceiptsForMultipleInstructions(t, receipts, instructions)

	deadline := time.Now().Add(utils.TestTimeConfig.Timeout)
	for utils.FetchActionResponse(t, pc.ExtPort, id, types.Threshold).Result.Status == statusDeclined {
		require.True(t, time.Now().Before(deadline), "declined result not replaced")
		time.Sleep(utils.TestTimeConfig.Interval)
	}
	utils.FetchAndVerifyActionResponse(t, pc.ExtPort, id, types.Threshold, op.FDC2, op.Prove, pc.TeeID, 1)

	<-end.C
	hash, err := iData.HashFixed()
	require.NoError(t, err)
	statuses := utils.GetVotingStatuses(t, pc, sp.RewardEpochID, id)
	require.Equal(t, hash, statuses.FinalizedHash)

	utils.FetchAndVerifyRewardingData(t, pc, id, op.FDC2, op.Prove, receipts)
}

// fdcProof returns a fresh FDC2 PROVE with a verified response, and the votes of all providers and cosigners on it.
func fdcProof(t *testing.T, pc *utils.ProxyConfig, sp *policy.SigningPolicy, providerPrivKeys, cosignerPrivKeys []*ecdsa.PrivateKey) (*instruction.Data, []hexutil.Bytes, []*ecdsa.PrivateKey) {
	t.Helper()

	request, requestEncoded := fdcRequest(t)
	cosigners, cosignerAndProvider := CosignerAddressesAndProvider(cosignerPrivKeys, providerPrivKeys)
	cosignersThreshold := uint64(len(cosigners))

	challenge, err := random.Hash()
	require.NoError(t, err)

	timestamp := uint64(time.Now().Unix())
	response, variableMessages, privKeys, err := GetAdditionalFixedMessage(t, pc, challenge, request, timestamp, cosignerAndProvider, providerPrivKeys, cosignerPrivKeys, cosigners, cosignersThreshold, utils.TestChainID)
	require.NoError(t, err)

	iData := utils.BuildInstructionData(t, op.FDC2, op.Prove, requestEncoded, timestamp, response, nil, cosigners, cosignersThreshold, pc.TeeID, sp.RewardEpochID)

	return iData, variableMessages, privKeys
}

// PaymentDecline declines a fresh XRP PAY from just enough providers to finalize the decline voting
// and checks the proxy's declined result.
func PaymentDecline(t *testing.T, pc *utils.ProxyConfig, sp *policy.SigningPolicy, payment payments.ITeePaymentsPaymentInstructionMessage, providerPrivKeys []*ecdsa.PrivateKey) {
	t.Helper()

	message, err := abi.Arguments{payments.MessageArguments[op.Pay]}.Pack(payment)
	require.NoError(t, err)

	iData := utils.BuildInstructionData(t, op.XRP, op.Pay, message, uint64(time.Now().Unix()), []byte(declineMarker), nil, nil, 0, pc.TeeID, sp.RewardEpochID)

	n := prefixAbove(t, sp, providerPrivKeys, declineThreshold(sp, false))
	utils.SignAndSendInstructions(t, iData, providerPrivKeys[:n], pc.ExtPort)

	requireDeclined(t, pc, iData.InstructionID, op.XRP, op.Pay, addresses(providerPrivKeys[:n]))
}

func requireDeclined(t *testing.T, pc *utils.ProxyConfig, id common.Hash, opType op.Type, opCommand op.Command, voters []common.Address) {
	t.Helper()

	res := utils.FetchActionResponse(t, pc.ExtPort, id, types.Threshold)
	require.Equal(t, uint8(statusDeclined), res.Result.Status)
	require.True(t, strings.HasPrefix(res.Result.Log, "declined:"), res.Result.Log)
	require.Empty(t, res.Signature)
	require.Equal(t, opType.Hash(), res.Result.OPType)
	require.Equal(t, opCommand.Hash(), res.Result.OPCommand)

	var data struct {
		Voters []common.Address `json:"voters"`
	}
	require.NoError(t, json.Unmarshal(res.Result.Data, &data))
	require.Equal(t, voters, data.Voters)
}

func fdcRequest(t *testing.T) (fdc2.IFdc2HubFdc2AttestationRequest, []byte) {
	t.Helper()

	request := fdc2.IFdc2HubFdc2AttestationRequest{
		Header: fdc2.IFdc2HubFdc2RequestHeader{
			ThresholdBIPS: uint16(TotalWeight * 0.6),
		},
		RequestBody: make([]byte, 10),
	}
	encoded, err := fdc.EncodeRequest(request)
	require.NoError(t, err)

	return request, encoded
}

// declineOf returns iData's decline as fcc-relay-client sends it.
func declineOf(iData *instruction.Data) *instruction.Data {
	d := *iData
	d.AdditionalFixedMessage = []byte(declineMarker)
	d.AdditionalVariableMessage = []byte{}

	return &d
}

// declineThreshold is the weight a decline voting must exceed: the total minus the lowest threshold
// the instruction may have, minus one.
func declineThreshold(sp *policy.SigningPolicy, fdcProve bool) uint16 {
	total := sp.Voters.TotalWeight
	threshold := sp.Threshold
	if fdcProve {
		threshold = min(threshold, uint16(uint64(total)*4000/10000)) //nolint:gosec // below total
	}

	return total - threshold - 1
}

// prefixAbove returns the length of the shortest prefix of keys whose weight exceeds w.
func prefixAbove(t *testing.T, sp *policy.SigningPolicy, keys []*ecdsa.PrivateKey, w uint16) int {
	t.Helper()

	var sum uint16
	for i, k := range keys {
		sum += sp.Voters.VoterDataMap[crypto.PubkeyToAddress(k.PublicKey)].Weight
		if sum > w {
			return i + 1
		}
	}
	require.FailNow(t, "providers' weight does not exceed", w)

	return 0
}

// prefixAtMost returns the length of the longest prefix of keys whose weight is at most w.
func prefixAtMost(sp *policy.SigningPolicy, keys []*ecdsa.PrivateKey, w uint16) int {
	var sum uint16
	for i, k := range keys {
		sum += sp.Voters.VoterDataMap[crypto.PubkeyToAddress(k.PublicKey)].Weight
		if sum > w {
			return i
		}
	}

	return len(keys)
}

func addresses(keys []*ecdsa.PrivateKey) []common.Address {
	a := make([]common.Address, len(keys))
	for i, k := range keys {
		a[i] = crypto.PubkeyToAddress(k.PublicKey)
	}

	return a
}
