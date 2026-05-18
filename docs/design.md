# Design

This document captures the design and on-disk format of `ferry`. The
operator-facing usage lives in [`../README.md`](../README.md); this is
the longer story behind the choices.

## Problem

A geth datadir is a few hundred gigabytes to a few terabytes of state.
Moving one between hosts the manual way is `tar | zstd | s3cmd` driven
from `tmux`, with no manifest, no per-stream checksum, and no fail-fast
on partial writes. Ferry replaces that runbook with a single binary that:

- splits the datadir into a small, named set of parts,
- streams each part through `tar → zstd → S3 multipart` while accumulating
  a sha256,
- writes `manifest.json` **last**, so the absence of a manifest is the
  signal that the snapshot is incomplete,
- reads back symmetrically, verifying sha256 against the manifest before
  it trusts a part's contents.

Out of scope by design: hot snapshots from a running node, incremental /
diff snapshots, splitting tarballs into sub-table parts, writing the
legacy HBSS-era format, and resuming an interrupted upload (re-run starts
the failed part over — operators still run inside `tmux`/`screen` like
they did before).

## Snapshot layout

A snapshot is a directory at a known prefix on the remote:

```
s3://bucket/snapshots/<name>/
  manifest.json
  parts/chaindata-live.tar.zst    # always — live pebble (SSTs, MANIFEST, WAL)
  parts/chaindata-live.toc.zst    # ← per-part TOC sidecar
  parts/ancient-chain.tar.zst     # always — chain freezer
  parts/ancient-chain.toc.zst
  parts/ancient-state.tar.zst     # PBSS only — state freezer
  parts/ancient-state.toc.zst
  parts/triedb.tar.zst            # PBSS only — merkle.journal
  parts/triedb.toc.zst
```

Each `*.toc.zst` is a zstd-compressed text file: one `<size> <name>\n`
line per regular tar entry inside the corresponding part. This lets
`ferry contents` list a snapshot's files without touching the parts
themselves (typically KB-to-MB total instead of GB).

Up to four parts. `manifest.json` is written **last**, after every part
has uploaded and its sha256 is known. No manifest → upload was
interrupted; downloader treats the prefix as not-a-snapshot.

### What goes into each part

- **`chaindata-live.tar.zst`** — the live pebble database (`tar -C
  <datadir>/geth -cf - chaindata`, but with `chaindata/ancient/`
  excluded). This is the SST set, MANIFEST/CURRENT/OPTIONS bookkeeping,
  and the WAL `.log` files — the bytes geth's KV-store layer touches
  every block.

- **`ancient-chain.tar.zst`** — `chaindata/ancient/chain/`: the chain
  freezer (headers, bodies, receipts, hashes, optional era1 files).
  Always present.

- **`ancient-state.tar.zst`** — `chaindata/ancient/state/`: the PBSS
  state freezer (`account.data`, `account.index`, `storage.data`,
  `storage.index`, `history.meta`). Present on PBSS nodes; missing
  on HBSS.

- **`triedb.tar.zst`** — `<datadir>/geth/triedb/`: the PBSS journal
  (`merkle.journal`). Without it geth rewinds to the last flushed
  state on restart, so it has to travel with the snapshot.

Before splitting, ferry checks that `chaindata/ancient/` contains
nothing but the two known namespaces (`chain/`, `state/`). Any other
entry signals a geth version we don't understand; ferry refuses to
upload rather than silently drop bytes.

Everything else under `<datadir>/geth/` is left out by design: `LOCK`,
`geth.ipc`, `blobpool/`, `nodes/`, `transactions.rlp`, `nodekey`,
`jwtsecret`. None of it is reproducible state, and the keys are per-host
secrets that should never land in a public bucket.

### Naming convention

```
geth-<chainid>-<role>-<block>-<unix-seconds>
```

- `geth` — fixed prefix marking the producing client.
- `chainid` — EVM chain ID (`1` mainnet, `11155111` sepolia).
- `role` — `archive` or `full`. That's the whole taxonomy.
- `block` — head block number at the moment geth was stopped.
- `unix-seconds` — snapshot creation time, matching
  `manifest.created_at`. Locale-free, unambiguously orderable.

