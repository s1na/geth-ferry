# geth-ferry — design plan

A Go tool that uploads and downloads geth datadir snapshots between a node host
and a remote object store (S3 today, pluggable for later). Replaces the manual
`tar | zstd | s3cmd` runbook in `archive_snapshot_upload.md` with something
team-friendly: resumable, parallelizable, verifiable, and forward-compatible.

- **Repo:** `geth-ferry`
- **Binary / CLI:** `ferry`
- **Module path:** `github.com/s1na/geth-ferry`

## Goals

1. Upload a stopped node's datadir to a remote, with the snapshot split into
   independently compressed and verifiable parts.
2. Download a snapshot from the remote into an empty (or `--force`) datadir.
3. Read the legacy single-file snapshots already in the bucket
   (`chaindata-<block>.tar.lz4`).
4. Be agnostic to the remote: S3 first, but the backend is an interface so
   GCS / HTTP-mirror / `file://` can drop in later without touching CLI code.
5. Be agnostic to the codec: zstd by default (level configurable, default 19),
   lz4 supported for read of legacy archives. No write path for lz4.

## Non-goals (v1)

- Hot snapshots from a running node. Geth must be stopped, same as today.
- Cross-snapshot diffing / incremental snapshots.
- A daemon mode or scheduled rotation. Cron-driven from outside.
- Anything provider-specific beyond the S3 API. OVH is just an S3 endpoint.

## Snapshot layout

A snapshot is a directory at a known prefix on the remote:

```
s3://bucket/snapshots/<name>/
  manifest.json
  parts/chaindata-0000.tar.zst
  parts/chaindata-0001.tar.zst
  parts/...
  parts/ancient-headers.tar.zst
  parts/ancient-bodies.tar.zst
  parts/ancient-receipts.tar.zst
  parts/ancient-diffs.tar.zst        # PBSS state history (if archive)
```

`manifest.json` is written **last**, after all parts are uploaded and verified.
Its presence is the atomic "this snapshot is complete" signal. No manifest →
upload was interrupted; downloader treats the prefix as not-a-snapshot.

### Naming convention

```
<role>-<scheme>-<block>-<YYYYMMDD>
```

- `role` ∈ `archive`, `full`, `snap`
- `scheme` ∈ `pbss`, `hbss`
- `block` is the head block number at the moment geth was stopped
- `YYYYMMDD` is UTC

Examples:

```
archive-pbss-23456789-20260430
full-pbss-23456789-20260430
```

The same string is the directory name on the remote and the prefix used in any
`latest.txt` pointer.

### Why split into parts

- `ancient/` is already chunked by table (headers / bodies / receipts / diffs).
  Each table maps cleanly to one part. Independently verifiable, independently
  resumable.
- `chaindata/` (live pebble state) gets streamed-tar-and-split into fixed-size
  chunks (default 4 GiB uncompressed), splitting at file boundaries so each
  part is a self-contained tar that extracts cleanly on its own.
- A single-file corruption only invalidates one part, not the whole snapshot.
- Per-part `sha256` lets `verify` work without re-extracting.
- Future parallelism (download or upload) becomes a flag, not a rewrite.

This is the spirit of reth's static-file segmentation, adapted to geth's actual
on-disk layout.

### manifest.json schema

