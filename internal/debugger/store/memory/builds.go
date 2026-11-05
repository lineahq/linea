package memory

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/lineahq/linea/internal/debugger/artifact"
)

// buildRow is a build with what only the store keeps: its admission ordinal
// and the last lines of its log.
type buildRow struct {
	artifact.BuildRecord
	ordinal uint64
	log     []string
}

// buildQueue holds runtime builds by ID. Ordinals only grow, so a trimmed
// build leaves a gap and the order of the rest stays the admission order.
type buildQueue struct {
	byID map[string]*buildRow
	next uint64
}

func cloneBuild(r *buildRow) artifact.BuildRecord {
	c := r.BuildRecord
	c.Spec, c.Result, c.Inputs = bytes.Clone(r.Spec), bytes.Clone(r.Result), slices.Clone(r.Inputs)
	return c
}

// micro is the precision the PostgreSQL store keeps.
func micro(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return t.UTC().Truncate(time.Microsecond)
}

// sorted lists the builds that match, oldest admission first. d.mu is held.
func (q *buildQueue) sorted(match func(*buildRow) bool) []*buildRow {
	var out []*buildRow
	for _, r := range q.byID {
		if match(r) {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b *buildRow) int { return cmp.Compare(a.ordinal, b.ordinal) })
	return out
}

func (r *buildRow) holds(builder string, attempt int) bool {
	return r.State == artifact.BuildRunning && r.Builder == builder && r.Attempt == attempt
}

// end makes a held or queued build terminal.
func (r *buildRow) end(state artifact.BuildState, now time.Time) {
	r.State, r.Builder, r.Expires, r.Finished = state, "", time.Time{}, micro(now)
}

// AdmitBuild stores the inputs and queues the build, or records it done at
// once with the binary of the newest done build of the same key.
func (s *Store) AdmitBuild(ctx context.Context, a artifact.BuildAdmission, limits artifact.BuildLimits) (artifact.BuildRecord, error) {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return artifact.BuildRecord{}, err
	}
	if err := artifact.ValidateBuildAdmission(a); err != nil {
		return artifact.BuildRecord{}, err
	}
	if d.builds.byID[a.ID] != nil {
		return artifact.BuildRecord{}, artifact.ErrConflict
	}
	var reuse *buildRow
	for _, r := range d.builds.byID {
		if r.State == artifact.BuildDone && r.Key == a.Key && (reuse == nil || r.ordinal > reuse.ordinal) {
			reuse = r
		}
	}
	if reuse == nil {
		var principal, total int
		for _, r := range d.builds.byID {
			if !r.State.Terminal() {
				total++
				if r.Principal == a.Principal {
					principal++
				}
			}
		}
		if principal >= limits.PerPrincipal || total >= limits.Total {
			return artifact.BuildRecord{}, artifact.ErrLimit
		}
	}
	// The manifest comes first, then the sources by module.
	contents := [][]byte{a.Manifest}
	inputs := []artifact.BuildInput{{Digest: digest(a.Manifest), Bytes: int64(len(a.Manifest))}}
	for _, src := range slices.SortedFunc(slices.Values(a.Sources), func(x, y artifact.BuildSource) int { return cmp.Compare(x.Module, y.Module) }) {
		contents = append(contents, src.Archive)
		inputs = append(inputs, artifact.BuildInput{Module: src.Module, Digest: digest(src.Archive), Bytes: int64(len(src.Archive))})
	}
	newBlobs := map[string][]byte{}
	var additional int64
	for i, in := range inputs {
		if _, stored := d.blobs[in.Digest]; !stored {
			if _, pending := newBlobs[in.Digest]; !pending {
				additional += in.Bytes
				newBlobs[in.Digest] = contents[i]
			}
		}
	}
	used, err := s.usage()
	if err != nil {
		return artifact.BuildRecord{}, err
	}
	if err := s.checkSpace(used, additional, s.reserved(d.runs)); err != nil {
		return artifact.BuildRecord{}, err
	}
	for dg, content := range newBlobs {
		d.storeBlob(dg, content)
	}
	created := micro(a.Created)
	row := &buildRow{BuildRecord: artifact.BuildRecord{ID: a.ID, Principal: a.Principal, Name: a.Name, Key: a.Key, Spec: bytes.Clone(a.Spec),
		State: artifact.BuildQueued, Inputs: inputs, Created: created}}
	if reuse != nil {
		row.State, row.ReusedFrom = artifact.BuildDone, reuse.ID
		row.Result, row.BinaryDigest, row.BinaryBytes = bytes.Clone(reuse.Result), reuse.BinaryDigest, reuse.BinaryBytes
		row.Started, row.Finished = created, created
	}
	if d.builds.byID == nil {
		d.builds.byID = map[string]*buildRow{}
	}
	d.builds.next++
	row.ordinal = d.builds.next
	d.builds.byID[a.ID] = row
	d.change(artifact.ChangeBuild, a.ID)
	return cloneBuild(row), nil
}

