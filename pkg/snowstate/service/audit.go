package service

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/thunter/snowstate/pkg/snowstate/store"
)

const (
	auditFlushInterval = 2 * time.Second
	auditFlushSize     = 100
	// auditMaxBuffered bounds memory during a long outage. Rows that go
	// through here are advisory (reads, denials); changes audit transactionally.
	auditMaxBuffered  = 10000
	auditFlushTimeout = 30 * time.Second
)

// auditBuffer batches audit rows that need not share a transaction.
type auditBuffer struct {
	st   store.Store
	log  *slog.Logger
	done chan struct{}
	wg   sync.WaitGroup

	mu      sync.Mutex
	pending []store.AuditEvent
	kick    chan struct{}
	// flushMu keeps one flush running at a time.
	flushMu sync.Mutex
}

func newAuditBuffer(st store.Store, log *slog.Logger) *auditBuffer {
	b := &auditBuffer{st: st, log: log, done: make(chan struct{}), kick: make(chan struct{}, 1)}
	b.wg.Add(1)
	go b.run()
	return b
}

func (b *auditBuffer) run() {
	defer b.wg.Done()
	t := time.NewTicker(auditFlushInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
		case <-b.kick:
		case <-b.done:
			return
		}
		b.flush()
	}
}

func (b *auditBuffer) add(e store.AuditEvent) {
	b.mu.Lock()
	if len(b.pending) >= auditMaxBuffered {
		b.pending = b.pending[1:]
		b.log.Warn("audit buffer full, dropping oldest row")
	}
	b.pending = append(b.pending, e)
	full := len(b.pending) >= auditFlushSize
	b.mu.Unlock()
	if full {
		select {
		case b.kick <- struct{}{}:
		default:
		}
	}
}

func (b *auditBuffer) depth() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.pending)
}

func (b *auditBuffer) flush() {
	b.flushMu.Lock()
	defer b.flushMu.Unlock()

	b.mu.Lock()
	batch := b.pending
	b.pending = nil
	b.mu.Unlock()
	if len(batch) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), auditFlushTimeout)
	defer cancel()
	if err := b.st.AppendAudit(ctx, batch); err != nil {
		b.log.Warn("audit flush failed, will retry", "rows", len(batch), "error", err)
		b.mu.Lock()
		b.pending = append(batch, b.pending...)
		if over := len(b.pending) - auditMaxBuffered; over > 0 {
			b.pending = b.pending[over:]
		}
		b.mu.Unlock()
	}
}

func (b *auditBuffer) close() {
	close(b.done)
	b.wg.Wait()
	b.flush()
}
