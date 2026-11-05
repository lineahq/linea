package memory

import (
	"bytes"
	"context"
	"slices"
	"time"

	"github.com/lineahq/linea/internal/debugger/artifact"
)

// jobQueue holds interactive jobs in posting order.
type jobQueue struct {
	byID  map[string]*artifact.JobRecord
	order []string
}

func cloneJob(j *artifact.JobRecord) artifact.JobRecord {
	c := *j
	c.Input, c.Result, c.Error = bytes.Clone(j.Input), bytes.Clone(j.Result), bytes.Clone(j.Error)
	return c
}

func (q *jobQueue) remove(id string) {
	delete(q.byID, id)
	q.order = slices.DeleteFunc(q.order, func(o string) bool { return o == id })
}

// PostJob queues a job.
func (s *Store) PostJob(ctx context.Context, j artifact.JobRecord) error {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return err
	}
	if err := artifact.ValidateJob(j); err != nil {
		return err
	}
	if d.jobs.byID == nil {
		d.jobs.byID = map[string]*artifact.JobRecord{}
	}
	if d.jobs.byID[j.ID] != nil {
		return artifact.ErrConflict
	}
	stored := artifact.JobRecord{ID: j.ID, Runtime: j.Runtime, Kind: j.Kind, Input: bytes.Clone(j.Input), State: artifact.JobQueued, Created: time.Now().UTC()}
	d.jobs.byID[j.ID] = &stored
	d.jobs.order = append(d.jobs.order, j.ID)
	d.change(artifact.ChangeJob, j.ID)
	return nil
}

// ClaimJobs claims the oldest queued jobs of runtime.
func (s *Store) ClaimJobs(ctx context.Context, worker, runtime string, n int) ([]artifact.JobRecord, error) {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return nil, err
	}
	if worker == "" || n <= 0 {
		return nil, invalid("a claim needs a worker and a positive count")
	}
	out := []artifact.JobRecord{}
	for _, id := range d.jobs.order {
		if len(out) == n {
			break
		}
		j := d.jobs.byID[id]
		if j.State == artifact.JobQueued && j.Runtime == runtime {
			j.State, j.Worker = artifact.JobClaimed, worker
			out = append(out, cloneJob(j))
		}
	}
	return out, nil
}

// FinishJob records a claimed job's answer.
func (s *Store) FinishJob(ctx context.Context, id, worker string, result, failure []byte) error {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return err
	}
	if err := artifact.ValidateJobAnswer(result, failure); err != nil {
		return err
	}
	j := d.jobs.byID[id]
	if j == nil {
		return artifact.ErrNotFound
	}
	if j.State != artifact.JobClaimed || j.Worker != worker {
		return artifact.ErrConflict
	}
	j.State, j.Result, j.Error = artifact.JobDone, bytes.Clone(result), bytes.Clone(failure)
	d.change(artifact.ChangeJob, id)
	return nil
}

// Job returns a job.
func (s *Store) Job(ctx context.Context, id string) (artifact.JobRecord, error) {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return artifact.JobRecord{}, err
	}
	j := d.jobs.byID[id]
	if j == nil {
		return artifact.JobRecord{}, artifact.ErrNotFound
	}
	return cloneJob(j), nil
}

// WithdrawJob deletes an unclaimed job.
func (s *Store) WithdrawJob(ctx context.Context, id string) error {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return err
	}
	j := d.jobs.byID[id]
	if j == nil {
		return artifact.ErrNotFound
	}
	if j.State != artifact.JobQueued {
		return artifact.ErrConflict
	}
	d.jobs.remove(id)
	return nil
}

// DeleteJob deletes a job.
func (s *Store) DeleteJob(ctx context.Context, id string) error {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return err
	}
	d.jobs.remove(id)
	return nil
}

// TrimJobs deletes jobs posted before before.
func (s *Store) TrimJobs(ctx context.Context, before time.Time) error {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return err
	}
	for _, id := range slices.Clone(d.jobs.order) {
		if d.jobs.byID[id].Created.Before(before) {
			d.jobs.remove(id)
		}
	}
	return nil
}
