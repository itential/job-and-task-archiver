# job-and-task-archiver

Exports and optionally deletes completed and canceled Itential Platform job documents — along with all associated
tasks and job data — from a MongoDB database. Designed to run safely against production databases with minimal impact.

## Features

- **Domain-aware queries** — finds eligible parent jobs first, then expands to all child jobs, exactly matching the logic of the reference `delete-jobs` Node.js script
- **Cascade delete** — removes documents from all five related collections: `jobs`, `tasks`, `job_data`, `job_data.files`, `job_data.chunks`
- **Safe deletion order** — tasks and job data are deleted before jobs, so job IDs remain discoverable if the run is interrupted
- **Idempotent** — re-running is always safe; discovery runs fresh every time, and exporting to a dated output directory preserves each run independently
- **Non-blocking** — reads default to `secondaryPreferred` to avoid loading the primary; configurable batch delays throttle write pressure
- **TLS support** — custom CA, mutual TLS, and Atlas `mongodb+srv://` URIs
- **Flexible config** — CLI flags, `ARCHIVER_*` environment variables, or a YAML config file

## How it works

### Document discovery (two-phase)

**Phase 1** — query the `jobs` collection for parent jobs that meet all three criteria:

- `metrics.end_time` is older than the cutoff date (stored as milliseconds)
- `status` is `complete` or `canceled`
- `ancestors` array has exactly one element (the job itself — this identifies parent jobs)

**Phase 2** — expand to all related jobs (parents and children) by querying for any job whose first ancestor
(`ancestors.0`) matches a parent ID from phase 1. This captures child jobs that may not individually meet the age or
status criteria but belong to an eligible parent.

The discovered IDs are saved to `job-ids.json` for post-run inspection, but are not read back on the next run — discovery always starts fresh.

### Cascade delete (safe order)

Deletions happen in this order so that job IDs remain queryable until the very end — allowing safe resume if the process is interrupted:

| Step | Collection | Filter |
| --- | --- | --- |
| 1 | `tasks` | `job._id` in job IDs |
| 2 | `job_data` | `job_id` in job IDs |
| 3 | `job_data.chunks` | `files_id` in file document IDs |
| 4 | `job_data.files` | `metadata.job` in job IDs |
| 5 | `jobs` | `_id` in job IDs |

`job_data.chunks` requires a two-phase delete: `files_id` references the `_id` of the parent `job_data.files`
document, not the job ID. File document IDs are resolved first, then chunks are deleted by those IDs.

## Build

Binaries are written to the `dist/` directory. The version string is set automatically from the current git tag.

```bash
make all        # build for all platforms
make mac        # darwin/amd64 and darwin/arm64
make linux      # linux/amd64 and linux/arm64 (RHEL/Rocky compatible)
make windows    # windows/amd64
make clean      # remove dist/
```

| Target | Output file |
| --- | --- |
| `mac` | `dist/itential-job-archiver-darwin-amd64` |
| `mac` | `dist/itential-job-archiver-darwin-arm64` |
| `linux` | `dist/itential-job-archiver-linux-amd64` |
| `linux` | `dist/itential-job-archiver-linux-arm64` |
| `windows` | `dist/itential-job-archiver-windows-amd64.exe` |

```bash
go mod tidy   # update dependencies
```

## Install

Download the appropriate archive for your platform from the [releases page](../../releases), then install the binary and create a short symlink:

```bash
tar -xzf itential-job-archiver-linux-amd64.tar.gz
sudo cp itential-job-archiver-linux-amd64 /usr/local/bin/
sudo ln -s /usr/local/bin/itential-job-archiver-linux-amd64 /usr/local/bin/itential-job-archiver
```

The symlink lets you invoke the tool as `itential-job-archiver` regardless of platform or architecture.

## Testing

Unit tests cover the core logic and do not require a MongoDB connection.

```bash
make test                              # run all tests
make coverage                          # run tests with coverage report
go test ./... -v                       # verbose output
go test ./... -run TestBatchDelete     # run a specific test
```

## Usage

```text
./itential-job-archiver [flags]
```

## Flags

