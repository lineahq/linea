package memory

import (
	"context"

	"github.com/lineahq/linea/internal/debugger/artifact"
)

// maxRetainedChanges bounds the change feed; a cursor older than the oldest
// retained change is expired.
const maxRetainedChanges = 10000

// feed is the change feed and the live events of the stores open on one
// Data. Every change commits under d.mu, so a change's ID is also its
// transaction and a reader never misses one in progress.
type feed struct {
	changes []artifact.Change
	next    int64
	// trimmed is the newest change dropped from changes.
	trimmed int64
	hints   []chan struct{}
	events  map[string][]artifact.EventBatch
}

// change records a change and hints every open store; the caller holds d.mu.
func (d *Data) change(kind artifact.ChangeKind, id string) {
	f := &d.feed
	f.next++
	f.changes = append(f.changes, artifact.Change{Kind: kind, ID: id, Cursor: artifact.ChangeCursor{XID: uint64(f.next), ID: f.next}})
	if over := len(f.changes) - maxRetainedChanges; over > 0 {
		f.trimmed = f.changes[over-1].Cursor.ID
		f.changes = append([]artifact.Change(nil), f.changes[over:]...)
	}
	for _, h := range f.hints {
		select {
		case h <- struct{}{}:
		default:
		}
	}
}

// ChangeHead returns the newest change's cursor.
func (s *Store) ChangeHead(ctx context.Context) (artifact.ChangeCursor, error) {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return artifact.ChangeCursor{}, err
	}
	return artifact.ChangeCursor{XID: uint64(d.feed.next), ID: d.feed.next}, nil
}

// Changes returns up to limit changes after cursor.
func (s *Store) Changes(ctx context.Context, after artifact.ChangeCursor, limit int) ([]artifact.Change, artifact.ChangeCursor, error) {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return nil, after, err
	}
	if limit <= 0 || limit > artifact.MaxChanges {
		return nil, after, invalid("invalid change limit")
	}
	if after.ID < d.feed.trimmed {
		return nil, after, artifact.ErrExpired
	}
	out := []artifact.Change{}
	for _, c := range d.feed.changes {
		if c.Cursor.ID > after.ID {
			if len(out) == limit {
				break
			}
			out = append(out, c)
			after = c.Cursor
		}
	}
	return out, after, nil
}

// ChangeHints returns this store's hint channel.
func (s *Store) ChangeHints() <-chan struct{} {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if s.hints == nil {
		s.hints = make(chan struct{}, 1)
		d.feed.hints = append(d.feed.hints, s.hints)
	}
	return s.hints
}

// Run returns one run's record.
func (s *Store) Run(ctx context.Context, id string) (artifact.RunRecord, error) {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return artifact.RunRecord{}, err
	}
	if d.runs == nil {
		return artifact.RunRecord{}, artifact.ErrNotFound
	}
	r, ok := d.runs.Records[id]
	if !ok {
		return artifact.RunRecord{}, artifact.ErrNotFound
	}
	r = cloneRun(r)
	return r, nil
}

// AppendRunEvents keeps a batch of an unfinished run's live events.
func (s *Store) AppendRunEvents(ctx context.Context, id string, b artifact.EventBatch) error {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return err
	}
	if b.Attempt <= 0 || b.First <= 0 || b.Last < b.First || len(b.Events) == 0 {
		return invalid("invalid event batch")
	}
	if d.runs == nil {
		return artifact.ErrNotFound
	}
	r, ok := d.runs.Records[id]
	if !ok {
		return artifact.ErrNotFound
	}
	if r.Terminal {
		return artifact.ErrConflict
	}
	if d.feed.events == nil {
		d.feed.events = map[string][]artifact.EventBatch{}
	}
	var kept []artifact.EventBatch
	var size int64
	for _, old := range d.feed.events[id] {
		if old.Attempt > b.Attempt {
			return artifact.ErrConflict
		}
		if old.Attempt == b.Attempt {
			if old.Last >= b.First {
				return artifact.ErrConflict
			}
			kept = append(kept, old)
			size += int64(len(old.Events))
		}
	}
	if size+int64(len(b.Events)) > artifact.MaxRunEventBytes {
		return artifact.ErrLimit
	}
	b.Events = cloneBytes(b.Events)
	d.feed.events[id] = append(kept, b)
	d.change(artifact.ChangeEvents, id)
	return nil
}

// RunEvents returns the latest attempt's batches that end after after.
func (s *Store) RunEvents(ctx context.Context, id string, after int) ([]artifact.EventBatch, error) {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return nil, err
	}
	out := []artifact.EventBatch{}
	for _, b := range d.feed.events[id] {
		if b.Last > after {
			b.Events = cloneBytes(b.Events)
			out = append(out, b)
		}
	}
	return out, nil
}