Example: `geth-1-archive-23456789-1746014400`.

State scheme is not in the name. New uploads are PBSS (we don't support
writing HBSS); PBSS-vs-HBSS is observable from whether a `triedb.tar.zst`
part exists, and the manifest records it explicitly.

### Codec

`zstd -5` for both parts. `chaindata/` is mostly snappy-compressed pebble
SSTs and zstd-compressed `.cdat` freezer segments, so the ratio
difference between zstd levels 3 and 9 on this data is consistently
under 2 %. We sit at the top of `klauspost/compress`'s `SpeedDefault`
band (zstd 3–5), which is the highest level that streams in parallel
across cores — anything ≥ 10 (`SpeedBestCompression`) is forced
single-threaded by the library and runs ~10× slower. Level is
configurable via `--level`, but going above 5 is rarely worth it.

`lz4` is decode-only, for the legacy single-file snapshots.

### `manifest.json` schema

```json
{
  "version": 1,
  "name": "geth-1-archive-23456789-1746014400",
  "chain_id": 1,
  "role": "archive",
  "state_scheme": "path",
  "head": {
    "block": 23456789,
    "hash": "0x…",
    "timestamp": 1745923200
  },
  "created_at": 1746014400,
  "created_by": "ferry/0.1.0",
  "codec": "zstd",
  "level": 5,
  "parts": [
    { "name": "parts/chaindata-live.tar.zst", "kind": "chaindata-live", "uncompressed_size": 2400000000000, "compressed_size": 2000000000000, "sha256": "…" },
    { "name": "parts/ancient-chain.tar.zst",  "kind": "ancient-chain",  "uncompressed_size":  280000000000, "compressed_size":  240000000000, "sha256": "…" },
    { "name": "parts/ancient-state.tar.zst",  "kind": "ancient-state",  "uncompressed_size":   20000000000, "compressed_size":   18000000000, "sha256": "…" },
    { "name": "parts/triedb.tar.zst",         "kind": "triedb",         "uncompressed_size":     484363426, "compressed_size":     460000000, "sha256": "…" }
  ]
}
```

`state_scheme` is descriptive only — ferry doesn't change behavior based
on it. It's there so a downloader can sanity-check before pulling 2 TB.

### Legacy compatibility

Legacy snapshots are single-file monoliths named like
`chaindata-<block>.tar.lz4`, sitting flat in a bucket prefix (no
manifest, no `<name>/` subdirectory).

Detection rule on download:

1. If `--src` ends in `.tar.lz4` or `.tar.zst`, treat as a legacy single
   file. Stream `GET → lz4/zstd decode → tar extract` directly into
   `<datadir>/geth/`.
2. Otherwise treat `--src` as a snapshot directory: fetch
   `manifest.json`, download each part, verify sha256, extract.

Legacy is read-only. We never write the legacy format.

## Backend abstraction

```go
type Backend interface {
    List(ctx context.Context, prefix string) ([]Object, error)
    Stat(ctx context.Context, key string) (Object, error)
    Get(ctx context.Context, key string) (io.ReadCloser, error)
    Put(ctx context.Context, key string) (io.WriteCloser, error)
    Delete(ctx context.Context, key string) error
}
```

Dispatched by URL scheme:

- `s3://bucket/prefix/...` → S3 (AWS SDK v2, endpoint-overridable for
  OVH and other S3-compatible providers).
- `file:///abs/path` → local filesystem (tests, staging).
- Future: `gcs://`, `https://`, etc. — register against
  `backend.Register("scheme", opener)`.

S3 `Put` drives multipart upload itself rather than using the SDK's
`manager.Uploader`. The SDK ≥ v1.30 wraps `UploadPart` bodies in
`aws-chunked` encoding with a CRC32 trailer, which OVH rejects with
`IncompleteBody`. The hand-rolled writer sends plain part bodies with an
explicit `Content-Length` and an inline `Crc32` header — works against
both native AWS S3 and the S3-compatible providers we care about.

