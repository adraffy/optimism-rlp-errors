package finality

import (
	"context"
	"fmt"
	"sync"

	"github.com/ethereum/go-ethereum/log"

	"github.com/ethereum-optimism/optimism/op-node/rollup"
	"github.com/ethereum-optimism/optimism/op-node/rollup/derive"
	"github.com/ethereum-optimism/optimism/op-service/eth"
)

// defaultFinalityLookback defines the amount of L1<>L2 relations to track for finalization purposes, one per L1 block.
//
// When L1 finalizes blocks, it finalizes finalityLookback blocks behind the L1 head.
// Non-finality may take longer, but when it does finalize again, it is within this range of the L1 head.
// Thus we only need to retain the L1<>L2 derivation relation data of this many L1 blocks.
//
// In the event of older finalization signals, misconfiguration, or insufficient L1<>L2 derivation relation data,
// then we may miss the opportunity to finalize more L2 blocks.
// This does not cause any divergence, it just causes lagging finalization status.
//
// The beacon chain on mainnet has 32 slots per epoch,
// and new finalization events happen at most 4 epochs behind the head.
// And then we add 1 to make pruning easier by leaving room for a new item without pruning the 32*4.
const defaultFinalityLookback = 4*32 + 1

// finalityDelay is the number of L1 blocks to traverse before trying to finalize L2 blocks again.
// We do not want to do this too often, since it requires fetching a L1 block by number, so no cache data.
const finalityDelay = 64

// calcFinalityLookback calculates the default finality lookback based on DA challenge window if plasma
// mode is activated or L1 finality lookback.
func calcFinalityLookback(cfg *rollup.Config) uint64 {
	// in alt-da mode the longest finality lookback is a commitment is challenged on the last block of
	// the challenge window in which case it will be both challenge + resolve window.
	if cfg.PlasmaEnabled() {
		lkb := cfg.PlasmaConfig.DAChallengeWindow + cfg.PlasmaConfig.DAResolveWindow + 1
		// in the case only if the plasma windows are longer than the default finality lookback
		if lkb > defaultFinalityLookback {
			return lkb
		}
	}
	return defaultFinalityLookback
}

type FinalityData struct {
	// The last L2 block that was fully derived and inserted into the L2 engine while processing this L1 block.
	L2Block eth.L2BlockRef
	// The L1 block this stage was at when inserting the L2 block.
	// When this L1 block is finalized, the L2 chain up to this block can be fully reproduced from finalized L1 data.
	L1Block eth.BlockID
}

type FinalizerEngine interface {
	Finalized() eth.L2BlockRef
	SetFinalizedHead(eth.L2BlockRef)
}

type FinalizerL1Interface interface {
	L1BlockRefByNumber(context.Context, uint64) (eth.L1BlockRef, error)
}

type Finalizer struct {
	mu sync.Mutex

	log log.Logger

	// finalizedL1 is the currently perceived finalized L1 block.
	// This may be ahead of the current traversed origin when syncing.
	finalizedL1 eth.L1BlockRef

	// triedFinalizeAt tracks at which L1 block number we last tried to finalize during sync.
	triedFinalizeAt uint64

	// Tracks which L2 blocks where last derived from which L1 block. At most finalityLookback large.
	finalityData []FinalityData

	// Maximum amount of L2 blocks to store in finalityData.
	finalityLookback uint64

	l1Fetcher FinalizerL1Interface

	ec FinalizerEngine
}

func NewFinalizer(log log.Logger, cfg *rollup.Config, l1Fetcher FinalizerL1Interface, ec FinalizerEngine) *Finalizer {
	lookback := calcFinalityLookback(cfg)
	return &Finalizer{
		log:              log,
		finalizedL1:      eth.L1BlockRef{},
		triedFinalizeAt:  0,
		finalityData:     make([]FinalityData, 0, lookback),
		finalityLookback: lookback,
		l1Fetcher:        l1Fetcher,
		ec:               ec,
	}
}

// FinalizedL1 identifies the L1 chain (incl.) that included and/or produced all the finalized L2 blocks.
// This may return a zeroed ID if no finalization signals have been seen yet.
func (fi *Finalizer) FinalizedL1() (out eth.L1BlockRef) {
	fi.mu.Lock()
	defer fi.mu.Unlock()
	out = fi.finalizedL1
	return
}

// Finalize applies a L1 finality signal, without any fork-choice or L2 state changes.
func (fi *Finalizer) Finalize(ctx context.Context, l1Origin eth.L1BlockRef) {
	fi.mu.Lock()
	defer fi.mu.Unlock()
	prevFinalizedL1 := fi.finalizedL1
	if l1Origin.Number < fi.finalizedL1.Number {
		fi.log.Error("ignoring old L1 finalized block signal! Is the L1 provider corrupted?",
			"prev_finalized_l1", prevFinalizedL1, "signaled_finalized_l1", l1Origin)
		return
	}

	if fi.finalizedL1 != l1Origin {
		// reset triedFinalizeAt, so we give finalization a shot with the new signal
		fi.triedFinalizeAt = 0

		// remember the L1 finalization signal
		fi.finalizedL1 = l1Origin
	}

	// remnant of finality in EngineQueue: the finalization work does not inherit a context from the caller.
	if err := fi.tryFinalize(ctx); err != nil {
		fi.log.Warn("received L1 finalization signal, but was unable to determine and apply L2 finality", "err", err)
	}
}

