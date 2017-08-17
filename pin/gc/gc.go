package gc

import (
	"context"
	"errors"
	"fmt"

	bstore "github.com/ipfs/go-ipfs/blocks/blockstore"
	dag "github.com/ipfs/go-ipfs/merkledag"
	pin "github.com/ipfs/go-ipfs/pin"

	logging "gx/ipfs/QmSpJByNKFX1sCsHBEp3R73FL4NF6FnQTEGyNAXHm2GS52/go-log"
	cid "gx/ipfs/QmTprEaAA2A9bst5XH7exuyi5KzNMK3SEDNN8rBDnKWcUS/go-cid"
	node "gx/ipfs/QmYNyRZJBUYPNrLszFmrBrPJbsBh2vMsefz5gnDpB5M1P6/go-ipld-format"
)

var log = logging.Logger("gc")

// Result represents an incremental output from a garbage collection
// run.  It contains either an error, or the cid of a removed object.
type Result struct {
	KeyRemoved *cid.Cid
	Error      error
}

func getGCLock(ctx context.Context, bs bstore.GCBlockstore) (bstore.Unlocker, *logging.EventInProgress) {
	elock := log.EventBegin(ctx, "GC.lockWait")
	unlocker := bs.GCLock()
	elock.Done()
	elock = log.EventBegin(ctx, "GC.locked")
	return unlocker, elock
}

func addRoots(tri *triset, pn pin.Pinner, bestEffortRoots []*cid.Cid) {
	for _, v := range bestEffortRoots {
		tri.InsertGray(v, false)
	}

	for _, v := range pn.RecursiveKeys() {
		tri.InsertGray(v, true)
	}

}

// GC performs a mark and sweep garbage collection of the blocks in the blockstore
// first, it creates a 'marked' set and adds to it the following:
// - all recursively pinned blocks, plus all of their descendants (recursively)
// - bestEffortRoots, plus all of its descendants (recursively)
// - all directly pinned blocks
// - all blocks utilized internally by the pinner
//
// The routine then iterates over every block in the blockstore and
// deletes any block that is not found in the marked set.
//
func GC(ctx context.Context, bs bstore.GCBlockstore, ls dag.LinkService, pn pin.Pinner, bestEffortRoots []*cid.Cid) <-chan Result {

	ls = ls.GetOfflineLinkService()
	output := make(chan Result, 128)
	tri := newTriset()

	unlocker, elock := getGCLock(ctx, bs)
	emark := log.EventBegin(ctx, "GC.mark")

	err := pn.Flush()
	if err != nil {
		output <- Result{Error: err}
		close(output)
		return output
	}
	addRoots(tri, pn, bestEffortRoots)

	unlocker.Unlock()
	elock.Done()

	go func() {
		defer close(output)

		bestEffortGetLinks := func(ctx context.Context, cid *cid.Cid) ([]*node.Link, error) {
			links, err := ls.GetLinks(ctx, cid)
			if err != nil && err != dag.ErrNotFound {
				return nil, &CannotFetchLinksError{cid, err}
			}
			return links, nil
		}

		getLinks := func(ctx context.Context, cid *cid.Cid) ([]*node.Link, error) {
			links, err := ls.GetLinks(ctx, cid)
			if err != nil {
				return nil, &CannotFetchLinksError{cid, err}
			}
			return links, nil
		}

		var criticalError error
		defer func() {
			if criticalError != nil {
				output <- Result{Error: criticalError}
			}
		}()

		// Enumerate without the lock
		for {
			finished, err := tri.EnumerateStep(ctx, getLinks, bestEffortGetLinks)
			if err != nil {
				output <- Result{Error: err}
				criticalError = ErrCannotFetchAllLinks
				return
			}
			if finished {
				break
			}
		}

		// Add white objects
		keychan, err := bs.AllKeysChan(ctx)
		if err != nil {
			output <- Result{Error: err}
			return
		}

	loop:
		for {
			select {
			case c, ok := <-keychan:
				if !ok {
					break loop
				}
				tri.InsertFresh(c)
			case <-ctx.Done():
				output <- Result{Error: ctx.Err()}
				return
			}
		}

		// Regain lock
		unlocker, elock := getGCLock(ctx, bs)

		defer unlocker.Unlock()
		defer elock.Done()

		err = pn.Flush()
		if err != nil {
			output <- Result{Error: err}
			return
		}
		// Add the roots again, they might have changed
		addRoots(tri, pn, bestEffortRoots)

		// This prevents incremental and concurrent GCing
		for _, v := range pn.DirectKeys() {
			tri.blacken(v, enumStrict)
		}
		for _, v := range pn.InternalPins() {
			tri.InsertGray(v, true)
		}

		// Reenumerate, fast as most will be duplicate
		for {
			finished, err := tri.EnumerateStep(ctx, getLinks, bestEffortGetLinks)
			if err != nil {
				output <- Result{Error: err}
				criticalError = ErrCannotFetchAllLinks
				return
			}
			if finished {
				break
			}
		}

		emark.Done()
		esweep := log.EventBegin(ctx, "GC.sweep")

		var whiteSetSize, blackSetSize uint64

	loop2:
		for v, e := range tri.colmap {
			if e.getColor() != tri.white {
				blackSetSize++
				continue
			}
			whiteSetSize++

			c, err := cid.Cast([]byte(v))
			if err != nil {
				// this should not happen
				panic("error in cast of cid, inpossibru " + err.Error())
			}

			err = bs.DeleteBlock(c)
			if err != nil {
				output <- Result{Error: &CannotDeleteBlockError{c, err}}
				criticalError = ErrCannotDeleteSomeBlocks
				continue
			}
			select {
			case output <- Result{KeyRemoved: c}:
			case <-ctx.Done():
				break loop2
			}
		}

		esweep.Append(logging.LoggableMap{
			"whiteSetSize": fmt.Sprintf("%d", whiteSetSize),
			"blackSetSize": fmt.Sprintf("%d", blackSetSize),
		})
		esweep.Done()
	}()
	return output
}

var ErrCannotFetchAllLinks = errors.New("garbage collection aborted: could not retrieve some links")

var ErrCannotDeleteSomeBlocks = errors.New("garbage collection incomplete: could not delete some blocks")

type CannotFetchLinksError struct {
	Key *cid.Cid
	Err error
}

func (e *CannotFetchLinksError) Error() string {
	return fmt.Sprintf("could not retrieve links for %s: %s", e.Key, e.Err)
}

type CannotDeleteBlockError struct {
	Key *cid.Cid
	Err error
}

func (e *CannotDeleteBlockError) Error() string {
	return fmt.Sprintf("could not remove %s: %s", e.Key, e.Err)
}
