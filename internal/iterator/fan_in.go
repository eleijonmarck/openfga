package iterator

import (
	"context"
	"sync"

	"github.com/sourcegraph/conc/pool"

	"github.com/openfga/openfga/internal/concurrency"
)

type FanIn struct {
	ctx       context.Context
	cancel    context.CancelFunc
	count     int
	out       chan *Msg
	addCh     chan (chan *Msg)
	mu        sync.Mutex
	accepting bool
	pool      *pool.ContextPool
	drained   chan bool
}

func NewFanIn(ctx context.Context, limit int) *FanIn {
	ctx, cancel := context.WithCancel(ctx)

	f := &FanIn{
		ctx:       ctx,
		cancel:    cancel,
		count:     0,
		out:       make(chan *Msg, limit),
		addCh:     make(chan (chan *Msg), limit),
		accepting: true,
		mu:        sync.Mutex{},
		pool:      concurrency.NewPool(ctx, limit),
		drained:   make(chan bool, 1),
	}

	go f.run()
	return f
}

func (f *FanIn) run() {
	defer func() {
		f.Done()
		_ = f.pool.Wait()
		close(f.out)
		for ch := range f.addCh {
			drainOnExit(ch)
		}
		f.drained <- true
		close(f.drained)
	}()
	for {
		select {
		case <-f.ctx.Done():
			return
		case ch, ok := <-f.addCh:
			if !ok {
				return
			}
			f.pool.Go(func(ctx context.Context) error {
				defer drainOnExit(ch)
				for {
					select {
					case <-ctx.Done():
						return nil
					case v, ok := <-ch:
						if !ok {
							return nil
						}
						if !concurrency.TrySendThroughChannel(ctx, v, f.out) {
							if v.Iter != nil {
								v.Iter.Stop()
							}
						}
					}
				}
			})
		}
	}
}

func drainOnExit(ch chan *Msg) {
	for msg := range ch {
		if msg.Iter != nil {
			msg.Iter.Stop()
		}
	}
}

func (f *FanIn) Add(ch chan *Msg) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.count++
	if !f.accepting {
		drainOnExit(ch)
		return
	}
	if !concurrency.TrySendThroughChannel(f.ctx, ch, f.addCh) {
		drainOnExit(ch)
		return
	}
}

func (f *FanIn) Out() chan *Msg {
	return f.out
}

func (f *FanIn) Count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.count
}

func (f *FanIn) Done() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.accepting {
		f.accepting = false
		close(f.addCh)
	}
}

func (f *FanIn) Close() {
	// Done gets called internally
	f.cancel()
	drainOnExit(f.out)
}
