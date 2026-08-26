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
- [x] **M1** — client pairing: `status.php`, capabilities, Login Flow v2
- [x] **M2** — read-only sync: PROPFIND/GET, file index, ETag propagation
- [x] **M3** — bidirectional sync: PUT/MKCOL/DELETE/MOVE/COPY, UID/GID stamping
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

## Trying it with a real client

Sync is bidirectional as of M3: create, edit, rename and delete on a client and
it reaches the NAS, and the reverse. Files land owned by the mapped NAS user, so
they stay usable over SMB.

Chunked upload arrives in M4. Until then Mirage does not advertise it, so
clients upload even large files with a single PUT. That works; it just restarts
from the beginning if the connection drops.

Either run it directly:

```
mirage doctor            # confirm the config and each user's storage mapping
mirage user passwd alice # set a password for pairing
mirage scan              # build the index (the server also does this on start)
mirage serve
```

or bring up the local Docker stack, which serves sample files from
`.local/homes/alice` on port 8080:

```
docker compose up --build -d
docker compose exec mirage mirage user passwd alice
```

Then in the Nextcloud desktop client, use **Log in** and enter your
`external_url`. The client opens a browser, you sign in, and it pairs — the
account password is only used here, and the client gets its own app password.

Three things to know while testing:

`external_url` must be the address the client can actually reach, because it is
baked into the pairing and poll URLs handed to the client. A container-internal
address will pair and then hang.

Until the filesystem watcher lands in M5, `storage.rescan_interval` is how long
it takes for a file added over SMB to reach clients. Lower it for testing, or
run `mirage scan`.

Mirage must run as **root** to chown files to each user's uid/gid. It will
otherwise still work, but files land owned by the server process and are
unreachable over SMB. `mirage doctor` warns about this, and every affected
upload is logged.

## Running it

[`deploy/mirage.example.yaml`](deploy/mirage.example.yaml) is an annotated
config. For the NAS there are two deployments:

- [`deploy/docker-compose.yml`](deploy/docker-compose.yml) — plain, on a LAN port.
- [`deploy/docker-compose.tailscale.yml`](deploy/docker-compose.tailscale.yml) —
  behind a Tailscale sidecar running `tailscale serve`, which gives you
  `https://mirage.<your-tailnet>.ts.net` with a real certificate and nothing to
  renew. Reachable only from your tailnet, so nothing is exposed publicly.
  Sync clients send credentials on every request, so HTTPS is worth having.

Mirage builds no URL from the `Host` header — every absolute URL comes from
`server.external_url` — so it sits behind a proxy without extra configuration.
The one requirement is that `external_url` matches the name clients use.

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
