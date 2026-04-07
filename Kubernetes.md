# Running job-and-task-archiver on Kubernetes

For Kubernetes environments the direct equivalent of a cron job is a **CronJob** resource — it is the native scheduler and handles everything cron does, with better observability and failure handling.

## Container image

A minimal multi-stage Dockerfile for this tool:

```dockerfile
FROM golang:1.22-alpine AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -ldflags="-s -w" -o job-and-task-archiver .

FROM alpine:3.19
RUN apk --no-cache add ca-certificates
COPY --from=builder /build/job-and-task-archiver /usr/local/bin/
ENTRYPOINT ["job-and-task-archiver"]
```

## Credentials

The MongoDB URI must be stored in a Secret, not a ConfigMap:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: archiver-mongo-credentials
  namespace: itential
type: Opaque
stringData:
  uri: "mongodb+srv://user:pass@cluster.mongodb.net/?retryWrites=true"
```

All `ARCHIVER_*` environment variables are supported and can be sourced from Secrets or ConfigMaps in the standard Kubernetes way.

## Basic CronJob

```yaml
apiVersion: batch/v1
kind: CronJob
metadata:
  name: job-and-task-archiver
  namespace: itential
spec:
  schedule: "0 2 * * *"          # 2am daily
  concurrencyPolicy: Forbid       # never run two instances simultaneously
  successfulJobsHistoryLimit: 3
  failedJobsHistoryLimit: 3
  jobTemplate:
    spec:
      backoffLimit: 0             # do not retry automatically — the tool is idempotent; retry manually
      template:
        spec:
          restartPolicy: Never
          containers:
            - name: archiver
              image: your-registry/job-and-task-archiver:latest
              args:
                - --database=itential
                - --cutoff-days=30
                - --delete
              env:
                - name: ARCHIVER_URI
                  valueFrom:
                    secretKeyRef:
                      name: archiver-mongo-credentials
                      key: uri
```

## Export file storage

The tool writes per-collection JSONL files to disk. In Kubernetes a pod's filesystem is ephemeral and disappears when the pod exits. Choose one of the following approaches based on your environment.

### Option 1 — PersistentVolumeClaim

The simplest approach. Works well for single-node clusters or when an NFS-backed storage class is available.

Because a PVC persists across runs, each run must write to a **dated subdirectory** — otherwise the next run overwrites the previous export before it can be copied off. `args` in a pod spec are not passed through a shell, so date expansion requires invoking `sh` explicitly:

```yaml
volumes:
  - name: exports
    persistentVolumeClaim:
      claimName: archiver-exports
containers:
  - name: archiver
    image: your-registry/job-and-task-archiver:latest
    command: ["/bin/sh", "-c"]
    args:
      - |
        job-and-task-archiver \
          --database=itential \
          --cutoff-days=30 \
          --output-dir="/exports/$(date +%Y-%m-%d)" \
          --delete
    volumeMounts:
      - name: exports
        mountPath: /exports
```

This produces a directory per run on the PVC:

```
/exports/
  2026-04-01/
  2026-04-02/
  2026-04-03/
```

Dated directories accumulate and must be pruned. Add a second container to the job that removes exports older than your retention window after the archiver exits:

```yaml
initContainers:
  - name: archiver
    # ... (as above)
containers:
  - name: cleanup
    image: alpine:3.19
    command: ["/bin/sh", "-c"]
    args:
      - find /exports -maxdepth 1 -type d -mtime +30 -exec rm -rf {} +
    volumeMounts:
      - name: exports
        mountPath: /exports
```

### Option 2 — initContainer with object storage upload (recommended for production)

Run the archiver as an `initContainer` writing to a shared `emptyDir`, then upload the exports to S3 or GCS in the main container. Using `initContainer` guarantees the upload only runs after the archiver exits successfully.

Each run gets a fresh `emptyDir` so there is no overwrite risk locally. The dated S3 prefix ensures each run's exports are preserved independently in object storage.

```yaml
volumes:
  - name: exports
    emptyDir: {}
initContainers:
  - name: archiver
    image: your-registry/job-and-task-archiver:latest
    command: ["/bin/sh", "-c"]
    args:
      - |
        job-and-task-archiver \
          --database=itential \
          --cutoff-days=30 \
          --output-dir="/exports/$(date +%Y-%m-%d)" \
          --delete
    env:
      - name: ARCHIVER_URI
        valueFrom:
          secretKeyRef:
            name: archiver-mongo-credentials
            key: uri
    volumeMounts:
      - name: exports
        mountPath: /exports
containers:
  - name: upload
    image: amazon/aws-cli:latest
    command: ["/bin/sh", "-c"]
    args:
      - aws s3 sync /exports s3://your-bucket/archives/
    volumeMounts:
      - name: exports
        mountPath: /exports
```

### Option 3 — Delete only, no export

If you are pruning job history without archiving to another database, skip the export entirely. No storage configuration is needed.

```yaml
args:
  - --database=itential
  - --cutoff-days=30
  - --export=false
  - --delete
```

## Key CronJob settings

| Setting | Recommended value | Reason |
|---|---|---|
| `concurrencyPolicy` | `Forbid` | Prevents overlapping runs on large or slow databases |
| `backoffLimit` | `0` | The tool is idempotent — prefer manual retry with full log visibility over silent automatic retries |
| `restartPolicy` | `Never` | Consistent with `backoffLimit: 0`; a failed pod is preserved for log inspection |
| `successfulJobsHistoryLimit` | `3` | Retains enough pods to review recent runs via `kubectl logs` |
| `failedJobsHistoryLimit` | `3` | Retains failed pods so logs are available for diagnosis |

## Checking run history and logs

```bash
# List recent job runs
kubectl get jobs -n itential

# List pods for a specific run
kubectl get pods -n itential -l job-name=<job-name>

# View logs from the most recent run
kubectl logs -n itential -l job-name=<job-name> --tail=100

# Trigger a run immediately outside the schedule (for testing)
kubectl create job --from=cronjob/job-and-task-archiver archiver-manual-run -n itential
```
