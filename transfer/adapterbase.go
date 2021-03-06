package transfer

import (
	"fmt"
	"sync"
	"time"

	"github.com/github/git-lfs/errutil"
	"github.com/rubyist/tracerx"
)

const (
	// objectExpirationGracePeriod is the grace period applied to objects
	// when checking whether or not they have expired.
	objectExpirationGracePeriod = 5 * time.Second
)

// adapterBase implements the common functionality for core adapters which
// process transfers with N workers handling an oid each, and which wait for
// authentication to succeed on one worker before proceeding
type adapterBase struct {
	name         string
	direction    Direction
	transferImpl transferImplementation
	jobChan      chan *Transfer
	cb           TransferProgressCallback
	outChan      chan TransferResult
	// WaitGroup to sync the completion of all workers
	workerWait sync.WaitGroup
	// WaitGroup to serialise the first transfer response to perform login if needed
	authWait sync.WaitGroup
}

// transferImplementation must be implemented to provide the actual upload/download
// implementation for all core transfer approaches that use adapterBase for
// convenience. This function will be called on multiple goroutines so it
// must be either stateless or thread safe. However it will never be called
// for the same oid in parallel.
// If authOkFunc is not nil, implementations must call it as early as possible
// when authentication succeeded, before the whole file content is transferred
type transferImplementation interface {
	DoTransfer(t *Transfer, cb TransferProgressCallback, authOkFunc func()) error
}

func newAdapterBase(name string, dir Direction, ti transferImplementation) *adapterBase {
	return &adapterBase{name: name, direction: dir, transferImpl: ti}
}

func (a *adapterBase) Name() string {
	return a.name
}

func (a *adapterBase) Direction() Direction {
	return a.direction
}

func (a *adapterBase) Begin(maxConcurrency int, cb TransferProgressCallback, completion chan TransferResult) error {
	a.cb = cb
	a.outChan = completion
	a.jobChan = make(chan *Transfer, 100)

	tracerx.Printf("xfer: adapter %q Begin() with %d workers", a.Name(), maxConcurrency)

	a.workerWait.Add(maxConcurrency)
	a.authWait.Add(1)
	for i := 0; i < maxConcurrency; i++ {
		go a.worker(i)
	}
	tracerx.Printf("xfer: adapter %q started", a.Name())
	return nil
}

func (a *adapterBase) Add(t *Transfer) {
	tracerx.Printf("xfer: adapter %q Add() for %q", a.Name(), t.Object.Oid)
	a.jobChan <- t
}

func (a *adapterBase) End() {
	tracerx.Printf("xfer: adapter %q End()", a.Name())
	close(a.jobChan)
	// wait for all transfers to complete
	a.workerWait.Wait()
	if a.outChan != nil {
		close(a.outChan)
	}
	tracerx.Printf("xfer: adapter %q stopped", a.Name())
}

// worker function, many of these run per adapter
func (a *adapterBase) worker(workerNum int) {

	tracerx.Printf("xfer: adapter %q worker %d starting", a.Name(), workerNum)
	waitForAuth := workerNum > 0
	signalAuthOnResponse := workerNum == 0

	// First worker is the only one allowed to start immediately
	// The rest wait until successful response from 1st worker to
	// make sure only 1 login prompt is presented if necessary
	// Deliberately outside jobChan processing so we know worker 0 will process 1st item
	if waitForAuth {
		tracerx.Printf("xfer: adapter %q worker %d waiting for Auth", a.Name(), workerNum)
		a.authWait.Wait()
		tracerx.Printf("xfer: adapter %q worker %d auth signal received", a.Name(), workerNum)
	}

	for t := range a.jobChan {
		var authCallback func()
		if signalAuthOnResponse {
			authCallback = func() {
				a.authWait.Done()
				signalAuthOnResponse = false
			}
		}
		tracerx.Printf("xfer: adapter %q worker %d processing job for %q", a.Name(), workerNum, t.Object.Oid)

		// Actual transfer happens here
		var err error
		if t.Object.IsExpired(time.Now().Add(objectExpirationGracePeriod)) {
			tracerx.Printf("xfer: adapter %q worker %d found job for %q expired, retrying...", a.Name(), workerNum, t.Object.Oid)
			err = errutil.NewRetriableError(fmt.Errorf("lfs/transfer: object %q has expired", t.Object.Oid))
		} else if t.Object.Size < 0 {
			tracerx.Printf("xfer: adapter %q worker %d found invalid size for %q (got: %d), retrying...", a.Name(), workerNum, t.Object.Oid, t.Object.Size)
			err = fmt.Errorf("Git LFS: object %q has invalid size (got: %d)", t.Object.Oid, t.Object.Size)
		} else {
			err = a.transferImpl.DoTransfer(t, a.cb, authCallback)
		}

		if a.outChan != nil {
			res := TransferResult{t, err}
			a.outChan <- res
		}

		tracerx.Printf("xfer: adapter %q worker %d finished job for %q", a.Name(), workerNum, t.Object.Oid)
	}
	// This will only happen if no jobs were submitted; just wake up all workers to finish
	if signalAuthOnResponse {
		a.authWait.Done()
	}
	tracerx.Printf("xfer: adapter %q worker %d stopping", a.Name(), workerNum)
	a.workerWait.Done()
}

func advanceCallbackProgress(cb TransferProgressCallback, t *Transfer, numBytes int64) {
	if cb != nil {
		// Must split into max int sizes since read count is int
		const maxInt = int(^uint(0) >> 1)
		for read := int64(0); read < numBytes; {
			remainder := numBytes - read
			if remainder > int64(maxInt) {
				read += int64(maxInt)
				cb(t.Name, t.Object.Size, read, maxInt)
			} else {
				read += remainder
				cb(t.Name, t.Object.Size, read, int(remainder))
			}

		}
	}
}
