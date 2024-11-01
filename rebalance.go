package main

import (
	"sync"

	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
)

type PendingRebalanceAttempts struct {
	sync.RWMutex
	rebalanceAttempts map[string]rebalanceAttempt
}

type rebalanceAttempt struct {
	// identifier for the intercepted htlc
	circuitKey *routerrpc.CircuitKey

	id                 string
	amount             uint64
	publicKey          []byte
	mints              []string
	ecashAmount        uint64
	invoice            string
	ecashHTLC          string
	rebalanceSucceeded bool
}

func NewPendingRebalanceAttempts() *PendingRebalanceAttempts {
	return &PendingRebalanceAttempts{
		rebalanceAttempts: make(map[string]rebalanceAttempt),
	}
}

func (p *PendingRebalanceAttempts) Get(id string) (rebalanceAttempt, bool) {
	p.RLock()
	defer p.RUnlock()
	v, ok := p.rebalanceAttempts[id]
	return v, ok
}

func (p *PendingRebalanceAttempts) Put(id string, rebalance rebalanceAttempt) {
	p.Lock()
	defer p.Unlock()
	p.rebalanceAttempts[id] = rebalance
}

func (r *PendingRebalanceAttempts) Delete(id string) {
	r.Lock()
	defer r.Unlock()
	delete(r.rebalanceAttempts, id)
}