| Flag | Env var | Default | Description |
| --- | --- | --- | --- |
| `--config` | `ARCHIVER_CONFIG` | _(none)_ | Path to YAML config file. Auto-discovers `./archiver.yaml` if present. |
| `--uri` | `ARCHIVER_URI` | `mongodb://localhost:27017` | MongoDB connection URI. Use `mongodb+srv://` for Atlas. **Always quote on the command line** — replica set and Atlas URIs contain `?` and `&` characters that the shell interprets as special syntax if unquoted. |
| `--database` | `ARCHIVER_DATABASE` | _(required)_ | Database name. |
| `--cutoff-days` | `ARCHIVER_CUTOFF_DAYS` | _(required)_ | Archive jobs completed or canceled before midnight UTC of the current day, minus this many days. |
| `--ids-file` | `ARCHIVER_IDS_FILE` | `job-ids.json` | Path where discovered job IDs are written after each run (for inspection only). |
| `--output-dir` | `ARCHIVER_OUTPUT_DIR` | `exports` | Directory where per-collection JSONL files are written. Created if it does not exist. |
| `--batch-size` | `ARCHIVER_BATCH_SIZE` | `1000` | Documents per batch for both export and delete. |
| `--batch-delay-ms` | `ARCHIVER_BATCH_DELAY_MS` | `100` | Milliseconds to sleep between batches. Increase to reduce database load. |
| `--export` | `ARCHIVER_EXPORT` | `true` | Export job documents to the output directory. Use `--export=false` to skip (boolean flags require `=` syntax). |
| `--delete` | `ARCHIVER_DELETE` | `false` | Delete documents after export. Deletion never runs unless this flag is explicitly set. Use `--delete=true` or just `--delete`. |
| `--skip-count` | `ARCHIVER_SKIP_COUNT` | `false` | Skip the per-collection document count summary after discovery. Useful for large datasets where the count queries are slow. |
| `--read-preference` | `ARCHIVER_READ_PREFERENCE` | `secondaryPreferred` | MongoDB read preference. Valid values: `primary`, `primaryPreferred`, `secondary`, `secondaryPreferred`, `nearest`. |
| `--tls-ca-file` | `ARCHIVER_TLS_CA_FILE` | _(none)_ | Path to a PEM file containing the CA certificate. Use for on-prem deployments with a custom CA. |
| `--tls-cert-file` | `ARCHIVER_TLS_CERT_FILE` | _(none)_ | Path to a PEM file containing the client certificate (mutual TLS). Requires `--tls-key-file`. |
| `--tls-key-file` | `ARCHIVER_TLS_KEY_FILE` | _(none)_ | Path to a PEM file containing the client private key (mutual TLS). Requires `--tls-cert-file`. |
| `--tls-skip-verify` | `ARCHIVER_TLS_SKIP_VERIFY` | `false` | Disable TLS certificate verification. Insecure — avoid in production. |

> **URI quoting**: always wrap the URI in single or double quotes on the command line. Replica set and Atlas URIs
> contain `?` and `&` which the shell treats as special characters when unquoted.
> `--uri "mongodb+srv://user:pass@cluster/?retryWrites=true&authSource=admin"` is correct; omitting the quotes
> silently truncates the URI at the `?` or backgrounds the process at `&`.
>
> **Cutoff timing**: the cutoff is always pinned to midnight UTC of the current day, minus `--cutoff-days`. Running
> the tool at 9am or 11pm on the same day with the same `--cutoff-days` produces identical results. This makes
> scheduled and ad-hoc runs predictable and comparable.
>
> **Boolean flag syntax**: boolean flags must use `=` when setting them to `false`. Use `--export=false`, not
> `--export false`. The latter is parsed as `--export` (true) with `false` as an unrecognized argument and silently
> ignored.

## Config file

Create `archiver.yaml` in the working directory (or pass `--config path/to/file.yaml`). YAML keys match the long flag names.

```yaml
uri: "mongodb+srv://user:pass@cluster.mongodb.net/?retryWrites=true"
database: mydb
cutoff-days: 30
output-dir: exports
batch-size: 500
batch-delay-ms: 250
export: true
delete: false
read-preference: secondaryPreferred
```

Priority order: CLI flag > environment variable > config file > default.

## Examples

**Preview — count eligible jobs without exporting or deleting:**

```bash
./itential-job-archiver \
  --uri "$PROD_URI" \
  --database mydb \
  --cutoff-days 30 \
  --export=false
```

This runs discovery and the document count summary, then exits without writing any files or deleting anything.

## Archiving from production to another database

The safest approach is a two-phase workflow: export first, verify the import, then delete.

**Phase 1 — discover and export:**

```bash
EXPORT_DIR="exports/$(date +%Y-%m-%d)"

./itential-job-archiver \
  --uri "$PROD_URI" \
  --database mydb \
  --cutoff-days 30 \
  --output-dir "$EXPORT_DIR"
```

This writes one file per collection to a dated subdirectory:

```text
exports/
  2026-04-03/
    jobs.jsonl
    tasks.jsonl
    job_data.jsonl
    job_data.files.jsonl
    job_data.chunks.jsonl
```

Using a dated directory means each run's exports are preserved independently. If the import or a follow-on copy step
fails, the data is still there — re-running on the same day writes to the same dated directory. Only collections with
matching documents produce a file.

**Import the exported data into the archive database:**

`mongoimport` accepts a single file per invocation and does not support importing a directory. Import each collection separately:

```bash
for f in "$EXPORT_DIR"/*.jsonl; do
  collection=$(basename "$f" .jsonl)
  mongoimport \
    --uri "$ARCHIVE_URI" \
    --db archive \
    --collection "$collection" \
    --mode insert \
    --file "$f"
done
```

**Verify the import before proceeding:**

```bash
mongosh "$ARCHIVE_URI" --eval \
  'db.getSiblingDB("archive").jobs.countDocuments()'
```

**Phase 2 — delete from production:**

```bash
./itential-job-archiver \
  --uri "$PROD_URI" \
  --database mydb \
  --cutoff-days 30 \
  --export=false \
  --delete
```

