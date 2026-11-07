package observability

import "sync/atomic"

type Readiness struct {
	ready atomic.Bool
}

func NewReadiness() *Readiness {
	return &Readiness{}
}

func (r *Readiness) Ready() bool {
	return r.ready.Load()
}

func (r *Readiness) SetReady(ready bool) {
	r.ready.Store(ready)
}
