# geth-ferry — design plan

A small Go tool that uploads and downloads geth datadir snapshots between a
node host and an S3-compatible object store. Replaces the manual
`tar | zstd | s3cmd` runbook in `archive_snapshot_upload.md` with something
that has a manifest, sha256 verification, and a sane CLI.

- **Repo:** `geth-ferry`
- **Binary / CLI:** `ferry`
- **Module path:** `github.com/s1na/geth-ferry`

## Goals (v1)

1. Upload a stopped node's datadir to a remote, with a manifest and
   per-part sha256.
2. Download a snapshot from the remote into an empty (or `--force`) datadir,
   verifying sha256 as it extracts.
3. Read the legacy single-file `chaindata-<block>.tar.lz4` snapshots already
   in the bucket.
4. Backend behind an interface: S3 first (with endpoint override for OVH /
   any S3-compatible store), `file://` for tests.

## Non-goals (v1)

- Hot snapshots from a running node. Geth must be stopped.
- Cross-snapshot diffing / incremental snapshots.
- Resume of an interrupted upload. Re-run starts the failed part over.
  Operators run inside tmux/screen exactly like today.
- Splitting the snapshot into per-table parts. The freezer has its own
  sub-structure; we don't mirror it. Two tarballs total.
- HBSS write support. PBSS only for new uploads. (Legacy HBSS-era snapshots
  still read fine through the legacy single-file path.)
- Auto-detecting block / role from rawdb. Operator passes `--block` and
  `--role` as flags.

## Snapshot layout

A snapshot is a directory at a known prefix on the remote:

```
s3://bucket/snapshots/<name>/
  manifest.json
  parts/chaindata.tar.zst
  parts/triedb.tar.zst       # only when <datadir>/geth/triedb/ exists (PBSS)
```

That's the whole layout. At most two parts. `manifest.json` is written
**last**, after both parts have uploaded and their sha256s are known. No
manifest → upload was interrupted; downloader treats the prefix as
not-a-snapshot.

### What goes into each part

`chaindata.tar.zst` is `tar -C <datadir>/geth -cf - chaindata`, streamed
through zstd. This includes the entire freezer at
`chaindata/ancient/{chain,state}/`, so headers/bodies/receipts/hashes/era1
and (on archive nodes) the PBSS state-history tables travel inside this
single tarball. We do not split by freezer table — geth's own
`.cidx`/`.ridx`/`.meta` files already describe what's present, including
the gaps that snap-synced/history-pruned nodes have.

`triedb.tar.zst` is `tar -C <datadir>/geth -cf - triedb`. It exists only
when `<datadir>/geth/triedb/` is present on disk — this is the PBSS journal
(`merkle.journal`). Without it geth rewinds to the last flushed state on
restart, so it has to travel with the snapshot.

Everything else under `<datadir>/geth/` is left out by design: `LOCK`,
`geth.ipc`, `blobpool/`, `nodes/`, `transactions.rlp`, `nodekey`,
`jwtsecret`. None of it is reproducible state, and the keys are per-host
secrets that should never land in a public bucket.

### Naming convention

```
geth-<chainid>-<role>-<block>-<YYYYMMDD>
```

- `geth` is a fixed prefix marking the producing client.
- `chainid` is the EVM chain ID (`1` mainnet, `11155111` sepolia).
- `role` ∈ `archive`, `full`. That's the whole taxonomy.
- `block` is the head block number at the moment geth was stopped.
- `YYYYMMDD` is UTC.

Example: `geth-1-archive-23456789-20260430`.

The same string is the directory name on the remote and the prefix used in
any `latest.txt` pointer.

State scheme is not in the name. New uploads are PBSS (we don't support
writing HBSS); PBSS-vs-HBSS is observable from whether a `triedb.tar.zst`
part exists, and the manifest records it explicitly.

### Codec

`zstd -13` for both parts. `chaindata/` is mostly snappy-compressed pebble
SSTs and zstd-compressed `.cdat` freezer segments — pushing past 13 buys
very little for a lot more CPU. The legacy runbook used 3; we move to 13
because the producing host has cores to spare and bandwidth, not CPU, is
the bottleneck. Level is configurable via `--level`.

`lz4` is decode-only, for legacy single-file snapshots.

### manifest.json schema

```json
{
  "version": 1,
  "name": "geth-1-archive-23456789-20260430",
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
  "level": 13,
  "parts": [
    {
      "name":              "parts/chaindata.tar.zst",
      "uncompressed_size": 2700000000000,
      "compressed_size":   2200000000000,
      "sha256":            "…"
    },
    {
      "name":              "parts/triedb.tar.zst",
      "uncompressed_size": 484363426,
      "compressed_size":   400000000,
      "sha256":            "…"
    }
  ]
}
```

