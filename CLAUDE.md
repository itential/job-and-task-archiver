# CLAUDE.md — job-and-task-archiver

## What This Tool Does

`job-and-task-archiver` is a Go CLI that safely exports and optionally deletes completed and canceled Itential Platform job documents — along with all associated tasks and job data — from a MongoDB database. It is designed to run against **production databases** with minimal impact.

## Core Design Goals

- **Non-blocking**: reads default to `secondaryPreferred` to avoid loading the primary; batch delays throttle write pressure
- **Idempotent**: re-running is always safe — discovery runs fresh every invocation, export files are overwritten (not appended), and delete is safe to re-run because jobs are deleted last
- **Per-collection export**: each collection is written to its own JSONL file in an output directory (`exports/` by default); documents are written exactly as stored in MongoDB — no fields added or modified
- **Safe deletion order**: tasks and job data are deleted before jobs, so job IDs remain discoverable if interrupted
- **Atlas-compatible**: `mongodb+srv://` URIs work without TLS flags; explicit TLS is for on-prem deployments

## Three Phases

The application has three distinct phases:

1. **Find** — two-phase discovery that produces the job ID set. Always runs fresh. Read-only, no side effects.
2. **Export** — reads documents from all five collections, writes per-collection JSONL files to the output directory. Controlled by `--export` (default `true`).
3. **Delete** — removes documents from all five collections in safe order. Controlled by `--delete` (default `false`).

Phases 2 and 3 are independently optional. Phase 1 always runs.

### Idempotency by phase

- **Phase 1 crash**: re-run; discovery repeats from scratch with no side effects
- **Phase 2 crash**: re-run; export files are truncated and rewritten from scratch (`O_TRUNC`)
- **Phase 3 crash**: re-run; remaining job IDs are still in the `jobs` collection; discovery finds them and deletion continues. No orphan risk — tasks and job_data are deleted before jobs, so by the time `jobs` is being deleted, everything else is already gone.

## Collections Operated On

Five collections, in safe deletion order (jobs last):

| Collection | Filter used for deletion |
|---|---|
| `tasks` | `job._id` in job IDs |
| `job_data` | `job_id` in job IDs |
| `job_data.chunks` | `files_id` in **file document IDs** (two-phase — see below) |
| `job_data.files` | `metadata.job` in job IDs |
| `jobs` | `_id` in job IDs |

**GridFS two-phase delete**: `job_data.chunks.files_id` references the `_id` of the parent `job_data.files` document — it is **not** the job ID. Deleting chunks requires:

1. Query `job_data.files` where `metadata.job` in job IDs → collect file document `_id` values (ObjectIDs)
2. Delete `job_data.chunks` where `files_id` in those file document IDs
3. Delete `job_data.files` where `metadata.job` in job IDs

This is implemented in `deleteGridFS()` and `findFileIDs()`. The count summary uses the same two-phase approach.

**Field types confirmed from a production deployment**:
- `jobs._id` — BSON string (hex, e.g. `"17ea0fe657ac4da1b5fec4b1"`)
- `tasks.job._id` — BSON string
- `job_data.job_id` — BSON string
- `job_data.files.metadata.job` — BSON string
- `job_data.files._id` — BSON ObjectID (generated, unrelated to job ID)
- `job_data.chunks.files_id` — BSON ObjectID (references `job_data.files._id`)

The code preserves each field's actual BSON type via `bson.M` decoding in `findIDs()` to avoid type mismatches in `$in` filters.

## Job Discovery Logic

**Phase 1** — find parent jobs:
```
jobs.find({
  $and: [
    { "metrics.end_time": { $lt: cutoffMS } },   // milliseconds, not BSON date
    { "status": { $in: ["complete", "canceled"] } },
    { "ancestors": { $size: 1 } }                 // parent jobs only
  ]
})
```

**Phase 2** — expand to parents + all children:
```
jobs.find({ "ancestors.0": { $in: parentIDs } })
```

`metrics.end_time` is stored as **milliseconds since epoch** (not a BSON date).

### Cutoff calculation

The cutoff is pinned to **midnight UTC of the current day**, minus N days:

```go
now := time.Now().UTC()
midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
cutoff := midnight.AddDate(0, 0, -cfg.CutoffDays)
```

This means two runs on the same day with the same `--cutoff-days` always produce the same cutoff timestamp, regardless of what time they are run. `AddDate` handles month and year boundaries correctly.

### ancestors field type detection

Itential Platform versions differ on whether `ancestors` stores BSON ObjectIDs or hex strings. `ancestorsStoredAsStrings()` samples one parent document from the Phase 1 result to detect which type is in use, then Phase 2 sends the correct type in its `$in` filter. Sending the wrong type causes MongoDB to return 0 results silently.

### Recommended indexes

The Phase 2 query (`ancestors.0 $in`) performs a COLLSCAN without an index. Create:

```javascript
db.jobs.createIndex({ "ancestors.0": 1 }, { background: true })
```

The `job_data` collection should have an index on `job_id` — verify with `db.job_data.getIndexes()`.

## Architecture

Single `main.go` file. No subcommands, no subpackages. Keep it that way unless complexity demands otherwise.

### Data flow

```
initConfig
  │
  ▼
buildMongoClient
  │
  ▼
discoverJobIDs (two-phase query, always fresh)
  │
  ▼
save job-ids.json (for inspection only — never read back)
  │
  ▼
summarizeAffectedDocuments (unless --skip-count)
  │
  ▼
findFileIDs (resolve GridFS file IDs for export)
  │
  ▼
runExport (if --export) ──► exports/<collection>.jsonl per collection
  │
  ▼ (if --delete)
deleteAllCollections
  tasks → job_data → [findFileIDs → job_data.chunks → job_data.files] → jobs
```

