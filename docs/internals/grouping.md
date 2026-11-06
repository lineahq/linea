# Grouping (groupbyattrs)

How the `groupbyattrs` processor regroups records, and how `report.grouping`
(report schema 0.15) explains it. The engine builds it in
`internal/debugger/analysis/grouping.go` (`analysis.Group`), wired from
`internal/debugger/engine/grouping.go`; `linea run` prints it and the web
client shows it on the processor's Grouping tab
(`web/src/inspect/GroupingPanel.tsx`). Verified against
`groupbyattrsprocessor` v0.162.0 (`processor.go`, `attribute_groups.go`) and a
run of `samples/resource-grouping`.

## What the processor does

For every span, log record or metric data point, in input order:

1. It looks up each key in `keys` on the record's own attributes. The keys
   found are removed from the record.
2. The record's target resource is its input resource's attributes with the
   found keys written over them (a record value replaces a resource value of
   the same key).
3. It appends the record to the output resource with exactly those
   attributes, creating it at the end when none exists yet. Resources are
   compared by an attribute hash, so input blocks whose attributes end up
   equal are merged, also for records that carried no key (compaction).
4. Within that resource the record joins the scope with the same name and
   version; metric data points join the metric with the same name and type.

Output resources therefore appear in the order their first record arrived.
The new resource holds only attributes: an input resource's
`dropped_attributes_count` and the resource and scope schema URLs are not
carried over. Scopes with equal name and version but different attributes
merge under the first one's scope.

## How the debugger explains it

The processor copies records into new resource groups, so record objects do
not survive it (`record-objects.md`); the stage diff then reports that
resources were merged or split without attributing their changes
(`components.md`). The record lineage still follows every record by content
(spans by ID), so `analysis.Group` ties each output record to its input
position through the lineage's `stage.input` and `stage.output` hops. Of the
configuration it reads only `keys`, to report them:

- `grouping[]` has one entry per processor and pipeline, with one request
  per stage input: its resource blocks before (attributes and record count)
  and the blocks of its first emission after.
- An output resource attribute is `moved` when the input block of a record
  under it lacked the attribute or held another value (compared as pdata
  values).
- A record lists the keys it gave up; a record with none kept its resource
  as it was (possibly merged with equal ones). An output block whose records
  came from more than one input block (`from`) was merged.
- Output records the lineage did not follow are counted (`unfollowed`), not
  guessed. A request without an emission has no `output` and no blocks
  after.

The section holds only resource attribute values and record names, which the
report's snapshots already carry. A stored report over its storage limit
drops it with the other derived sections (`omittedSections` in
`internal/debugger/platform/persistence.go`).
