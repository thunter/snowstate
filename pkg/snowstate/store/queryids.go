package store

import "sync"

// QueryIDs collects Snowflake query IDs for one request.
type QueryIDs struct {
	mu  sync.Mutex
	ids []string
}

// Add records a query ID.
func (q *QueryIDs) Add(id string) {
	if q == nil || id == "" {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.ids = append(q.ids, id)
}

// List returns the IDs collected so far.
func (q *QueryIDs) List() []string {
	if q == nil {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]string(nil), q.ids...)
}