Non-secret config goes on the URL query string:

```
s3://my-bucket/snapshots/?endpoint=s3.example.com&region=us-east-1
```

Credentials come from the standard AWS chain (env vars,
`~/.aws/credentials`, instance profile). No bespoke credential format.

## Code layout

```
github.com/s1na/geth-ferry
  cmd/ferry/              # cobra CLI, no business logic
  pkg/snapshot/           # manifest types, naming, URL helpers
  pkg/codec/              # zstd encoder + lz4 decoder
  pkg/backend/            # Backend interface and dispatch
    file/                 # local-filesystem implementation
    s3/                   # aws-sdk-go-v2 + endpoint override
  pkg/upload/             # tar → zstd → backend.Put, sha256 alongside
  pkg/download/           # manifest fetch, part download, verify, extract
  pkg/legacy/             # single-file .tar.lz4 / .tar.zst path
  pkg/progress/           # throttled stderr byte/rate reporter
  internal/datadir/       # read-only rawdb peek for head/chain-id/scheme
```

## Safety

- Refuse to upload if `<datadir>/geth/LOCK` or `<datadir>/geth/geth.ipc`
  exists. Override with `--force`.
- Refuse to download into a non-empty `<datadir>/geth/`. Override with
  `--force` — semantically *replace* (not merge): a successful run
  atomically swaps a freshly-extracted tree into place; a failed run
  leaves the original untouched.
- Tar extraction rejects entries that would escape the destination
  (`..`-relative paths, absolute symlinks). Device files, fifos, and
  hard links are silently skipped — geth doesn't need them.
- Ctrl+C (SIGINT/SIGTERM) cancels the root context. Backends propagate:
  in-flight S3 multipart uploads are `AbortMultipartUpload`'d with a
  fresh background context (so the abort RPC reaches the server even
  though the request context is cancelled); download scratch
  directories are removed.

## Atomicity

**Upload.** Each part is a single multipart upload. The writer hands the
caller two terminators: `Close` commits (`CompleteMultipartUpload`) and
`Abort` discards (`AbortMultipartUpload`). The upload code defers
`Abort` and only sets a `committed` flag after `Close` returns nil — so
any mid-stream failure (tar walk, zstd encode, network) leaves no
committed object on the bucket. The manifest is written last; its
absence is the integrity signal.

**Download.** Parts are extracted into a hidden scratch directory
(`.ferry-partial-*/`) next to `<datadir>/geth/`. After every part has
streamed and its sha256 has been verified, the scratch is renamed into
`<datadir>/geth/`. Same-filesystem rename ensures atomicity; with
`--force`, the original tree is removed only at this final promote
step, so a failure midway preserves it.

## Concurrency

Two independent dials:

- `--multipart-concurrency` (per object): in-flight `UploadPart`
  requests for a single multipart upload. Default 5. Each in-flight
  part borrows a `--multipart-size` buffer from a per-Backend
  `sync.Pool`, so steady-state allocation is concurrency × part-size
  regardless of how many parts the object turns into.
- `--parallel-parts` (per snapshot): how many of the snapshot's parts
  upload (or download) concurrently. Default 1 (the historical
  sequential behavior). With `N > 1`, peak memory scales linearly
  because each in-flight part owns its own multipart buffer set.

Parallel parts are write-disjoint by construction: `chaindata-live`
covers `chaindata/` minus `ancient/`; `ancient-chain` covers
`chaindata/ancient/chain/`; `ancient-state` covers
`chaindata/ancient/state/`; `triedb` covers `triedb/`. No part writes
into another's namespace, so concurrent extraction is race-free.

## Dependencies

- `github.com/aws/aws-sdk-go-v2` — S3 client + endpoint override.
- `github.com/klauspost/compress/zstd` — pure-Go, multithreaded.
- `github.com/pierrec/lz4/v4` — pure-Go, decode only.
- `github.com/cockroachdb/pebble` — read-only datadir inspection.
- `github.com/spf13/cobra` — CLI.
- stdlib `archive/tar`, `crypto/sha256`, `encoding/json`.

No CGO.
