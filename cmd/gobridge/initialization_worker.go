package main

import (
	"context"
	"time"
)

type initializationRequest struct {
	ctx    context.Context
	cancel context.CancelFunc
}

// initializationWorker owns one goroutine for its entire startup lifetime.
// The command loop owns pending: deadline/cancellation alone never clears it,
// since an uninterruptible filesystem operation may still be outstanding.
type initializationWorker struct {
	ctx           context.Context
	stop          context.CancelFunc
	requests      chan initializationRequest
	results       chan error
	pending       bool
	cancelAttempt context.CancelFunc
}

func newInitializationWorker(parent context.Context, initialize func(context.Context) error) *initializationWorker {
	ctx, cancel := context.WithCancel(parent)
	w := &initializationWorker{ctx: ctx, stop: cancel, requests: make(chan initializationRequest, 1), results: make(chan error, 1)}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case request := <-w.requests:
				err := request.ctx.Err()
				if err == nil {
					err = initialize(request.ctx)
				}
				request.cancel()
				select {
				case w.results <- err:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return w
}

func (w *initializationWorker) start() {
	if w.pending || w.ctx.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(w.ctx, 30*time.Second)
	w.pending, w.cancelAttempt = true, cancel
	w.requests <- initializationRequest{ctx: ctx, cancel: cancel}
}

func (w *initializationWorker) supersede() {
	if w.cancelAttempt != nil {
		w.cancelAttempt()
	}
}

func (w *initializationWorker) complete() {
	w.supersede()
	w.pending, w.cancelAttempt = false, nil
}