```json
{
  "version": 1,
  "name": "archive-pbss-23456789-20260430",
  "chain_id": 1,
  "role": "archive",
  "state_scheme": "path",
  "head": {
    "block": 23456789,
    "hash": "0x…",
    "timestamp": 1745923200
  },
  "created_at": "2026-04-30T12:00:00Z",
  "created_by": "ferry/0.1.0",
  "codec": "zstd",
  "level": 19,
  "contents": {
    "blocks":         { "from": 0,         "to": 23456789 },
    "receipts":       { "from": 0,         "to": 23456789 },
    "state_current":  { "block": 23456789 },
    "state_history":  { "from": 0,         "to": 23456789 },
    "trie_nodes":     { "from": 0,         "to": 23456789 }
  },
  "parts": [
    {
      "name": "parts/chaindata-0000.tar.zst",
      "kind": "chaindata",
      "uncompressed_size": 4294967296,
      "compressed_size":   3100000000,
      "sha256":            "…",
      "files": ["chaindata/MANIFEST-000123", "chaindata/000456.sst", "…"]
    },
    {
      "name": "parts/ancient-headers.tar.zst",
      "kind": "ancient",
      "table": "headers",
      "uncompressed_size": 12345678,
      "compressed_size":   2345678,
      "sha256":            "…"
    }
  ],
  "totals": {
    "uncompressed_size": 2900000000000,
    "compressed_size":   2200000000000
  }
}
```

`contents` describes **what data the snapshot carries** (the ranges, the state
scheme, etc.) so a downloader can decide whether this snapshot is the right
one for its target node mode without unpacking it. Optional sections are
omitted when not present:

- A `full` PBSS node would have `blocks`, `receipts`, `state_current`. No
  `state_history`, no `trie_nodes`.
- An archive PBSS node adds `state_history`.
- An archive HBSS node has `trie_nodes` instead of `state_history`.

### Legacy compatibility

Legacy snapshots are single-file monoliths named `chaindata-<block>.tar.lz4`,
sitting flat in a bucket prefix (no manifest, no `<name>/` subdirectory).

Detection rule on download:

1. If the `--src` URL ends in `.tar.lz4` or `.tar.zst`, treat as legacy single
   file. Stream `GET → lz4/zstd decode → tar extract` directly into
   `<datadir>/geth/`. Same code path reth uses, just adapted to our backends.
2. Otherwise treat `--src` as a snapshot directory: fetch `manifest.json`,
   then download and extract each part.

Legacy is read-only. We never write the legacy format. New uploads always use
the multi-part layout.

## Backend abstraction

```go
type Backend interface {
    List(ctx context.Context, prefix string) ([]Object, error)
    Stat(ctx context.Context, key string) (Object, error)
    // Get returns a stream starting at offset. offset==0 for a full read.
    Get(ctx context.Context, key string, offset int64) (io.ReadCloser, error)
    // Put returns a writer that streams to the remote. Implementations may
    // use multipart upload internally; Close finalizes.
    Put(ctx context.Context, key string) (io.WriteCloser, error)
    Delete(ctx context.Context, key string) error
}

type Object struct {
    Key     string
    Size    int64
    ETag    string
    ModTime time.Time
}
```

Backends are dispatched by URL scheme:

- `s3://bucket/prefix/...` → S3 (AWS SDK v2, endpoint-overridable for OVH /
  any S3-compatible store).
- `file:///abs/path` → local filesystem (great for tests, pre-staging
  multi-TB to a different mount before uploading).
- `https://host/path/` → read-only HTTP (legacy public mirrors).
- Future: `gcs://`, etc.

### Backend-specific flags

Decision: **URL query-string for non-secret config; env vars for credentials.**
Subcommand groups per backend would clutter the CLI as we add more backends.

```
ferry upload \
  --src /datadrive/geth \
  --dst 's3://geth-s3-storage/snapshots/?endpoint=s3.gra.io.cloud.ovh.net&region=gra' \
  --name archive-pbss-23456789-20260430

# credentials via standard env
AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=... ferry upload ...
```

Optional `~/.config/ferry/config.yaml` lets us alias remotes:

```yaml
remotes:
  ovh-archives:
    url: s3://geth-s3-storage/snapshots/
    endpoint: s3.gra.io.cloud.ovh.net
    region: gra
```

…then `--dst ovh-archives` is enough. Credentials still come from env / the
existing AWS credential chain (`~/.aws/credentials`, instance profile, etc.).
We do not invent our own credential file format.