### ID cache file (`job-ids.json`)

Written after discovery for post-run inspection only. **Never read back** — the next run always re-discovers. Delete it freely; it has no operational impact.

### Output directory (`exports/`)

One JSONL file per collection. Files are created with `O_TRUNC` — each run overwrites the previous output. If a collection has no matching documents, no file is created for it. Configurable via `--output-dir`.

## MongoDB Interfaces

To enable unit testing without a real MongoDB connection, the application defines thin interfaces over the driver types:

- `CursorAPI` — `Next`, `Decode`, `Close`, `All`, `Err`
- `SingleResultAPI` — `Decode`
- `CollectionAPI` — `Name`, `Find`, `FindOne`, `DeleteMany`, `CountDocuments`
- `DatabaseAPI` — `Collection`

`mongoCollection` and `mongoDatabase` are the production adapters that wrap the real driver types. All MongoDB-touching functions accept these interfaces rather than concrete driver types.

## Configuration

Handled by `pflag` + `viper` (same pattern as `ipctl` and `iag`). Viper env prefix is `ARCHIVER_`; hyphens in flag names become underscores in env vars (e.g. `--cutoff-days` → `ARCHIVER_CUTOFF_DAYS`).

YAML config file is auto-discovered as `./archiver.yaml` or passed via `--config`. YAML keys match flag names (hyphenated).

Key flags:

| Flag | Default | Notes |
|---|---|---|
| `--uri` | `mongodb://localhost:27017` | Use `mongodb+srv://` for Atlas |
| `--database` | _(required)_ | |
| `--cutoff-days` | _(required)_ | Days before today's midnight UTC |
| `--output-dir` | `exports` | Directory for per-collection JSONL files |
| `--export` | `true` | Set `--export=false` to skip (use `=` syntax) |
| `--delete` | `false` | Must be explicitly set to enable deletion |
| `--batch-size` | `1000` | Documents per batch |
| `--batch-delay-ms` | `100` | Throttle between batches |
| `--skip-count` | `false` | Skip per-collection count summary |
| `--read-preference` | `secondaryPreferred` | |

## TLS

`buildTLSConfig()` is only invoked when the caller sets at least one of `--tls-ca-file`, `--tls-cert-file`, or `--tls-skip-verify`. For Atlas (`mongodb+srv://`), TLS is negotiated automatically — no extra flags needed.

- Custom CA only → `--tls-ca-file`
- Mutual TLS → `--tls-cert-file` + `--tls-key-file` (both required together)
- Skip verify → `--tls-skip-verify` (insecure; avoid in production)

## Build & Test

```bash
make all        # all platforms → dist/
make mac        # darwin amd64 + arm64
make linux      # linux amd64 + arm64 (RHEL/Rocky compatible)
make windows    # windows amd64
make test       # run unit tests
make clean      # remove dist/
go mod tidy     # update dependencies
```

Go version: **1.22.2**. Binaries land in `dist/`. Version string embedded from `git describe`.

Unit tests live in `main_test.go` and use mock implementations of the MongoDB interfaces. They do not require a running MongoDB instance.

## Dependencies

| Package | Purpose |
|---|---|
| `go.mongodb.org/mongo-driver v1.17.2` | MongoDB client |
| `github.com/spf13/pflag v1.0.5` | POSIX-style CLI flags |
| `github.com/spf13/viper v1.19.0` | Config layering (flags + env + YAML) |

## Known Constraints & Gotchas

- **`job_data.chunks` requires two-phase delete**: `files_id` is the `_id` of the `job_data.files` document, not the job ID. `findFileIDs()` resolves file IDs first; `deleteGridFS()` uses them. Do not simplify to a direct job ID filter — it will silently delete nothing.
- **`metrics.end_time` is milliseconds**: not a BSON date, not seconds. The filter passes raw `int64` milliseconds to MongoDB.
- **Export files are overwritten on every run**: `O_TRUNC` is intentional. There is no resume — re-running always produces a clean export.
- **Cutoff slides daily, not hourly**: the cutoff is fixed at midnight UTC of the current day. Running the tool multiple times on the same day with the same `--cutoff-days` produces the same result set.
- **`ancestors.0` COLLSCAN without index**: Phase 2 is slow without an index on `ancestors.0`. See recommended indexes above.

## What NOT to Change Without Care

- **Deletion order**: tasks/data before jobs. If jobs are deleted first and the run is interrupted, their IDs are gone and phase 3 cannot resume.
- **Phase 2 discovery query** (`ancestors.0`): finds parents AND all children in one pass. Changing this filter will miss child jobs.
- **GridFS deletion order**: chunks before files (both before jobs). `deleteGridFS()` handles this pair. Do not reorder.
- **Cutoff calculation**: pinned to midnight UTC via `AddDate`. Do not revert to duration arithmetic — it produces inconsistent results depending on time of day.
- **`O_TRUNC` on export files**: intentional. Changing to `O_APPEND` reintroduces duplicate records on re-run.
- **MongoDB interface signatures**: `CollectionAPI.DeleteMany` and `CountDocuments` intentionally omit variadic options — they only expose what the application actually uses. Expanding them requires updating both the interface and all mock implementations in tests.

## Deferred Work

- **Parallel export and delete**: both phases can be parallelized using `golang.org/x/sync/errgroup`. Export: all five collections are independent, all filter IDs are pre-resolved before `runExport` is called. Delete: `tasks`, `job_data`, and `deleteGridFS` are independent and can run concurrently; `jobs` must still be last. Estimated ~3-5x speedup on large datasets. Side effect: increased MongoDB read/write pressure proportional to the number of goroutines; `--batch-delay-ms` applies per goroutine.