// Build returns a build.
func (s *Store) Build(ctx context.Context, id string) (artifact.BuildRecord, error) {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return artifact.BuildRecord{}, err
	}
	r := d.builds.byID[id]
	if r == nil {
		return artifact.BuildRecord{}, artifact.ErrNotFound
	}
	return cloneBuild(r), nil
}

// BuildManifest returns the manifest a build was admitted with.
func (s *Store) BuildManifest(ctx context.Context, id string) ([]byte, error) {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return nil, err
	}
	r := d.builds.byID[id]
	if r == nil {
		return nil, artifact.ErrNotFound
	}
	in, ok := manifestInput(r.Inputs)
	if !ok {
		return nil, fmt.Errorf("%w: build %s has no manifest", artifact.ErrCorrupt, id)
	}
	raw, ok := d.blobs[in.Digest]
	if !ok {
		return nil, fmt.Errorf("%w: object unavailable", artifact.ErrCorrupt)
	}
	return bytes.Clone(raw), nil
}

// manifestInput is a build's manifest among its inputs: the one without a
// module.
func manifestInput(inputs []artifact.BuildInput) (artifact.BuildInput, bool) {
	for _, in := range inputs {
		if in.Module == "" {
			return in, true
		}
	}
	return artifact.BuildInput{}, false
}

// ListBuilds returns a page of builds, newest first, keyed on the admission
// ordinal as the PostgreSQL store does.
func (s *Store) ListBuilds(ctx context.Context, q artifact.BuildQuery) (*artifact.BuildPage, error) {
	q, before, err := q.Normalize()
	if err != nil {
		return nil, err
	}
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return nil, err
	}
	matched := d.builds.sorted(func(r *buildRow) bool {
		return (q.Principal == "" || r.Principal == q.Principal) && (before == 0 || r.ordinal < before)
	})
	slices.Reverse(matched)
	p := &artifact.BuildPage{Builds: []artifact.BuildRecord{}}
	for _, r := range matched[:min(len(matched), q.Limit)] {
		p.Builds = append(p.Builds, cloneBuild(r))
	}
	if len(matched) > q.Limit {
		p.NextCursor = q.NextCursor(matched[q.Limit-1].ordinal)
	}
	return p, nil
}

// BuildLog returns the kept lines from line from on.
func (s *Store) BuildLog(ctx context.Context, id string, from int) ([]string, int, error) {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return nil, 0, err
	}
	r := d.builds.byID[id]
	if r == nil {
		return nil, 0, artifact.ErrNotFound
	}
	from = max(from, 0)
	oldest := r.LogLines - len(r.log)
	first := max(from, oldest)
	if first >= r.LogLines {
		return []string{}, first, nil
	}
	return slices.Clone(r.log[first-oldest:]), first, nil
}

// ClaimBuilds leases up to n queued builds, oldest first.
func (s *Store) ClaimBuilds(ctx context.Context, builder string, n int, now, expires time.Time) ([]artifact.BuildRecord, error) {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return nil, err
	}
	if !validWorker(builder) || n < 0 || expires.IsZero() {
		return nil, invalid("invalid build claim")
	}
	out := []artifact.BuildRecord{}
	if n == 0 {
		return out, nil
	}
	for _, r := range d.builds.sorted(func(r *buildRow) bool { return r.State == artifact.BuildQueued }) {
		if len(out) == n {
			break
		}
		r.State, r.Builder, r.Attempt, r.Expires, r.Started = artifact.BuildRunning, builder, r.Attempt+1, micro(expires), micro(now)
		d.change(artifact.ChangeBuild, r.ID)
		out = append(out, cloneBuild(r))
	}
	return out, nil
}

// RenewBuilds extends the builder's leases. A renewal is no change: only a
// cancellation request is, and that is recorded when it is made.
func (s *Store) RenewBuilds(ctx context.Context, builder string, expires time.Time) ([]artifact.BuildRecord, error) {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return nil, err
	}
	if !validWorker(builder) || expires.IsZero() {
		return nil, invalid("invalid build renewal")
	}
	out := []artifact.BuildRecord{}
	for _, r := range d.builds.sorted(func(r *buildRow) bool { return r.State == artifact.BuildRunning && r.Builder == builder }) {
		if micro(expires).After(r.Expires) {
			r.Expires = micro(expires)
		}
		out = append(out, cloneBuild(r))
	}
	return out, nil
}

