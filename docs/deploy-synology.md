# Deploying Mirage on a Synology NAS

The NAS needs no source tree and no git — only a container image and a config
file. There are two ways to get the image there. Read the first section either
way, because getting the architecture wrong is the most common way to end up
with a container that will not start.

## First: which architecture?

Over SSH on the NAS:

```
uname -m
```

| Output | Build for | Typical models |
|---|---|---|
| `x86_64` | `amd64` | most Synology models (DS220+, DS923+, DS1522+, …) |
| `aarch64` | `arm64` | some value models (DS223, DS124, …) |

An image built for the wrong architecture fails with `exec format error`, which
says nothing about the real cause. Your Mac is arm64 and most Synology models
are x86_64, so a plain `docker build` on the Mac produces an image the NAS
cannot run.

---

## Route A — pull a published image (recommended)

CI builds for both architectures on every push to `main` and publishes to the
GitHub Container Registry, so the NAS pulls whichever matches itself and updates
are a `docker compose pull` away.

### 1. Let CI publish

Push to `main`. The `build` workflow runs the tests, then publishes:

```
ghcr.io/poitch/mirage:latest
```

Check it under **Packages** on the GitHub repository page.

### 2. Decide whether the NAS needs credentials

The repository is private, so the package is private too and pulling it requires
a login. Two options:

**Make the package public** — simplest. On GitHub: *Packages → mirage → Package
settings → Change visibility → Public*. The repository stays private; only the
built image becomes public. It contains no configuration and no data, but do
consider that anyone can then download your build.

**Keep it private** — then the NAS must authenticate. Create a classic personal
access token with the `read:packages` scope, and on the NAS:

```
sudo docker login ghcr.io -u poitch
# paste the token as the password
```

In Container Manager the equivalent is *Registry → Settings → Add*, with
`https://ghcr.io` and the same credentials.

### 3. Continue to "Preparing the NAS" below.

---

## Route B — build it yourself and copy it across

Useful when you would rather not involve a registry at all. You have a Debian
server, which makes this straightforward — and if that machine is x86_64, it
matches most Synology models directly.

On the Debian box (or your Mac; `buildx` cross-compiles either way):

```
scripts/export-image.sh amd64       # or arm64, per `uname -m` on the NAS
```

That writes `mirage-amd64.tar.gz`, around 10 MB. Copy it to the NAS and load it:

```
scp mirage-amd64.tar.gz you@nas:/volume1/docker/mirage/
ssh you@nas 'sudo docker load < /volume1/docker/mirage/mirage-amd64.tar.gz'
```

In Container Manager the equivalent is *Image → Add → Add From File*.

Then change the image line in your compose file to `image: mirage:latest` and
drop the `pull_policy: always` line — otherwise Docker tries to pull from
ghcr.io and ignores the image you just loaded.

You can also skip the file entirely and push straight from the Debian box:

```
docker buildx build --platform linux/amd64,linux/arm64 \
    -f deploy/Dockerfile -t ghcr.io/poitch/mirage:latest --push .
```

---

## Preparing the NAS

### 1. Enable user home directories

In DSM: *Control Panel → User & Group → Advanced → Enable user home service*.
This is what creates `/volume1/homes/<user>`, which Mirage maps onto.

### 2. Find each user's uid and gid

Over SSH:

```
id alice
# uid=1026(alice) gid=100(users) groups=100(users)
```

These go into the config. Getting them wrong means files Mirage writes are
unreadable over SMB — `mirage doctor` checks for exactly this.

### 3. Create the directories

```
sudo mkdir -p /volume1/docker/mirage/{config,data}
```

### 4. Write the config

Copy `deploy/mirage.example.yaml` to
`/volume1/docker/mirage/config/mirage.yaml` and edit it. It does not list
accounts — those are added from the admin page once the server is up.

`external_url` must be the address clients actually reach — it is baked into the
pairing URLs handed to them, so a wrong value pairs and then hangs.

Then set an admin password in a `.env` file beside the compose file:

```
MIRAGE_ADMIN_USERNAME=admin
MIRAGE_ADMIN_PASSWORD=something-long-and-random
```

The compose file refuses to start without it. Choose something real: this page
can repoint any account at any directory on the NAS.

### 5. Start it

Copy `deploy/docker-compose.yml` to the NAS. In Container Manager: *Project →
Create*, point it at the folder holding the compose file. Or over SSH:

```
cd /volume1/docker/mirage && sudo docker compose up -d
```

For the Tailscale variant, use `deploy/docker-compose.tailscale.yml` instead and
follow the setup notes at the top of that file. Two things to know before you
start it:

The URL is **https**, not http. `tailscale serve` terminates TLS with a
certificate Tailscale issues for the ts.net name, so `external_url` must read
`https://mirage.<your-tailnet>.ts.net`. That value is baked into the pairing
URLs handed to clients, so a mismatch pairs and then hangs.

Auth keys expire, and so do the nodes authenticated with one. Unless you turn
off key expiry for the node — or run it tagged — the NAS quietly leaves the
tailnet months later and every client stops syncing with nothing obviously
wrong. The compose file explains both fixes, including the policy-file entry a
tag needs before an auth key can be issued for it.

### 6. Check it

Open `/admin` on your `external_url` and sign in. Add an account for each
person: a username, the directory backing it, and the uid/gid from step 2. The
page checks the directory as you save it and says so if it is missing, not
writable, or owned by somebody else — the mapping mistakes that otherwise
surface as confusing permission errors mid-sync.

Set a password on each account before pointing a client at it; a fresh account
has none and cannot sign in.

The same checks are available from the command line:

```
sudo docker compose exec mirage mirage doctor
```

### 7. Watch the first startup

```
sudo docker compose logs -f mirage
```

Two lines are worth looking for:

`ran out of filesystem watches` — Synology's default
`fs.inotify.max_user_watches` is low and a large tree will exhaust it. Sync stays
correct, because the periodic rescan is the backstop, but changes made outside
Mirage then take up to `rescan_interval` to appear rather than about a second.
Raise it on the host, or lower `rescan_interval`.

`could not set file ownership` — Mirage is not running as root, so files are
landing owned by the server process and will not be reachable over SMB. The
supplied compose files run as root with only the capabilities this needs.

---

## Updating

Route A:

```
sudo docker compose pull && sudo docker compose up -d
```

Route B: build and load the new image, then `sudo docker compose up -d`.

The index database survives upgrades and migrates itself on start. Losing it
costs a rescan, not data — the files are ordinary files on the NAS.
