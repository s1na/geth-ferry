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
  parts/chaindata-live.tar.zst    # always
  parts/ancient-chain.tar.zst     # always
  parts/ancient-state.tar.zst     # PBSS only (full or archive)
  parts/triedb.tar.zst            # PBSS only
```

Up to four parts. `manifest.json` is written **last**, after every part
has uploaded and its sha256 is known. No manifest → upload was interrupted;
downloader treats the prefix as not-a-snapshot.

### What goes into each part

`chaindata-live.tar.zst` is the live pebble database — `tar -C <datadir>/geth -cf - chaindata`
streamed through zstd, but with `chaindata/ancient/` excluded. This is the
SST set, MANIFEST/CURRENT/OPTIONS bookkeeping, and the WAL `.log` files —
i.e. the bytes geth's KV-store layer touches every block.

`ancient-chain.tar.zst` is `chaindata/ancient/chain/` — the chain freezer
(headers, bodies, receipts, hashes, optional era1 files). Always present.

`ancient-state.tar.zst` is `chaindata/ancient/state/` — the PBSS state
freezer (account.data, account.index, storage.data, storage.index,
history.meta). Present on PBSS nodes; missing on HBSS.

`triedb.tar.zst` is `<datadir>/geth/triedb/`. Exists only when the
directory is present on disk — this is the PBSS journal (`merkle.journal`).
Without it geth rewinds to the last flushed state on restart, so it has to
travel with the snapshot.

Before splitting, ferry checks that `chaindata/ancient/` contains nothing
but the two known namespaces (`chain/`, `state/`). Any other entry is a
sign of a geth version we don't understand; ferry refuses to upload rather
than silently drop bytes.

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

`zstd -5` for both parts. `chaindata/` is mostly snappy-compressed pebble
SSTs and zstd-compressed `.cdat` freezer segments, so the ratio difference
between zstd levels 3 and 9 on this data is consistently under 2%. We sit
at the top of klauspost/compress's `SpeedDefault` band (zstd 3–5), which
is the highest level that streams in parallel across cores — anything
≥ 10 (`SpeedBestCompression`) is forced single-threaded by the library
and runs ~10× slower. Level is configurable via `--level`, but going
above 5 is rarely worth it.

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
s3://bucket/snapshots/?endpoint=s3.de.io.cloud.ovh.net&region=de
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
--level 5                  # zstd level, default 5
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
