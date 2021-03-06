/*
Package protocol provides the logic to tie together
storage and validation for a Chain Protocol blockchain.

This comprises all behavior that's common to every full
node, as well as other functions that need to operate on the
blockchain state.

Here are a few examples of typical full node types.

Generator

A generator has two basic jobs: collecting transactions from
other nodes and putting them into blocks.

To add a new block to the blockchain, call GenerateBlock,
sign the block (possibly collecting signatures from other
parties), and call CommitAppliedBlock.

Signer

A signer validates blocks generated by the Generator and signs
at most one block at each height.

Participant

A participant node in a network may select outputs for spending
and compose transactions.

To publish a new transaction, prepare your transaction
(select outputs, and compose and sign the tx) and send the
transaction to the network's generator. To wait for
confirmation, call BlockWaiter on successive block heights
and inspect the blockchain state until you find that the
transaction has been either confirmed or rejected. Note
that transactions may be malleable if there's no commitment
to TXSIGHASH.

New block sequence

Every new block must be validated against the existing
blockchain state. New blocks are validated by calling
ValidateBlock. Blocks produced by GenerateBlock are already
known to be valid.

A new block goes through the sequence:
  - If not generated locally, the block is validated by
    calling ValidateBlock.
  - The new block is committed to the Chain's Store through
    its SaveBlock method. This is the linearization point.
    Once a block is saved to the Store, it's committed and
    can be recovered after a crash.
  - The Chain's in-memory representation of the blockchain
    state is updated. If the block was remotely-generated,
    the Chain must apply the new block to its current state
    to retrieve the new state. If the block was generated
    locally, the resulting state is already known and does
    not need to be recalculated.
  - Other cored processes are notified of the new block
    through Store.FinalizeHeight.

Committing a block

As a consumer of the package, there are two ways to
commit a new block: CommitBlock and CommitAppliedBlock.

When generating new blocks, GenerateBlock will return
the resulting state snapshot with the new block. To
ingest a block with a known resulting state snapshot,
call CommitAppliedBlock.

When ingesting remotely-generated blocks, the state after
the block must be calculated by taking the Chain's
current state and applying the new block. To ingest a
block without a known resulting state snapshot, call
CommitBlock.
*/
package protocol

import (
	"context"
	"sync"
	"time"

	"github.com/chain/txvm/errors"
	"github.com/chain/txvm/log"
	"github.com/chain/txvm/protocol/bc"
	"github.com/chain/txvm/protocol/state"
)

var (
	// ErrTheDistantFuture is returned when waiting for a blockheight
	// too far in excess of the tip of the blockchain.
	ErrTheDistantFuture = errors.New("block height too far in future")
)

// Store provides storage for blockchain data: blocks and state tree
// snapshots.
//
// Note, this is different from a state snapshot. A state snapshot
// provides access to the state at a given point in time -- outputs
// and issuance memory. The Chain type uses Store to load state
// from storage and persist validated data.
type Store interface {
	Height(context.Context) (uint64, error)
	GetBlock(context.Context, uint64) (*bc.Block, error)
	LatestSnapshot(context.Context) (*state.Snapshot, error)

	SaveBlock(context.Context, *bc.Block) error
	FinalizeHeight(context.Context, uint64) error
	SaveSnapshot(context.Context, *state.Snapshot) error
}

// Chain provides a complete, minimal blockchain database. It
// delegates the underlying storage to other objects, and uses
// validation logic from package validation to decide what
// objects can be safely stored.
type Chain struct {
	InitialBlockHash bc.Hash

	// only used by generators
	MaxNonceWindow time.Duration
	MaxBlockWindow uint64

	state struct {
		cond     sync.Cond // protects height, block, snapshot
		height   uint64
		snapshot *state.Snapshot // current only if leader
	}
	store Store

	lastQueuedSnapshotMS uint64
	pendingSnapshots     chan *state.Snapshot
}

// NewChain returns a new Chain using store as the underlying storage.
func NewChain(ctx context.Context, initialBlock *bc.Block, store Store, heights <-chan uint64) (*Chain, error) {
	c := &Chain{
		InitialBlockHash: initialBlock.Hash(),
		store:            store,
		pendingSnapshots: make(chan *state.Snapshot, 1),
	}
	c.state.cond.L = new(sync.Mutex)
	c.state.snapshot = state.Empty()

	var err error
	c.state.height, err = store.Height(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "looking up blockchain height")
	}

	// Note that c.state.height may still be zero here.
	if heights != nil {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case h := <-heights:
					c.setHeight(h)
				}
			}
		}()
	}

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case s := <-c.pendingSnapshots:
				err = store.SaveSnapshot(ctx, s)
				if err != nil {
					log.Error(ctx, err, "at", "saving snapshot")
				}
			}
		}
	}()

	return c, nil
}

// Height returns the current height of the blockchain.
func (c *Chain) Height() uint64 {
	c.state.cond.L.Lock()
	defer c.state.cond.L.Unlock()
	return c.state.height
}

// State returns the most recent state available. It will not be current
// unless the current process is the leader. Callers should examine the
// returned state header's height if they need to verify the current state.
func (c *Chain) State() *state.Snapshot {
	c.state.cond.L.Lock()
	defer c.state.cond.L.Unlock()
	return c.state.snapshot
}

func (c *Chain) setState(s *state.Snapshot) {
	c.state.cond.L.Lock()
	defer c.state.cond.L.Unlock()

	// Multiple goroutines may attempt to set the state at the
	// same time. If b is an older block than c.state, ignore it.
	if s.Height() <= c.state.snapshot.Height() {
		return
	}

	c.state.snapshot = s
	if s.Height() > c.state.height {
		c.state.height = s.Height()
		c.state.cond.Broadcast()
	}
}

func (c *Chain) setHeight(h uint64) {
	// We update c.state.height from multiple places:
	// setState and here, called by the Postgres LISTEN
	// goroutine. setHeight must ignore heights less than
	// the current height.

	c.state.cond.L.Lock()
	defer c.state.cond.L.Unlock()

	if h <= c.state.height {
		return
	}
	c.state.height = h
	c.state.cond.Broadcast()
}

// BlockSoonWaiter returns a channel that
// waits for the block at the given height,
// but it is an error to wait for a block far in the future.
// WaitForBlockSoon will timeout if the context times out.
// To wait unconditionally, the caller should use WaitForBlock.
func (c *Chain) BlockSoonWaiter(ctx context.Context, height uint64) <-chan error {
	ch := make(chan error, 1)

	go func() {
		const slop = 3
		if height > c.Height()+slop {
			ch <- ErrTheDistantFuture
			return
		}

		select {
		case <-c.BlockWaiter(height):
			ch <- nil
		case <-ctx.Done():
			ch <- ctx.Err()
		}
	}()

	return ch
}

// BlockWaiter returns a channel that
// waits for the block at the given height.
func (c *Chain) BlockWaiter(height uint64) <-chan struct{} {
	ch := make(chan struct{}, 1)
	go func() {
		c.state.cond.L.Lock()
		defer c.state.cond.L.Unlock()
		for c.state.height < height {
			c.state.cond.Wait()
		}
		ch <- struct{}{}
	}()

	return ch
}
