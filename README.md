# Mirage

A self-hosted file sync server in Go, built to run in a container on a Synology
NAS and write files **directly onto the NAS filesystem** — no opaque blob store,
no database full of your data. Files Mirage creates are owned by the right NAS
user, so they stay readable over SMB and in DSM File Station.

## Sync clients

Mirage implements the **Nextcloud server API**, so you sync with the official
Nextcloud desktop and mobile clients rather than a bespoke one. That is a
deliberate choice: those clients already ship deep OS integration that would
take years to reproduce.

| Mode | Support | Provided by |
|---|---|---|
| On-demand / virtual files | ✅ | Windows CfAPI, macOS File Provider, Linux placeholders |
| Selective sync | ✅ | Official client |
| Full offline copy | ✅ | Official client |

Point the Nextcloud client at your Mirage URL and it pairs through the normal
browser login flow.

## Status

Early development. Milestone progress:

- [x] **M0** — config, index database, CLI, container image
- [ ] **M1** — client pairing: `status.php`, capabilities, Login Flow v2
- [ ] **M2** — read-only sync: PROPFIND/GET, file index, ETag propagation
- [ ] **M3** — bidirectional sync: PUT/MKCOL/DELETE/MOVE/COPY, UID/GID stamping
- [ ] **M4** — chunked upload for large files
- [ ] **M5** — out-of-band changes: watcher, rescan, rename detection
- [ ] **M6** — instant sync via notify_push
- [ ] **M7** — trashbin, versions, previews
- [ ] **M8** — quotas, rate limiting, hardening

## Design

Three decisions shape everything else:

**The filesystem is authoritative.** SQLite holds an index, not your files.
Delete it and Mirage rebuilds it with a rescan. Because you can also change
files over SMB or in File Station, Mirage reconciles continuously: a watcher for
speed, a periodic rescan as the backstop that guarantees correctness.

**File IDs are stable, ETags propagate.** Sync clients skip any directory whose
ETag is unchanged, so a change anywhere must bump the ETag of every directory
above it. File IDs survive renames, which is what lets a client move a file
instead of re-downloading it.

**Tenants are isolated by the kernel, not by string checks.** Each account is
served through an `os.Root` handle confined to its own directory, so `..`
traversal and symlinks pointing outside it fail at the syscall.

## Running it

See [`deploy/docker-compose.yml`](deploy/docker-compose.yml) for the Synology
setup and [`deploy/mirage.example.yaml`](deploy/mirage.example.yaml) for an
annotated config.

```
mirage serve     # run the server
mirage doctor    # check the config and every user's storage mapping
mirage user      # list accounts, set passwords
```

Accounts live in the config file; `mirage doctor` will tell you if a mapping is
wrong before it turns into a permissions problem mid-sync.

## Development

```
go test ./...
go run ./cmd/mirage doctor -config ./local.yaml
```