Discovery runs fresh, then deletes. If this run is interrupted, re-run the same command — the delete is idempotent.

## Scheduling with cron

A cron job is a scheduled task that runs automatically at a defined interval on Unix-based systems. Without automated
scheduling, database cleanup depends on someone remembering to run it manually — which means it doesn't happen
consistently. Adding this tool to cron ensures job history is pruned on a regular cadence, preventing unbounded
collection growth before it becomes a performance problem.

The recommended pattern is to run the archiver nightly during off-peak hours via a wrapper script. Using a wrapper
avoids the `%` escaping required in crontab and makes the dated output directory straightforward to set:

```bash
#!/usr/bin/env bash
# /opt/archiver/run-archiver.sh
set -euo pipefail

EXPORT_DIR="/var/archives/$(date +%Y-%m-%d)"

itential-job-archiver \
  --config /etc/archiver.yaml \
  --output-dir "$EXPORT_DIR"

# Remove export directories older than 30 days
find /var/archives -maxdepth 1 -type d -mtime +30 -exec rm -rf {} +
```

Edit the crontab with `crontab -e` and call the wrapper:

```text
0 2 * * * /opt/archiver/run-archiver.sh >> /var/log/archiver.log 2>&1
```

This runs at 2:00am every day and writes exports to a dated subdirectory:

```text
/var/archives/
  2026-04-01/
  2026-04-02/
  2026-04-03/
```

Each run's exports are preserved independently, so a failed copy or upload does not risk losing the previous night's
data. The cleanup at the end of the wrapper removes directories older than 30 days — adjust to match your retention
policy. Adjust all paths to match your environment.

To verify the job is registered:

```bash
crontab -l
```

To check that it ran and review its output:

```bash
tail -f /var/log/archiver.log
```

> **Note for RHEL/Rocky Linux users:** systemd timers are an alternative to cron that provide better logging via
> `journalctl` and resilience across reboots (`Persistent=true` will catch up a missed run after downtime). Either
> approach works — cron is simpler to set up, systemd timers are easier to operate at scale.

## Example: export and import script

The following script runs the archiver and then imports each exported collection into an archive database.

```bash
#!/usr/bin/env bash
set -euo pipefail

PROD_URI="mongodb://host1:27017,host2:27017,host3:27017/?replicaSet=rs0&readPreference=secondaryPreferred"
ARCHIVE_URI="mongodb://archive-host:27017"
DATABASE="itential"
ARCHIVE_DB="itential_archive"
CUTOFF_DAYS=30
EXPORT_DIR="exports/$(date +%Y-%m-%d)"

# Export from production into a dated directory
./itential-job-archiver \
  --uri "$PROD_URI" \
  --database "$DATABASE" \
  --cutoff-days "$CUTOFF_DAYS" \
  --output-dir "$EXPORT_DIR"

# Import each collection into the archive database
for f in "$EXPORT_DIR"/*.jsonl; do
  collection=$(basename "$f" .jsonl)
  echo "Importing $collection..."
  mongoimport \
    --uri "$ARCHIVE_URI" \
    --db "$ARCHIVE_DB" \
    --collection "$collection" \
    --mode upsert \
    --file "$f"
done

# Compress the dated export directory and remove the uncompressed copy
tar -czf "${EXPORT_DIR}.tar.gz" "$EXPORT_DIR"
rm -rf "$EXPORT_DIR"

# Remove compressed archives older than 30 days
find exports -maxdepth 1 -name "*.tar.gz" -mtime +30 -delete

echo "Done. Archive: ${EXPORT_DIR}.tar.gz"
```

`set -euo pipefail` ensures the script exits immediately if the archiver or any `mongoimport` invocation fails,
rather than silently continuing with a partial import. Because the export directory is dated, a failure at any step
leaves the previous run's data intact.

The `tar` step compresses the directory and removes the uncompressed copy. The result (e.g.
`exports/2026-04-03.tar.gz`) is a self-contained archive for that run. The final `find` removes compressed archives
older than 30 days — adjust to match your retention policy.

To inspect or restore an archive later:

```bash
# List contents
tar -tzf exports/2026-04-03.tar.gz

# Extract
tar -xzf exports/2026-04-03.tar.gz
```

## Output format

Each JSONL file contains one document per line, exactly as it exists in MongoDB — no fields are added or modified:

```json
{"_id":"507f1f77bcf86cd799439011","status":"complete","metrics":{"end_time":1704067200000},"ancestors":["507f1f77bcf86cd799439011"]}
{"_id":"507f1f77bcf86cd799439012","status":"canceled","metrics":{"end_time":1704153600000},"ancestors":["507f1f77bcf86cd799439012"]}
```

JSONL (newline-delimited JSON) is well-suited for this use case:

- **Streamable**: each line is a complete, self-contained document. Tools like `mongoimport`, `jq`, `grep`, and `awk` can process the file line by line without loading it all into memory.
- **Recoverable**: a crash mid-write at most corrupts the line being written. All previous lines remain valid.
- **Compatible**: `mongoimport` natively accepts JSONL as input.
