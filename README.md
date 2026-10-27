# fruit-fs 🍊🍌

A small, fruity, self-hosted file browser and device-to-device drop. One Go binary with the web app built in, shipped as a ~10 MB container.

- **Files:** browse, upload (resumable, folders too), download, zip whole folders, rename, move, search, preview photos, video, audio, PDFs and text, and a 30-day trash.
- **Drop:** open fruit-fs on two devices and send files or notes between them, AirDrop style. Files stream straight through and are never stored, unless the receiver picks "save to fruit-fs".
- **Two ways in:** behind [Tailscale Serve](https://tailscale.com/kb/1312/serve) you're signed in automatically from your tailnet identity; on the local network you sign in with a password.

New to all this? [docs/walkthrough.md](docs/walkthrough.md) explains how it works in plain English, commit by commit.

## Run it

```bash
cp .env.example .env            # then edit it
mkdir -p data state && sudo chown 65532:65532 data state
docker compose up -d
```

Make the password hash for `.env` (keep the single quotes around it there):

```bash
docker run --rm -i ghcr.io/zelder-bit/fruit-fs hash-password
```

### Settings

| Variable | Default | Meaning |
| --- | --- | --- |
| `FRUITFS_DATA` | `/data` | Folder that is served |
| `FRUITFS_STATE` | `/state` | Private state (session key). Must not be inside `FRUITFS_DATA` |
| `FRUITFS_LISTEN` | `:8080` | Listen address |
| `FRUITFS_TRUSTED_PROXIES` | | Addresses or CIDRs allowed to vouch for a user with `Tailscale-User-Login` (where `tailscale serve` runs) |
| `FRUITFS_ALLOWED_USERS` | | Optional comma-separated tailnet logins; empty allows the whole tailnet |
| `FRUITFS_PASSWORD_HASH` | | argon2id hash for LAN sign-in; empty turns password sign-in off |
| `FRUITFS_MAX_UPLOAD` | | Largest file, e.g. `20G`; empty means no limit |

At least one of `FRUITFS_TRUSTED_PROXIES` or `FRUITFS_PASSWORD_HASH` must be set.

### Behind Tailscale Serve

```bash
sudo tailscale serve --bg --https=8444 http://<fruit-fs host>:8080
```

Set `FRUITFS_TRUSTED_PROXIES` to the address fruit-fs sees those requests come from: the machine running `tailscale serve`. Tailscale identity headers from any other address are ignored. The container uses host networking so it sees real client addresses.

## Security notes

- Every file operation goes through Go's `os.Root`, so paths and symlinks can't leave the data folder.
- State-changing requests need an `X-Fruit: 1` header plus a matching `Origin`, and cookies are `SameSite=Strict`.
- Uploaded files are only ever shown inline as images, video, audio, PDF or plain text, with a sandbox CSP; anything else downloads.
- Password sign-in uses argon2id, a per-address attempt limit, and at most two hash checks at once.
- On a plain-HTTP LAN the password travels unencrypted; use the Tailscale address when you can.
- Deleted files and unfinished uploads live in `.fruit-fs/` inside the data folder (hidden from the app).

## Develop

Go doesn't need to be installed; `scripts/go.sh` runs the toolchain in Docker.

```bash
scripts/go.sh test -race ./...
scripts/go.sh build -o /src/fruit-fs ./cmd/fruit-fs
FRUITFS_DATA=./data FRUITFS_STATE=./state FRUITFS_PASSWORD_HASH='…' ./fruit-fs
```

The frontend in `web/static/` is plain ES modules and CSS with no build step. Before committing, `scripts/check-public.sh` (installed as the pre-commit hook with `ln -sf ../../scripts/check-public.sh .git/hooks/pre-commit`) checks for private addresses, emails and keys.

## License

MIT. Fonts: Fredoka and Nunito, SIL Open Font License (see `web/static/fonts/`).
