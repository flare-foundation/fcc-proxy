// Package machinepath assembles SET_MACHINE_PATH_LIST direct actions for the
// FCC node from governance-signed machine path lists recorded on chain by the
// MachinePathManager facet.
package machinepath

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/ethereum/go-ethereum/common"
	"github.com/flare-foundation/fcc-node/pkg/types"
	teeutils "github.com/flare-foundation/fcc-node/pkg/utils"
	"github.com/flare-foundation/fcc-proxy/internal/queue"
	"github.com/flare-foundation/go-flare-common/pkg/contracts/tee/machinepathmanager"
	"github.com/flare-foundation/go-flare-common/pkg/safe"
	csigning "github.com/flare-foundation/go-flare-common/pkg/signing"
	"github.com/flare-foundation/go-flare-common/pkg/tee/op"
)

var errNoPaths = errors.New("no machine paths found for list")

// ErrNoAuthorization marks an activated list with neither direct governance signatures nor a verifiable Safe approval.
var ErrNoAuthorization = errors.New("no governance signatures or safe approvals found for list")

// SetMachinePathListAction builds the SET_MACHINE_PATH_LIST direct action for the list (extensionID, nonce)
// from src: its paths in on-chain order, the governance signatures (only gov.Signers' when known, as the node
// rejects any other signer) and, for a Safe-backed gov, one pre-verified Safe approval.
// With neither it returns ErrNoAuthorization. gov is the node's reported governance.
func SetMachinePathListAction(
	ctx context.Context,
	src Source,
	managerAddress common.Address,
	gov types.Governance,
	extensionID common.Hash,
	chainID uint64,
	nonce uint64,
) (*types.Action, error) {
	list, err := src.SignedList(ctx, extensionID, nonce, gov)
	if err != nil {
		return nil, fmt.Errorf("fetching machine path list: %w", err)
	}
	if len(list.Paths) == 0 {
		return nil, errNoPaths
	}

	dataHash, err := types.MachinePathListDataHash(extensionID, nonce, list.Paths)
	if err != nil {
		return nil, fmt.Errorf("computing machine path list data hash: %w", err)
	}
	hash, err := csigning.NewPayload(csigning.TEEMachinePathList, chainID, dataHash).Hash()
	if err != nil {
		return nil, fmt.Errorf("computing machine path list message hash: %w", err)
	}

	sigs := collectSignatures(list.Signatures, hash, gov.Signers)

	var approval *safe.Approval
	if gov.Safe != (common.Address{}) {
		approval, err = selectApproval(list.Approvals, gov, managerAddress, extensionID, nonce, hash, chainID)
		if err != nil {
			return nil, fmt.Errorf("selecting safe approval: %w", err)
		}
	}

	if len(sigs) == 0 && approval == nil {
		return nil, ErrNoAuthorization
	}

	req := types.SetMachinePathListRequest{
		Paths:        list.Paths,
		Nonce:        nonce,
		Signatures:   sigs,
		SafeApproval: approval,
	}

	msg, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshaling set machine path list request: %w", err)
	}

	return queue.PrepareDirectAction(op.Governance, op.SetMachinePathList, msg)
}

// collectSignatures serializes the first signature per signer recovered against hash, only from signers
// when non-empty; an empty result is not an error.
func collectSignatures(signatures []machinepathmanager.Signature, hash common.Hash, signers []common.Address) [][]byte {
	sigs := make([][]byte, 0, len(signatures))
	seen := make(map[common.Address]bool, len(signatures))

	for _, sig := range signatures {
		serialized := serializeSig(sig)
		signer, err := teeutils.SignatureToSignersAddress(hash[:], serialized)
		if err != nil || seen[signer] {
			continue
		}
		if len(signers) > 0 && !slices.Contains(signers, signer) {
			continue
		}

		seen[signer] = true
		sigs = append(sigs, serialized)
	}

	return sigs
}

// serializeSig serializes a contract Signature to [R||S||V-27].
func serializeSig(s machinepathmanager.Signature) []byte {
	sig := make([]byte, 0, 65)
	sig = append(sig, s.R[:]...)
	sig = append(sig, s.S[:]...)
	sig = append(sig, s.V-27)

	return sig
}
