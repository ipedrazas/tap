# casa-collector

Uploads the session files agents write to a shared directory into an
S3-compatible bucket (Tigris). It runs beside the agent, so the agent holds no
bucket credentials and carries no upload code: an agent only has to write
files.

It is its own Go module with no dependencies outside the standard library, so
this directory can move to its own repository as is.

## The contract for producers

1. Write each session to `<dir>/<agent>/<session_id>.trace.json`. The object
   key is `--prefix` plus the path relative to `--dir`.
2. Write atomically: write to a dotfile or a `*.tmp` file in the same
   directory, then rename it into place. The collector ignores both. A producer
   that writes in place is covered by `--settle` (files modified within the last
   2s wait for the next scan), but renaming is the reliable option.
3. Rewrite the whole file when the session changes. The collector uploads the
   latest version of each file. Objects are never deleted from the bucket, and
   deleting a local file only makes the collector forget it.

## How it works

The collector scans `--dir` every `--interval`. It uploads any matching file
whose size or mtime changed since its last upload. A failed upload is retried
on the next scan. On SIGTERM it runs a final scan that ignores `--settle` and
retries until `--drain-timeout` runs out. `--once` runs one scan and exits, for
cron jobs or backfills.

Without `--state-file`, everything is uploaded again on start (PUTs are
idempotent). With it, uploads are remembered across restarts. Keep the state
file outside `--dir`, so the directory can be mounted read-only.

Credentials come from `--credentials-dir` (`access-key-id` and
`secret-access-key`, read again for every request so rotated keys are picked
up) or from `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`. Outbound requests
honour `HTTPS_PROXY`.

## Security

The collector holds the bucket key and the producer does not. Producers can
still choose which files the collector reads, so:

- Files are opened through an `os.Root` and symlinks are skipped, so a
  symlink cannot point the collector at its own credentials.
- Mount the credentials on a different filesystem from `--dir` (a Secret
  volume is a tmpfs), so a hard link cannot reach them either.
- Mount `--dir` read-only in the collector. Where the producer and the
  collector share a filesystem view (compose bind mounts, one host), also run
  them as different uids. In a Kubernetes pod each container sees only its
  own mounts, so the producer never sees the credentials volume.

## In tap

tap renders the collector into every agent pod when `platform.yaml` has a
`sessions` block. The harness writes `/traces/<session_id>.trace.json` to a
volume only the harness and the collector mount. The collector uploads it
under `--prefix=<agent>`, so a compromised harness cannot write to another
agent's keys. Uploads go out through tap's egress proxy:
`--egress-proxy`, `--egress-key-file`, `--egress-agent` and
`--egress-scope=collector:sessions` mint a fresh credential for every
connection. The token format is described in `egress.go`; the test vector
there was minted by tap's `pkg/egress`.

## Compose

```yaml
services:
  agent:
    volumes: [sessions:/sessions]
  casa-collector:
    image: casa-collector
    command: [--dir=/sessions, --bucket=hiddenfield-tap-sessions, --prefix=ppm, --state-file=/state/state.json]
    env_file: .sessions-writer.env   # AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY
    volumes: [sessions:/sessions:ro, collector-state:/state]
volumes:
  sessions:
  collector-state:
```

## Kubernetes

As a native sidecar (an init container with `restartPolicy: Always`), it
starts before the agent and stops after it, so the final scan sees the agent's
last writes:

```yaml
initContainers:
  - name: casa-collector
    image: casa-collector@sha256:...
    restartPolicy: Always
    args: [--dir=/workspace/traces, --bucket=hiddenfield-tap-sessions, --credentials-dir=/run/casa/credentials]
    volumeMounts:
      - {name: workspace, mountPath: /workspace, readOnly: true}
      - {name: sessions-credentials, mountPath: /run/casa/credentials, readOnly: true}
```

## Flags

| Flag | Default | |
|---|---|---|
| `--dir` | | directory to scan (required) |
| `--bucket` | | bucket to upload to (required) |
| `--endpoint` | `https://t3.storage.dev` | S3 endpoint |
| `--region` | `auto` | signing region |
| `--prefix` | | key prefix |
| `--match` | `*.trace.json` | glob on base names |
| `--credentials-dir` | | key files; empty uses the AWS env vars |
| `--interval` | `5s` | time between scans |
| `--settle` | `2s` | minimum file age before upload |
| `--max-size` | 64 MiB | larger files are skipped |
| `--state-file` | | remember uploads across restarts |
| `--drain-timeout` | `20s` | time allowed for the final scan |
| `--once` | | scan once and exit |
| `--egress-proxy` | | tap-style egress proxy `host:port` |
| `--egress-key-file` | | HMAC key for proxy credentials |
| `--egress-agent` | | agent the credentials are for |
| `--egress-scope` | | scope the credentials are for |
