package policy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/flare-foundation/fcc-node/pkg/types"
	"github.com/flare-foundation/fcc-proxy/internal/queue"
	"github.com/flare-foundation/fcc-proxy/pkg/config"
	"github.com/flare-foundation/go-flare-common/pkg/contracts/registry"
	"github.com/flare-foundation/go-flare-common/pkg/contracts/relay"
	"github.com/flare-foundation/go-flare-common/pkg/policy"
	"github.com/flare-foundation/go-flare-common/pkg/tee/op"
)

var errNoSigningPolicyEvents = errors.New("no signing policy events found")

// UpdateFailureReason maps an UpdatePolicyAction error to a bounded metric reason
// (pubkey_mismatch/sig_deadline/indexer/not_consecutive/build_failed); "" for nil.
func UpdateFailureReason(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, errWrongAddressRecovered):
		return "pubkey_mismatch"
	case errors.Is(err, ErrDeadlineExceeded):
		return "sig_deadline"
	case errors.Is(err, ErrTooManyErrors), errors.Is(err, errNoRegistration):
		return "indexer"
	case errors.Is(err, errNotConsecutivePolicy):
		return "not_consecutive"
	default:
		return "build_failed"
	}
}

// InitializePolicyAction prepares the INITIALIZE_POLICY action from the SigningPolicyInitialized event offset
// before the latest, or the oldest found; it returns the action, the policy and the offset used.
func InitializePolicyAction(
	ctx context.Context,
	src Source,
	addresses config.Addresses,
	offset int,
	chainID *big.Int,
) (*types.Action, *policy.SigningPolicy, int, error) {
	events, err := src.LatestSigningPolicies(ctx, offset+1)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("fetching signing policy events: %w", err)
	}

	if len(events) == 0 {
		return nil, nil, 0, errNoSigningPolicyEvents
	}

	p, err := policy.NewSigningPolicy(events[len(events)-1], nil)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("creating signing policy instance: %w", err)
	}

	msg, err := prepareInitializePolicyActionMessage(ctx, src, addresses.VoterRegistry, p, chainID)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("preparing initialize policy message for reward epoch %d: %w", p.RewardEpochID, err)
	}

	action, err := prepareInitializePolicyAction(msg)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("preparing initialize policy action: %w", err)
	}

	return action, p, len(events) - 1, nil
}

func prepareInitializePolicyAction(msg []byte) (*types.Action, error) {
	return queue.PrepareDirectAction(op.Policy, op.InitializePolicy, msg)
}

func prepareInitializePolicyActionMessage(ctx context.Context, src Source, voterRegistryAddress common.Address, signingPolicy *policy.SigningPolicy, chainID *big.Int) ([]byte, error) {
	pubKeys, err := fetchPubKeys(ctx, src, voterRegistryAddress, signingPolicy, chainID)
	if err != nil {
		return nil, err
	}

	req := &types.InitializePolicyRequest{
		InitialPolicyBytes: signingPolicy.RawBytes(),
		PublicKeys:         pubKeys,
	}

	encoded, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshaling initialize policy request: %w", err)
	}

	return encoded, nil
}

// FetchSigningPolicy fetches and parses the SigningPolicyInitialized event for the given
// policy ID. found is false (with a nil error) when no such event exists — i.e. the
// reward epoch is not in the relay's history held by src.
func FetchSigningPolicy(ctx context.Context, src Source, signingPolicyID uint32) (*policy.SigningPolicy, bool, error) {
	event, found, err := FetchSigningPolicyEvent(ctx, src, signingPolicyID)
	if err != nil || !found {
		return nil, false, err
	}

	p, err := policy.NewSigningPolicy(event, nil)
	if err != nil {
		return nil, false, fmt.Errorf("creating signing policy %d instance: %w", signingPolicyID, err)
	}

	return p, true, nil
}

// FetchSigningPolicyEvent fetches the SigningPolicyInitialized event for the given
// policy ID. found is false (with a nil error) when no such event exists yet — i.e.
// the reward epoch has not been initialized on chain.
func FetchSigningPolicyEvent(ctx context.Context, src Source, signingPolicyID uint32) (*relay.RelaySigningPolicyInitialized, bool, error) {
	event, found, err := src.SigningPolicy(ctx, signingPolicyID)
	if err != nil {
		return nil, false, fmt.Errorf("fetching signing policy %d event: %w", signingPolicyID, err)
	}

	// the indexer cannot filter on epoch 0's zero-hash topic and answers with the
	// newest event instead — reject an event for a different epoch
	if !found || event.RewardEpochId == nil || !event.RewardEpochId.IsUint64() || event.RewardEpochId.Uint64() != uint64(signingPolicyID) {
		return nil, false, nil
	}

	return event, true, nil
}

// prepareSignatures transforms a slice of Signatures to a slice SignatureMessage.
func prepareSignatures(sigs []*registry.Signature) [][]byte {
	msgs := make([][]byte, len(sigs))

	for k := range sigs {
		msgs[k] = serializeSig(sigs[k])
	}

	return msgs
}

// UpdatePolicyAction prepares an UPDATE_POLICY action from a SigningPolicyInitialized event.
func UpdatePolicyAction(ctx context.Context, src Source, addresses config.Addresses, event *relay.RelaySigningPolicyInitialized, activePolicy *policy.SigningPolicy, chainID *big.Int) (*types.Action, *policy.SigningPolicy, error) {
	p, err := policy.NewSigningPolicy(event, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("creating signing policy instance: %w", err)
	}

	// the signature scan range is (start, to], so start one block below the event
	start := event.Raw.BlockNumber
	if start > 0 {
		start--
	}

	msg, err := prepareUpdatePolicyMessage(ctx, src, addresses.VoterRegistry, p, activePolicy, start, chainID)
	if err != nil {
		return nil, nil, fmt.Errorf("preparing update policy message: %w", err)
	}

	action, err := prepareUpdatePolicyAction(msg)
	if err != nil {
		return nil, nil, fmt.Errorf("preparing update policy action: %w", err)
	}

	return action, p, nil
}

func prepareUpdatePolicyAction(msg []byte) (*types.Action, error) {
	return queue.PrepareDirectAction(op.Policy, op.UpdatePolicy, msg)
}

// policyRolloverSignatureDeadline caps one rollover attempt's wait for signNewSigningPolicy signatures;
// later attempts collect late ones. A var for tests.
var policyRolloverSignatureDeadline = 30 * time.Minute

func prepareUpdatePolicyMessage(ctx context.Context, src Source, voterRegistryAddress common.Address, nextPolicy *policy.SigningPolicy, activePolicy *policy.SigningPolicy, start uint64, chainID *big.Int) ([]byte, error) {
	deadline := time.Now().Add(policyRolloverSignatureDeadline)

	sigs, err := collectSignatures(ctx, src, start, uint64(deadline.Unix()), nextPolicy, activePolicy, chainID.Uint64())
	if err != nil {
		return nil, fmt.Errorf("collecting signatures: %w", err)
	}

	pubKeys, err := fetchPubKeys(ctx, src, voterRegistryAddress, nextPolicy, chainID)
	if err != nil {
		return nil, err
	}

	req := types.UpdatePolicyRequest{
		NewPolicy: types.MultiSignedPolicy{
			PolicyBytes: nextPolicy.RawBytes(),
			Signatures:  prepareSignatures(sigs),
		},
		PublicKeys: pubKeys,
	}

	msg, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshaling update policy request: %w", err)
	}

	return msg, nil
}