// AppendBuildLog adds lines to a build the builder holds.
func (s *Store) AppendBuildLog(ctx context.Context, id, builder string, attempt int, lines []string) error {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return err
	}
	r := d.builds.byID[id]
	if r == nil {
		return artifact.ErrNotFound
	}
	if !r.holds(builder, attempt) {
		return artifact.ErrConflict
	}
	if len(lines) == 0 {
		return nil
	}
	for _, line := range lines {
		r.log = append(r.log, artifact.ClipBuildLine(line))
	}
	r.LogLines += len(lines)
	if over := len(r.log) - artifact.MaxBuildLogLines; over > 0 {
		r.log = slices.Clone(r.log[over:])
	}
	d.change(artifact.ChangeBuild, id)
	return nil
}

// FinishBuild ends a build the builder holds.
func (s *Store) FinishBuild(ctx context.Context, id, builder string, attempt int, o artifact.BuildOutcome, now time.Time) (artifact.BuildRecord, error) {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return artifact.BuildRecord{}, err
	}
	if err := artifact.ValidateBuildOutcome(o); err != nil {
		return artifact.BuildRecord{}, err
	}
	r := d.builds.byID[id]
	if r == nil {
		return artifact.BuildRecord{}, artifact.ErrNotFound
	}
	if !r.holds(builder, attempt) {
		return artifact.BuildRecord{}, artifact.ErrConflict
	}
	r.end(o.State, now)
	r.Result, r.BinaryDigest, r.BinaryBytes = bytes.Clone(o.Result), o.BinaryDigest, o.BinaryBytes
	d.change(artifact.ChangeBuild, id)
	return cloneBuild(r), nil
}

// CancelBuild cancels a queued build and flags a running one.
func (s *Store) CancelBuild(ctx context.Context, id string, now time.Time) (artifact.BuildRecord, error) {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return artifact.BuildRecord{}, err
	}
	r := d.builds.byID[id]
	if r == nil {
		return artifact.BuildRecord{}, artifact.ErrNotFound
	}
	switch {
	case r.State.Terminal():
		return artifact.BuildRecord{}, artifact.ErrConflict
	case r.State == artifact.BuildQueued:
		r.end(artifact.BuildCancelled, now)
		d.change(artifact.ChangeBuild, id)
	case !r.CancelRequested:
		r.CancelRequested = true
		d.change(artifact.ChangeBuild, id)
	}
	return cloneBuild(r), nil
}

// ExpireBuilds ends the leases that ran out before now: a build whose
// cancellation was requested is cancelled, one that used its attempts fails
// with failed, and any other returns to the queue.
func (s *Store) ExpireBuilds(ctx context.Context, now time.Time, maxAttempts int, failed []byte) ([]artifact.BuildRecord, error) {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return nil, err
	}
	if maxAttempts < 1 || len(failed) > artifact.MaxBuildResult {
		return nil, invalid("invalid build expiry")
	}
	out := []artifact.BuildRecord{}
	for _, r := range d.builds.sorted(func(r *buildRow) bool { return r.State == artifact.BuildRunning && r.Expires.Before(now) }) {
		switch {
		case r.CancelRequested:
			r.end(artifact.BuildCancelled, now)
		case r.Attempt >= maxAttempts:
			r.end(artifact.BuildFailed, now)
			r.Result = bytes.Clone(failed)
		default:
			r.State, r.Builder, r.Expires = artifact.BuildQueued, "", time.Time{}
		}
		d.change(artifact.ChangeBuild, r.ID)
		out = append(out, cloneBuild(r))
	}
	return out, nil
}

// TrimBuilds deletes the terminal builds beyond the newest keep and drops the
// objects nothing references any more.
func (s *Store) TrimBuilds(ctx context.Context, keep int) (int, error) {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return 0, err
	}
	if keep < 0 {
		return 0, invalid("invalid build trim")
	}
	ended := d.builds.sorted(func(r *buildRow) bool { return r.State.Terminal() })
	if len(ended) <= keep {
		return 0, nil
	}
	gone := ended[:len(ended)-keep]
	for _, r := range gone {
		delete(d.builds.byID, r.ID)
	}
	d.sweep()
	return len(gone), nil
}

// CountBuilds counts the unfinished builds by state.
func (s *Store) CountBuilds(ctx context.Context) (artifact.BuildCounts, error) {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return artifact.BuildCounts{}, err
	}
	var c artifact.BuildCounts
	for _, r := range d.builds.byID {
		switch r.State {
		case artifact.BuildQueued:
			c.Queued++
		case artifact.BuildRunning:
			c.Running++
		}
	}
	return c, nil
}
