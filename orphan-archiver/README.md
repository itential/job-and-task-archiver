# itential-orphan-archiver

A companion tool to [job-and-task-archiver](../README.md). It finds and optionally exports/deletes orphaned Itential
Platform documents in `tasks`, `job_data`, `job_data.files`, and `job_data.chunks` — records whose parent job no
longer exists in the `jobs` collection.

Orphans like these are not expected to occur under normal use of `job-and-task-archiver`, which always deletes
child records before the parent job. They can appear when a job document is removed some other way — a manual
delete, a legacy script with a different deletion order, or a partially-run process outside this tool.

## How it works

Unlike `job-and-task-archiver`, this tool does not filter by age or status: a record is either orphaned (its
parent job is gone) or it isn't. There is no "too recent to touch" state to protect, since the parent is already
gone.

Discovery runs one MongoDB aggregation per collection, using `$lookup` to join against `jobs` and keep only
documents with no match:

- `tasks` — orphaned if `job._id` matches no `jobs._id`
- `job_data` — orphaned if `job_id` matches no `jobs._id`
- `job_data.files` — orphaned if `metadata.job` matches no `jobs._id`
- `job_data.chunks` — orphaned if `files_id` does not resolve to a "valid" `job_data.files` document, where valid
  means the file document both exists and its own `metadata.job` still points to a live job. This single query
  captures both orphan cases for chunks: a `files_id` with no `job_data.files` document at all, and a `files_id`
  whose file document exists but is itself already orphaned.

Each collection is queried independently — `job_data.chunks` resolves file validity itself in a nested `$lookup`
rather than depending on a separately discovered orphan file ID list.

**This performs a full collection scan on each of the four collections.** There is no field that flags a document
as orphaned ahead of time, so every document must be checked. The `$lookup` joins themselves are indexed (against
`jobs._id` and `job_data.files._id`, both primary keys), but the base scan is not avoidable. Run this during
low-traffic windows and consider the default `--read-preference=secondaryPreferred` to keep the load off the
primary.

### Progress feedback during a scan

Because each collection's orphan check is one long-running aggregation with no natural per-document progress
signal, a large scan can otherwise go silent for minutes at a time — indistinguishable from the process being
stuck (or from a dropped connection, if you're running this over an unreliable network path). Discovery logs two
things to make this visible:

- **Before scanning a collection**, its estimated document count (`EstimatedDocumentCount`, a fast metadata read,
  not a scan) so you know roughly what you're waiting on.
- **During the scan**, a heartbeat line every `--progress-interval-secs` (default `30`) showing elapsed time and
  orphans found so far, e.g.:

  ```text
    tasks: ~12,458,213 documents to scan
    tasks: still scanning... 90s elapsed, 3 orphans found so far
    tasks: still scanning... 120s elapsed, 7 orphans found so far
  ```

Set `--progress-interval-secs=0` to disable the heartbeat entirely.

## Usage

```bash
# Discovery + export only (default) — writes JSONL files, deletes nothing
./itential-orphan-archiver --uri "$PROD_URI" --database mydb

# Discovery only, no export, no delete
./itential-orphan-archiver --uri "$PROD_URI" --database mydb --export=false

# Discovery, export, and delete
./itential-orphan-archiver --uri "$PROD_URI" --database mydb --delete
```

## Flags

| Flag | Env var | Default | Notes |
| --- | --- | --- | --- |
| `--config` | `ORPHAN_CONFIG` | _(none)_ | Path to YAML config file. Auto-discovers `./orphan-archiver.yaml` if present. |
| `--uri` | `ORPHAN_URI` | `mongodb://localhost:27017` | MongoDB connection URI. Use `mongodb+srv://` for Atlas. |
| `--database` | `ORPHAN_DATABASE` | _(required)_ | Database name. |
| `--report-file` | `ORPHAN_REPORT_FILE` | `orphan-report.json` | Path where discovered orphan IDs are written after each run (for inspection only, never read back). |
| `--output-dir` | `ORPHAN_OUTPUT_DIR` | `orphan-exports` | Directory where per-collection JSONL files are written. |
| `--batch-size` | `ORPHAN_BATCH_SIZE` | `1000` | Documents per batch for both export and delete. |
| `--batch-delay-ms` | `ORPHAN_BATCH_DELAY_MS` | `100` | Milliseconds to sleep between batches. |
| `--progress-interval-secs` | `ORPHAN_PROGRESS_INTERVAL_SECS` | `30` | Seconds between discovery heartbeat log lines per collection. Set to `0` to disable. |
| `--export` | `ORPHAN_EXPORT` | `true` | Export orphaned documents. Use `--export=false` to skip. |
| `--delete` | `ORPHAN_DELETE` | `false` | Delete orphaned documents after export. Never runs unless explicitly set. |
| `--read-preference` | `ORPHAN_READ_PREFERENCE` | `secondaryPreferred` | MongoDB read preference. |
| `--tls-ca-file` / `--tls-cert-file` / `--tls-key-file` / `--tls-skip-verify` | `ORPHAN_TLS_*` | _(none)_ | Same TLS options as `job-and-task-archiver`. |
| `--log-dir` | `ORPHAN_LOG_DIR` | `/var/log/orphan-archiver` | Rotating log file directory. Set to `""` to disable file logging. |

## Idempotency

Discovery always runs fresh, exactly like `job-and-task-archiver`. If a delete run is interrupted, the next run
simply re-discovers whatever orphans remain in each collection — there is no shared parent ID to coordinate
resumability around, since each collection's orphan status is independent of the others.

## Safety

- `--delete` must be explicitly set; the default is discover + export only.
- Export always runs before delete unless `--export=false` is passed.
- Deletion order is `tasks`, `job_data`, `job_data.chunks`, `job_data.files` — chunks before files, matching
  `job-and-task-archiver`'s convention, so a chunk never outlives the file document it references (even though
  both are being removed in the same run).
