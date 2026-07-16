# job-and-task-archiver

Exports and optionally deletes completed, canceled, and errored Itential Platform job documents — along with all
associated tasks and job data — from a MongoDB database. Designed to run safely against production databases with minimal impact.

A companion tool, [`itential-orphan-archiver`](orphan-archiver/README.md), finds and optionally exports/deletes
orphaned `tasks`, `job_data`, `job_data.files`, and `job_data.chunks` documents whose parent job no longer exists —
records this tool would never touch, since it always walks forward from a live job's own ID.

## Contents

- [Features](#features)
- [How it works](#how-it-works)
- [Build](#build)
- [Install](#install)
  - [Create the archiver user](#create-the-archiver-user)
- [Testing](#testing)
- [Usage](#usage)
- [Flags](#flags)
- [Logging](#logging)
- [Config file](#config-file)
- [Examples](#examples)
- [Archiving from production to another database](#archiving-from-production-to-another-database)
- [Scheduling with cron](#scheduling-with-cron)
- [Scheduling with a systemd timer](#scheduling-with-a-systemd-timer)
- [Reclaiming disk space after delete](#reclaiming-disk-space-after-delete)
  - [Rolling resync (replica set, non-blocking for apps)](#rolling-resync-replica-set-non-blocking-for-apps)
  - [`compact` command (single-node, or quick fix)](#compact-command-single-node-or-quick-fix)
- [Example: export and import script](#example-export-and-import-script)
- [Output format](#output-format)
- [Companion tool: itential-orphan-archiver](orphan-archiver/README.md)

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

- The job is old enough, per its status (stored as milliseconds since epoch, not a BSON date):
  - `complete` and `canceled` jobs are aged off `metrics.end_time`
  - `error` jobs never reach that terminal step, so `metrics.end_time` is never set for them — they are aged off `metrics.start_time` instead
- `status` is `complete`, `canceled`, or `error` (pass `--ignore-error` to exclude `error` jobs and skip them instead)
- `ancestors` array has exactly one element (the job itself — this identifies parent jobs)

Each status is queried separately so the correct date field is used. The Phase 1 log line reports a count per status.

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
document, not the job ID. File document IDs are resolved first, then chunks are deleted by those IDs. If no
`job_data.files` documents are found for the job ID set (no GridFS attachments), steps 3 and 4 are skipped entirely.

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

### Create the archiver user

For scheduled, unattended runs (cron or systemd), run the archiver as a dedicated
unprivileged system user so log files, export directories, and the config file
are owned by a least-privilege account:

```bash
sudo groupadd --system archiver
sudo useradd  --system --gid archiver --home-dir /var/lib/archiver \
              --shell /usr/sbin/nologin --comment "Itential job archiver" archiver

sudo mkdir -p /etc/archiver /var/lib/archiver/runs /var/log/archiver
sudo chown -R archiver:archiver /var/lib/archiver /var/log/archiver
sudo chmod 750 /etc/archiver /var/lib/archiver /var/log/archiver
```

Place the config at `/etc/archiver/archiver.yaml` and restrict access — it
typically contains a MongoDB connection string with credentials:

```bash
sudo install -o root -g archiver -m 0640 \
  archiver.example.yaml /etc/archiver/archiver.yaml

# Then edit /etc/archiver/archiver.yaml with your URI, database, cutoff-days, etc.
sudo -u archiver vi /etc/archiver/archiver.yaml
```

`640 root:archiver` lets the archiver user read the config but not modify it,
and prevents non-archiver users from reading the MongoDB credentials.

For quick smoke-testing without setting up the system user, run as your own
user with a writable log directory:

```bash
itential-job-archiver --log-dir ./logs --config ./archiver.yaml
```

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
| `--version` | _(n/a)_ | _(n/a)_ | Print the version (from the git tag at build time) and exit. |
| `--config` | `ARCHIVER_CONFIG` | _(none)_ | Path to YAML config file. Auto-discovers `./archiver.yaml` if present. |
| `--uri` | `ARCHIVER_URI` | `mongodb://localhost:27017` | MongoDB connection URI. Use `mongodb+srv://` for Atlas. **Always quote on the command line** — replica set and Atlas URIs contain `?` and `&` characters that the shell interprets as special syntax if unquoted. |
| `--database` | `ARCHIVER_DATABASE` | _(required)_ | Database name. |
| `--cutoff-days` | `ARCHIVER_CUTOFF_DAYS` | _(required)_ | Archive jobs with status `complete`, `canceled`, or `error` (unless `--ignore-error`) before midnight UTC of the current day, minus this many days. |
| `--ids-file` | `ARCHIVER_IDS_FILE` | `job-ids.json` | Path where discovered job IDs are written after each run (for inspection only). |
| `--output-dir` | `ARCHIVER_OUTPUT_DIR` | `exports` | Directory where per-collection JSONL files are written. Created if it does not exist. |
| `--batch-size` | `ARCHIVER_BATCH_SIZE` | `1000` | Documents per batch for both export and delete. |
| `--batch-delay-ms` | `ARCHIVER_BATCH_DELAY_MS` | `100` | Milliseconds to sleep between batches. Increase to reduce database load. |
| `--export` | `ARCHIVER_EXPORT` | `true` | Export job documents to the output directory. Use `--export=false` to skip (boolean flags require `=` syntax). |
| `--delete` | `ARCHIVER_DELETE` | `false` | Delete documents after export. Deletion never runs unless this flag is explicitly set. Use `--delete=true` or just `--delete`. |
| `--skip-count` | `ARCHIVER_SKIP_COUNT` | `false` | Skip the per-collection document count summary after discovery. Useful for large datasets where the count queries are slow. |
| `--ignore-error` | `ARCHIVER_IGNORE_ERROR` | `false` | Exclude jobs with status `error` from the archive process — they are skipped, not exported or deleted. By default, `error` jobs are archived alongside `complete` and `canceled`. |
| `--read-preference` | `ARCHIVER_READ_PREFERENCE` | `secondaryPreferred` | MongoDB read preference. Valid values: `primary`, `primaryPreferred`, `secondary`, `secondaryPreferred`, `nearest`. |
| `--tls-ca-file` | `ARCHIVER_TLS_CA_FILE` | _(none)_ | Path to a PEM file containing the CA certificate. Use for on-prem deployments with a custom CA. |
| `--tls-cert-file` | `ARCHIVER_TLS_CERT_FILE` | _(none)_ | Path to a PEM file containing the client certificate (mutual TLS). Requires `--tls-key-file`. |
| `--tls-key-file` | `ARCHIVER_TLS_KEY_FILE` | _(none)_ | Path to a PEM file containing the client private key (mutual TLS). Requires `--tls-cert-file`. |
| `--tls-skip-verify` | `ARCHIVER_TLS_SKIP_VERIFY` | `false` | Disable TLS certificate verification. Insecure — avoid in production. |
| `--log-dir` | `ARCHIVER_LOG_DIR` | `/var/log/archiver` | Directory for the rotating log file. Created if missing. Set to `""` to disable file logging (stdout only). |
| `--log-file` | `ARCHIVER_LOG_FILE` | `archiver.log` | Log file name inside `--log-dir`. |
| `--log-max-size-mb` | `ARCHIVER_LOG_MAX_SIZE_MB` | `100` | Maximum size in MB of the active log file before rotation. |
| `--log-max-backups` | `ARCHIVER_LOG_MAX_BACKUPS` | `7` | Maximum number of rotated log files to retain. `0` keeps all. |
| `--log-max-age-days` | `ARCHIVER_LOG_MAX_AGE_DAYS` | `30` | Maximum age in days to retain rotated log files. `0` keeps all. |
| `--log-compress` | `ARCHIVER_LOG_COMPRESS` | `true` | Gzip rotated log files. |

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

## Logging

Output is written to stdout and tee'd to a rotating log file at
`/var/log/archiver/archiver.log` by default. Rotation is handled in-process by
[lumberjack](https://github.com/natefinch/lumberjack) — no `logrotate` config
needed. The log directory is created on first run if it does not exist; ensure
the process user has write permission, or override `--log-dir` to a path it can
write to (e.g. `--log-dir ./logs` for local runs). Set `--log-dir=""` to disable
file logging entirely.

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

**Archive only `complete` and `canceled` jobs, leaving `error` jobs untouched:**

```bash
./itential-job-archiver \
  --uri "$PROD_URI" \
  --database mydb \
  --cutoff-days 30 \
  --ignore-error
```

`error` jobs are excluded from discovery entirely — they are not exported and not deleted.

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
avoids the `%` escaping required in crontab and makes the dated output directory straightforward to set.

This setup assumes you have already created the `archiver` user as described in the
[Install](#create-the-archiver-user) section. The same wrapper is also used by the systemd timer in the next section,
so picking one scheduler does not lock you out of the other.

**1. Install the wrapper** at `/usr/local/sbin/run-archiver`:

```bash
sudo tee /usr/local/sbin/run-archiver >/dev/null <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

# Move out of whatever directory the caller was sitting in. When the wrapper
# is invoked via `sudo -u archiver` from a root shell in /root, find(1) tries
# to restore its initial CWD at the end of the traversal and fails because
# the archiver user can't read /root.
cd /var/lib/archiver

RETENTION_DAYS=30
EXPORT_ROOT="/var/lib/archiver/runs"
EXPORT_DIR="$EXPORT_ROOT/$(date +%Y-%m-%d)"

# Prune previous run directories older than the retention window before
# starting a new run.
find "$EXPORT_ROOT" -mindepth 1 -maxdepth 1 -type d -mtime +"$RETENTION_DAYS" \
  -exec rm -rf {} +

exec /usr/local/bin/itential-job-archiver \
  --config /etc/archiver/archiver.yaml \
  --output-dir "$EXPORT_DIR" \
  --ids-file  "$EXPORT_DIR/job-ids.json"
EOF
sudo chmod 0755 /usr/local/sbin/run-archiver
```

**2. Schedule it.** Use `/etc/cron.d/archiver` so cron runs as the `archiver` user (matching the systemd setup),
rather than dropping the entry into root's crontab:

```bash
sudo tee /etc/cron.d/archiver >/dev/null <<'EOF'
0 2 * * * archiver /usr/local/sbin/run-archiver
EOF
sudo chmod 0644 /etc/cron.d/archiver
```

This runs at 02:00 every day and writes each run to its own dated subdirectory:

```text
/var/lib/archiver/runs/
  2026-04-01/
    jobs.jsonl
    tasks.jsonl
    job_data.jsonl
    job_data.files.jsonl
    job_data.chunks.jsonl
    job-ids.json
  2026-04-02/
  2026-04-03/
```

`RETENTION_DAYS=30` in the wrapper controls how long each run directory is kept. Adjust to match your retention
policy. The binary's own log file at `/var/log/archiver/archiver.log` is rotated independently by the embedded
logger (configured via `log-max-*` in the YAML), so no additional log redirection is needed in the cron entry.

**3. Verify it's registered:**

```bash
ls -l /etc/cron.d/archiver
```

**4. Check recent runs:**

```bash
sudo tail -f /var/log/archiver/archiver.log
```

> **Note for RHEL/Rocky/Ubuntu users:** systemd timers are an alternative to cron that provide better logging via
> `journalctl` and resilience across reboots (`Persistent=true` will catch up a missed run after downtime). See the
> next section for a full setup. Either approach works — cron is simpler to set up, systemd timers are easier to
> operate at scale.

## Scheduling with a systemd timer

systemd timers replace cron on modern Linux distributions (RHEL/Rocky/Alma 8+,
Ubuntu 18.04+, Debian 10+). They are easier to introspect (`systemctl status`,
`journalctl -u`), survive reboots cleanly (`Persistent=true` catches up a
missed run after downtime), and can be sandboxed using systemd's built-in
hardening options.

This setup assumes you have already created the `archiver` user as described
in the [Install](#create-the-archiver-user) section.

**1. Install the wrapper script** at `/usr/local/sbin/run-archiver`. This is
the same wrapper used by the cron setup above — install it once and use either
scheduler. If you skipped the cron section, copy the wrapper from
[Scheduling with cron](#scheduling-with-cron) → step 1.

The wrapper exists so the dated run directory is computed in shell — systemd
unit files do not expand `$(date ...)` on their own.

**2. Create the service unit** at `/etc/systemd/system/archiver.service`:

```ini
[Unit]
Description=Itential job-and-task archiver
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
User=archiver
Group=archiver
ExecStart=/usr/local/sbin/run-archiver

# Sandboxing — the archiver only needs to write to its own data and log dirs.
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths=/var/lib/archiver /var/log/archiver
```

**3. Create the timer unit** at `/etc/systemd/system/archiver.timer`:

```ini
[Unit]
Description=Run the Itential job-and-task archiver nightly

[Timer]
OnCalendar=*-*-* 02:00:00
Persistent=true
RandomizedDelaySec=10m
Unit=archiver.service

[Install]
WantedBy=timers.target
```

`Persistent=true` ensures a run that was missed (host powered off or systemd
down) will fire as soon as the system is back. `RandomizedDelaySec=10m`
spreads concurrent runs across a fleet so they do not all hit MongoDB at
exactly 02:00:00.

**4. Enable and start the timer:**

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now archiver.timer
```

**5. Verify it's scheduled:**

```bash
systemctl list-timers archiver.timer
```

The `NEXT` column shows when the next run will fire; `LAST` shows the previous
firing (or `n/a` if it has not run yet).

**6. Run it once on-demand** to confirm the unit works without waiting for the
next scheduled fire:

```bash
sudo systemctl start archiver.service
sudo systemctl status archiver.service
```

**7. Inspect run output.** The binary writes structured log lines to the
rotated file at `/var/log/archiver/archiver.log` (configurable via `--log-dir`
in the YAML); systemd additionally captures stdout to the journal:

```bash
journalctl -u archiver.service -n 200 --no-pager     # most recent run
sudo tail -f /var/log/archiver/archiver.log          # tee'd log file
```

Use the journal for short investigations and the rotated log file for longer
history. The binary's lumberjack rotation handles log retention automatically
— no logrotate config required.

## Reclaiming disk space after delete

The archiver issues `deleteMany`, which marks the freed space as reusable
**inside the same collection** but does not return it to the operating system.
This is standard WiredTiger behavior — deletes do not shrink the data files.
After several months of nightly runs you may notice `du -sh` on the MongoDB
data directory has barely moved even though `db.jobs.countDocuments()` is way
down. That is expected; new job/task inserts will reuse the freed space first.

If you need the disk back — for capacity planning, to migrate to a smaller
volume, or just for hygiene — there are two practical approaches.

### Rolling resync (replica set, non-blocking for apps)

The cleanest option on a 3-node replica set. App traffic stays up the whole
time because at least two members are always available. For each node, one at
a time:

```bash
# In mongosh, on the primary, if the target node is the primary:
rs.stepDown(60)        # hand off primary; wait for new primary to elect

# On the target host:
sudo systemctl stop mongod
sudo rm -rf /var/lib/mongo/*                # adjust to your dbPath
sudo systemctl start mongod
```

The node will resync from a peer and write a fresh, compact dataset. Watch
`rs.status()` until the node shows `stateStr: "SECONDARY"` and `optimeDate`
caught up to the other members, then move on to the next node. After the
third node finishes, every member is compact.

Resync time scales with dataset size — expect hours for multi-hundred-GB
databases. Do it during a low-traffic window even though it is non-blocking,
because the initial sync pressures the source node.

### `compact` command (single-node, or quick fix)

Faster than resync but blocks writes to the collection being compacted. On
older MongoDB versions it can block the whole `mongod` process. Run during a
maintenance window:

```javascript
// In mongosh, against the itential database.
db.runCommand({ compact: "jobs",            force: true })
db.runCommand({ compact: "tasks",           force: true })
db.runCommand({ compact: "job_data",        force: true })
db.runCommand({ compact: "job_data.files",  force: true })
db.runCommand({ compact: "job_data.chunks", force: true })
```

`force: true` is required on a primary (otherwise the command refuses). On a
replica set, run it against each secondary first, then `rs.stepDown()` the
primary and run it against the former primary.

### What does not work

- **Restarting `mongod`** — fragmentation is on disk, not in memory.
- **`db.dropDatabase()`** — reclaims everything but also deletes everything.
  Only useful if you have already migrated the data elsewhere.

### When to do this

In most Itential deployments, you do not need to reclaim space at all. The
archiver runs nightly, so the steady-state disk footprint is roughly
"`cutoff-days` worth of jobs plus the worst-case fragmentation gap." Once
that gap is full, deletes free space that inserts immediately reuse. Reclaim
disk when you need to shrink the volume, replace storage, or migrate — not
on a schedule.

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