// OnDerivationL1End is called when a L1 block has been fully exhausted (i.e. no more L2 blocks to derive from).
//
// Since finality applies to all L2 blocks fully derived from the same block,
// it optimal to only check after the derivation from the L1 block has been exhausted.
//
// This will look at what has been buffered so far,
// sanity-check we are on the finalizing L1 chain,
// and finalize any L2 blocks that were fully derived from known finalized L1 blocks.
func (fi *Finalizer) OnDerivationL1End(ctx context.Context, derivedFrom eth.L1BlockRef) error {
	fi.mu.Lock()
	defer fi.mu.Unlock()
	if fi.finalizedL1 == (eth.L1BlockRef{}) {
		return nil // if no L1 information is finalized yet, then skip this
	}
	// If we recently tried finalizing, then don't try again just yet, but traverse more of L1 first.
	if fi.triedFinalizeAt != 0 && derivedFrom.Number <= fi.triedFinalizeAt+finalityDelay {
		return nil
	}
	fi.log.Info("processing L1 finality information", "l1_finalized", fi.finalizedL1, "derived_from", derivedFrom, "previous", fi.triedFinalizeAt)
	fi.triedFinalizeAt = derivedFrom.Number
	return fi.tryFinalize(ctx)
}

func (fi *Finalizer) tryFinalize(ctx context.Context) error {
	// default to keep the same finalized block
	finalizedL2 := fi.ec.Finalized()
	var finalizedDerivedFrom eth.BlockID
	// go through the latest inclusion data, and find the last L2 block that was derived from a finalized L1 block
	for _, fd := range fi.finalityData {
		if fd.L2Block.Number > finalizedL2.Number && fd.L1Block.Number <= fi.finalizedL1.Number {
			finalizedL2 = fd.L2Block
			finalizedDerivedFrom = fd.L1Block
			// keep iterating, there may be later L2 blocks that can also be finalized
		}
	}
	if finalizedDerivedFrom != (eth.BlockID{}) {
		// Sanity check the finality signal of L1.
		// Even though the signal is trusted and we do the below check also,
		// the signal itself has to be canonical to proceed.
		// TODO(#10724): This check could be removed if the finality signal is fully trusted, and if tests were more flexible for this case.
		signalRef, err := fi.l1Fetcher.L1BlockRefByNumber(ctx, fi.finalizedL1.Number)
		if err != nil {
			return derive.NewTemporaryError(fmt.Errorf("failed to check if on finalizing L1 chain, could not fetch block %d: %w", fi.finalizedL1.Number, err))
		}
		if signalRef.Hash != fi.finalizedL1.Hash {
			return derive.NewResetError(fmt.Errorf("need to reset, we assumed %s is finalized, but canonical chain is %s", fi.finalizedL1, signalRef))
		}

		// Sanity check we are indeed on the finalizing chain, and not stuck on something else.
		// We assume that the block-by-number query is consistent with the previously received finalized chain signal
		derivedRef, err := fi.l1Fetcher.L1BlockRefByNumber(ctx, finalizedDerivedFrom.Number)
		if err != nil {
			return derive.NewTemporaryError(fmt.Errorf("failed to check if on finalizing L1 chain, could not fetch block %d: %w", finalizedDerivedFrom.Number, err))
		}
		if derivedRef.Hash != finalizedDerivedFrom.Hash {
			return derive.NewResetError(fmt.Errorf("need to reset, we are on %s, not on the finalizing L1 chain %s (towards %s)",
				finalizedDerivedFrom, derivedRef, fi.finalizedL1))
		}

		fi.ec.SetFinalizedHead(finalizedL2)
	}
	return nil
}

// PostProcessSafeL2 buffers the L1 block the safe head was fully derived from,
// to finalize it once the derived-from L1 block, or a later L1 block, finalizes.
func (fi *Finalizer) PostProcessSafeL2(l2Safe eth.L2BlockRef, derivedFrom eth.L1BlockRef) {
	fi.mu.Lock()
	defer fi.mu.Unlock()
	// remember the last L2 block that we fully derived from the given finality data
	if len(fi.finalityData) == 0 || fi.finalityData[len(fi.finalityData)-1].L1Block.Number < derivedFrom.Number {
		// prune finality data if necessary, before appending any data.
		if uint64(len(fi.finalityData)) >= fi.finalityLookback {
			fi.finalityData = append(fi.finalityData[:0], fi.finalityData[1:fi.finalityLookback]...)
		}
		// append entry for new L1 block
		fi.finalityData = append(fi.finalityData, FinalityData{
			L2Block: l2Safe,
			L1Block: derivedFrom.ID(),
		})
		last := &fi.finalityData[len(fi.finalityData)-1]
		fi.log.Debug("extended finality-data", "last_l1", last.L1Block, "last_l2", last.L2Block)
	} else {
		// if it's a new L2 block that was derived from the same latest L1 block, then just update the entry
		last := &fi.finalityData[len(fi.finalityData)-1]
		if last.L2Block != l2Safe { // avoid logging if there are no changes
			last.L2Block = l2Safe
			fi.log.Debug("updated finality-data", "last_l1", last.L1Block, "last_l2", last.L2Block)
		}
	}
}

// Reset clears the recent history of safe-L2 blocks used for finalization,
// to avoid finalizing any reorged-out L2 blocks.
func (fi *Finalizer) Reset() {
	fi.mu.Lock()
	defer fi.mu.Unlock()
	fi.finalityData = fi.finalityData[:0]
	fi.triedFinalizeAt = 0
	// no need to reset finalizedL1, it's finalized after all
}