`state_scheme` is descriptive only — ferry doesn't change behavior based on
it. It's there so a downloader can sanity-check before pulling 2 TB.

### Legacy compatibility

Legacy snapshots are single-file monoliths named
`chaindata-<block>.tar.lz4`, sitting flat in a bucket prefix (no manifest,
no `<name>/` subdirectory).

Detection rule on download:

1. If `--src` ends in `.tar.lz4` or `.tar.zst`, treat as a legacy single
   file. Stream `GET → lz4/zstd decode → tar extract` directly into
   `<datadir>/geth/`.
2. Otherwise treat `--src` as a snapshot directory: fetch `manifest.json`,
   download each part, verify sha256, extract.

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

type Object struct {
    Key     string
    Size    int64
    ETag    string
    ModTime time.Time
}
```

Dispatched by URL scheme:

- `s3://bucket/prefix/...` → S3 (AWS SDK v2, endpoint-overridable for OVH).
- `file:///abs/path` → local filesystem (tests).
- Future: `gcs://`, `https://`, etc.

S3 `Put` uses the SDK's multipart uploader internally. Non-secret config
goes on the URL query string:

```
s3://bucket/snapshots/?endpoint=s3.gra.io.cloud.ovh.net&region=gra
```

Credentials come from the standard AWS chain (env vars,
`~/.aws/credentials`, instance profile). No bespoke credential format.

## CLI

```
ferry upload   --src <datadir> --dst <remote> --name <n> --role <r> --block <n> [flags]
ferry download --src <remote>  --dst <datadir> [flags]
ferry list     --src <remote-prefix>
ferry verify   --src <remote-snapshot>
ferry inspect  <local-or-remote>     # print manifest, no I/O on parts
```

### Upload flags

```
--src <datadir>            # required, e.g. /datadrive/geth
--dst <remote>             # required
--name <name>              # required: geth-1-archive-23456789-20260430
--role archive|full        # required
--block <n>                # required (head block at stop time)
--level 13                 # zstd level, default 13
--threads <n>              # zstd encoder threads, default GOMAXPROCS
--force                    # ignore presence of LOCK / geth.ipc
```

### Download flags

```
--src <remote>             # required: snapshot directory URL or
                           # legacy single-file .tar.{lz4,zst} URL
--dst <datadir>            # required
--force                    # extract into a non-empty datadir
```

### Safety

- Refuse to upload if `<datadir>/geth/LOCK` or `<datadir>/geth/geth.ipc`
  exists. Override with `--force`.
- Refuse to download into a non-empty `<datadir>/geth/`. Override with
  `--force`.

## Code layout

```
github.com/s1na/geth-ferry
  cmd/ferry/                # cobra CLI, no business logic
  pkg/snapshot/
    manifest.go             # manifest types, JSON, validation
    layout.go               # naming, path helpers
  pkg/codec/
    zstd.go                 # klauspost/compress/zstd
    lz4.go                  # pierrec/lz4/v4 (decode only)
  pkg/backend/
    backend.go              # Backend interface
    s3/                     # aws-sdk-go-v2 + endpoint override
    file/                   # file:// for local + tests
    registry.go             # URL → Backend dispatch
  pkg/upload/               # tar → zstd → backend.Put, sha256 alongside
  pkg/download/             # manifest fetch, part download, verify, extract
  pkg/legacy/               # single-file .tar.lz4 / .tar.zst path
```

## Dependencies

- `github.com/aws/aws-sdk-go-v2` (S3 + endpoint override + multipart)
- `github.com/klauspost/compress/zstd` (pure-Go, multithreaded)
- `github.com/pierrec/lz4/v4` (pure-Go, decode only)
- `github.com/spf13/cobra` (CLI)
- stdlib `archive/tar`, `crypto/sha256`

No CGO.

## Milestones

1. **Skeleton.** Module bootstrap, `Backend` interface, `Manifest` types,
   `file://` backend, `pkg/codec/zstd`, `cmd/ferry` stubbed with cobra.
   Round-trip a tiny fake datadir to a local `file://` and back, with
   manifest and sha256 verified.
2. **S3 backend.** AWS SDK v2 with endpoint override. End-to-end round-trip
   against a real OVH bucket using a small datadir.
3. **Legacy reader.** Single-file `.tar.lz4` / `.tar.zst` download path.
4. **Polish.** `verify`, `inspect`, `list`, basic progress output, docs.
5. **Production run.** Replace the manual runbook for the next archive
   snapshot upload.