## Streaming vs disk staging

Worth discussing carefully. The current runbook's `tar | zstd | s3cmd` never
lands the ~2 TB tarball anywhere on disk, which matters because the archive
host's free space is much less than the snapshot size.

For the new multi-part design we have three ways to honor that:

### Option A — full streaming (tar → zstd → S3 multipart pipe)

For each part:

```
tar.Writer ──► zstd.Encoder ──► io.Pipe ──► S3 UploadPart loop
```

- The S3 multipart uploader reads from the pipe in 8–16 MiB chunks and
  PUTs each chunk as a multipart part. RAM use is bounded by the pipe buffer
  + zstd's window (8 MiB at level 19's default; adjustable).
- We never write the compressed tarball to disk.
- Resume: each ferry-part is an S3 multipart upload. We persist `UploadId` +
  the list of completed S3 parts to a small local sidecar
  (`<datadir>/.ferry-state.json`). On restart, ferry queries
  `ListMultipartUploads` for each in-progress UploadId, finds which S3 parts
  already landed, and resumes from the next byte. If the sidecar is gone,
  ferry-part is restarted from zero (S3-side parts get garbage-collected via
  `AbortMultipartUpload`).
- Constraint: S3 requires every multipart part except the last to be ≥ 5 MiB,
  and at most 10 000 parts per upload. With 4 GiB ferry-parts at 16 MiB S3
  chunks → 256 S3 parts. Plenty of headroom.

### Option B — stage one part to disk, upload, delete, repeat

- Each ferry-part is tar+zstd-compressed to a temp file under `--stage-dir`,
  then uploaded with a regular multipart upload, then deleted.
- Free disk needed: one part-size (default 4 GiB).
- Resume: trivial — temp file on disk, retry the upload.
- Sequential by nature; no extra concurrency complexity.

### Option C — hybrid

- Default to A, but expose `--stage-dir <path>`. When set, switch to B.
- Useful when the host has spare disk and the operator wants the simplest
  possible failure mode, or when debugging.

