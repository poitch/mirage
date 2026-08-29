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
- [x] **M4** — chunked upload for large files
- [x] **M5** — out-of-band changes: watcher, rescan, rename detection
- [x] **M6** — instant sync via notify_push
- [x] search by filename, account pictures, a browse-and-download web view
- [x] **M7** — trashbin, versions, previews
- [ ] **M8** — quotas, rate limiting, hardening

## The web view

There is a small browser view at `/web/`, and account holders sign in to it with
their own Mirage credentials. It browses folders, shows thumbnails, searches by
filename, restores deleted files and earlier versions, and lets somebody change
their own password or add a device without an administrator in the room. It does
not edit, upload or share &mdash; Mirage is a sync server, and the clients are
how you work with your files.

It exists because a search result has to lead somewhere. Clicking one in the
desktop client normally opens the file locally without involving the server at
all; when the client cannot do that, it opens a browser at the folder, and this
is what it finds.

## Accounts

Accounts are managed from a small admin page at `/admin`. It maps each account
onto a directory on the NAS and shows, live, whether that directory exists, is
writable, and whether its ownership matches the uid and gid Mirage will stamp on
files — the mapping mistake that otherwise surfaces as a permissions error
partway through a sync.

Sign in with `MIRAGE_ADMIN_USERNAME` and `MIRAGE_ADMIN_PASSWORD`, set on the
container. **With no password set the page is not served at all**, which is the
deliberate default: it can repoint any account at any directory.

The config file may still declare accounts instead. If it does, it is
authoritative and the admin page becomes read-only — one source of truth rather
than two. `mirage user add` covers scripting and the case where no admin
password is set.

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

Large files upload in chunks, so an interrupted transfer resumes from what
already arrived rather than starting over.

Either run it directly:

```
mirage serve             # then open /admin to add accounts
mirage doctor            # check the config and each account's storage mapping
mirage scan              # rebuild the index (the server also does this on start)
```

or bring up the local Docker stack on port 8080:

```
docker compose up --build -d
```

Then open <http://localhost:8080/admin> and sign in with the credentials from
`compose.yaml`.

Then in the Nextcloud desktop client, use **Log in** and enter your
`external_url`. The client opens a browser, you sign in, and it pairs — the
account password is only used here, and the client gets its own app password.

Three things to know while testing:

`external_url` must be the address the client can actually reach, because it is
baked into the pairing and poll URLs handed to the client. A container-internal
address will pair and then hang.

A file added over SMB or in File Station reaches clients within about a second:
the watcher notices it and the push connection wakes them. `storage.rescan_interval` is the backstop for
anything the watcher misses — watch limits, dropped events, or changes made
while Mirage was down.

Mirage must run as **root** to chown files to each user's uid/gid. It will
otherwise still work, but files land owned by the server process and are
unreachable over SMB. `mirage doctor` warns about this, and every affected
upload is logged.

## Running it

**[docs/deploy-synology.md](docs/deploy-synology.md) walks through the NAS
deployment**, including getting the image there without git and finding the
right uid/gid.

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

## License

[MIT](LICENSE).
