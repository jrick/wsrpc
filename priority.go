package wsrpc

import "sync"

type priorityMutex struct {
	hi, lo, data sync.Mutex
}

func (mu *priorityMutex) LockHi() {
	mu.hi.Lock()
	mu.data.Lock()
	mu.hi.Unlock()
}

func (mu *priorityMutex) UnlockHi() {
	mu.data.Unlock()
}

func (mu *priorityMutex) LockLo() {
	mu.lo.Lock()
	mu.hi.Lock()
	mu.data.Lock()
	mu.hi.Unlock()
}

func (mu *priorityMutex) UnlockLo() {
	mu.data.Unlock()
	mu.lo.Unlock()
}