**Recommendation: C.** Streaming by default (matches today's behavior), with
`--stage-dir` as the escape hatch for paranoia / debugging / hosts where the
S3-side flakiness makes restart-from-scratch painful.

Open question for you: do you want to start v1 with **A only** and add B in
v2, or is `--stage-dir` worth the extra plumbing on day one?

## CLI

```
ferry upload   --src <datadir> --dst <remote> [flags]
ferry download --src <remote>  --dst <datadir> [flags]
ferry list     --src <remote-prefix>
ferry verify   --src <remote-snapshot>
ferry inspect  <local-or-remote>     # print manifest, no I/O on the parts
```

### Flags (high-traffic ones)

```
upload:
  --name <name>              # required for now (autogen later from rawdb)
  --role archive|full|snap   # required, goes into manifest + filename
  --scheme pbss|hbss         # required
  --block <n>                # required (head block at stop time)
  --codec zstd               # default zstd; lz4 not a write target
  --level 19                 # zstd level 1-22, default 19
  --part-size 4GiB           # uncompressed bytes per chaindata part
  --concurrency 1            # parallel parts (v1: 1)
  --stage-dir <path>         # opt out of streaming
  --include chaindata,ancient/headers,ancient/bodies,ancient/receipts,ancient/diffs
  --promote                  # update <prefix>/latest.txt at the end
  --force                    # ignore presence of geth lock file
  --threads <n>              # zstd encoder threads (default GOMAXPROCS)

download:
  --name <name>              # required if --src is a prefix
  --concurrency 1            # parallel parts (v1: 1)
  --verify                   # check sha256 of each part before extract
  --skip <kinds>             # e.g. skip state_history when restoring as full
  --force                    # extract into a non-empty datadir
```

### Safety

- Refuse to upload if `<datadir>/geth/LOCK` or `<datadir>/geth/geth.ipc`
  exists. Override with `--force`. (geth must be stopped.)
- Refuse to download into a non-empty `<datadir>/geth/`. Override with
  `--force`.

## Code layout

```
github.com/s1na/geth-ferry
  cmd/ferry/                # thin cobra CLI, no business logic
  pkg/snapshot/
    manifest.go             # manifest types, JSON marshal, validation
    layout.go               # naming, path helpers
  pkg/codec/
    codec.go                # interface: Encoder, Decoder
    zstd.go                 # klauspost/compress/zstd
    lz4.go                  # pierrec/lz4/v4 (decode only)
  pkg/backend/
    backend.go              # Backend interface
    s3/                     # aws-sdk-go-v2 + endpoint override
    file/                   # file:// for local + tests
    http/                   # https:// read-only (for legacy mirrors)
    registry.go             # URL → Backend dispatch
  pkg/upload/               # planning, part streaming, resume sidecar
  pkg/download/             # manifest fetch, parallel part download, verify
  pkg/legacy/               # single-file .tar.lz4 / .tar.zst path
  pkg/progress/             # throttled progress aggregator (reth-style)
  internal/datadir/         # detect chain id / head block from rawdb (v2)
```

## Dependencies

- `github.com/aws/aws-sdk-go-v2` (S3 + endpoint override + multipart)
- `github.com/klauspost/compress/zstd` (pure-Go, level 1–22, multithreaded)
- `github.com/pierrec/lz4/v4` (pure-Go, decode path only)
- `github.com/spf13/cobra` (CLI)
- `github.com/spf13/viper` (config file, optional — could go without)
- stdlib `archive/tar`

No CGO. No mandatory geth import in v1 (we take `--block`, `--scheme`,
`--role` as flags). v2 adds `internal/datadir` to read these from `rawdb`
on a stopped pebble instance, removing those flags.

## Milestones

1. **Skeleton + interfaces.** Module bootstrap, `Backend` interface,
   `Manifest` types, `file://` backend, `pkg/codec/zstd`, `cmd/ferry`
   stubbed with cobra. Round-trip a tiny fake datadir to a local `file://`
   and back, with manifest, no streaming or resume yet.
2. **S3 backend.** AWS SDK v2 wired up with endpoint override. End-to-end
   round-trip against a real OVH bucket using a small test datadir.
3. **Streaming upload.** `tar → zstd → io.Pipe → S3 multipart`, sequential
   parts, no resume yet. Smoke test against a small datadir.
4. **Resume.** Local sidecar (`.ferry-state.json`), `AbortMultipartUpload`
   on cancel, `ListMultipartUploads` on restart. Resume tested by killing
   mid-upload.
5. **Streaming download + verify.** Parallel part download (concurrency
   flag), per-part sha256 verification, parallel tar extract.
6. **Legacy reader.** Single-file `.tar.lz4` and `.tar.zst` download path.
7. **Polish.** `verify`, `inspect`, `list`, `latest.txt` promote, progress
   bar, docs, dry-run flag.
8. **v2 add-ons (deferred).** Auto-detect block/scheme/role from rawdb.
   `--stage-dir`. Parallel upload. GCS backend.

## Open questions to resolve before milestone 1

1. **Stage-dir from day one, or v2?** I lean v2; honoring the streaming
   property of the current runbook from the start keeps the design honest.
2. **Where does `latest.txt` live?** Per-prefix (`snapshots/latest.txt`) or
   per-role (`snapshots/archive-pbss/latest.txt`)? I lean per-role so a
   `full` consumer doesn't accidentally fetch an `archive`.
3. **Do we want a `--dry-run` from day one?** Useful for upload because
   the cost of finding out you got the flags wrong after 4 hours is high.
   Trivial to add up front; I'd say yes.
4. **Should `verify` re-download or accept ETag/sha256 lookups only?**
   v1: re-download and recompute sha256. v2: trust ETag for unchanged
   parts.
