package limiter

import (
	"bytes"
	"fmt"
	"sort"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/flare-foundation/fcc-proxy/pkg/status"
)

var (
	ErrCannotInitialize = fmt.Errorf("%w: cannot initialize voting", status.HTTP[403])
	ErrLimitReached     = fmt.Errorf("%w: propose limit reached", status.HTTP[429])
)

// Limiter caps the unfinalized proposals each registered data provider may have open in a reward epoch.
// Expired proposals stay counted, so letting proposals expire does not evade the cap.
type Limiter struct {
	counter map[common.Address]*State

	maxPendingRequests uint

	sync.RWMutex
}

// State tracks per-voter proposal counters.
type State struct {
	pending        uint
	TotalProposed  int
	TotalCompleted int
}

// New creates a Limiter that allows each voter at most maxPendingRequests pending proposals.
func New(voters []common.Address, maxPendingRequests uint) *Limiter {
	c := make(map[common.Address]*State)

	for _, voter := range voters {
		c[voter] = &State{
			pending:        0,
			TotalProposed:  0,
			TotalCompleted: 0,
		}
	}

	return &Limiter{
		counter:            c,
		maxPendingRequests: maxPendingRequests,
	}
}

// Add adds zero state for an address if it is not already present.
func (l *Limiter) Add(address common.Address) {
	l.Lock()
	defer l.Unlock()

	_, exists := l.counter[address]
	if !exists {
		l.counter[address] = &State{
			pending:        0,
			TotalProposed:  0,
			TotalCompleted: 0,
		}
	}
}

// Increment increments counter for address in round and returns error if address is not eligible start the vote.
func (l *Limiter) Increment(address common.Address) error {
	l.Lock()
	defer l.Unlock()

	state, exists := l.counter[address]
	if !exists {
		return ErrCannotInitialize
	}

	// Check if the validator has too many pending requests
	if state.pending >= l.maxPendingRequests {
		return ErrLimitReached
	}

	state.pending++
	state.TotalProposed++

	return nil
}

// VoterPending pairs a voter with its count of unfinalised (pending) proposals.
type VoterPending struct {
	Address common.Address
	Pending uint
}

// TopPending returns up to n voters with the most unfinalised proposals, highest first,
// excluding any with zero pending. Ties are broken by address for a stable ordering.
func (l *Limiter) TopPending(n int) []VoterPending {
	l.RLock()
	pending := make([]VoterPending, 0, len(l.counter))
	for addr, state := range l.counter {
		if state.pending > 0 {
			pending = append(pending, VoterPending{Address: addr, Pending: state.pending})
		}
	}
	l.RUnlock()

	return SortTopPending(pending, n)
}

// SortTopPending sorts pending by count descending, then address ascending, and truncates it to n
// (all if n < 0); it takes no lock.
func SortTopPending(pending []VoterPending, n int) []VoterPending {
	sort.Slice(pending, func(i, j int) bool {
		if pending[i].Pending != pending[j].Pending {
			return pending[i].Pending > pending[j].Pending
		}
		return bytes.Compare(pending[i].Address.Bytes(), pending[j].Address.Bytes()) < 0
	})

	if n >= 0 && len(pending) > n {
		pending = pending[:n]
	}
	return pending
}

// Decrement releases one of address's pending proposals; call it only on finalization (see Limiter).
func (l *Limiter) Decrement(address common.Address) {
	l.Lock()
	defer l.Unlock()

	state, exists := l.counter[address]
	if !exists {
		return
	}

	if state.pending > 0 {
		state.pending--
	}

	state.TotalCompleted++
}
