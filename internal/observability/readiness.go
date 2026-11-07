package observability

import "sync/atomic"

type Readiness struct {
	ready    atomic.Bool
	draining atomic.Bool
}

func NewReadiness() *Readiness {
	return &Readiness{}
}

func (r *Readiness) Ready() bool {
	return r.ready.Load() && !r.draining.Load()
}

func (r *Readiness) SetReady(ready bool) {
	r.ready.Store(ready)
}

func (r *Readiness) Draining() bool {
	return r.draining.Load()
}

func (r *Readiness) SetDraining(draining bool) {
	r.draining.Store(draining)
}
