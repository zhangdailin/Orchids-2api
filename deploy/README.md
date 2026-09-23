# Host deployment (us1)

What runs on the server, and which file in this directory owns each piece.

| Piece | Path on the host | Source in this repo |
| --- | --- | --- |
| Server binary | `/opt/orchids-2api/orchids-server` | built from `./cmd/server` by `.github/workflows/release.yml` |
| Config | `/opt/orchids-2api/config.json` (not in git) | `config.example.json` |
| systemd unit | `/etc/systemd/system/orchids-2api.service` | — (host-local) |
| Environment file | `/etc/orchids-2api.env` (`EnvironmentFile=`) | — (host-local, secrets) |
| Reverse proxy | `/etc/caddy/Caddyfile` | `deploy/Caddyfile` |
| Loopback guard | `/etc/orchids-guard.nft` + `orchids-3002-loopback.service` | `deploy/orchids-guard.nft`, `deploy/orchids-3002-loopback.service` |

## Deploy a build

```sh
# 1. build the released artifact (run from the repo root)
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags "-s -w" -o dist/orchids-server-linux-amd64 ./cmd/server
cd dist && sha256sum orchids-server-linux-amd64 > orchids-server-linux-amd64.sha256
printf '%s\n' "version=manual-$(date -u +%Y%m%d-%H%M%S)" "commit=$(git rev-parse --short HEAD)" \
  "built_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)" goos=linux goarch=amd64 \
  > orchids-server-linux-amd64.build-info.txt

# 2. upload and deploy (the script verifies the checksum, keeps the old binary,
#    restarts the service and rolls back when the health check fails)
rsync -a dist/orchids-server-linux-amd64* scripts/deploy-orchids.sh root@HOST:/root/release/
ssh root@HOST 'cd /root/release && bash deploy-orchids.sh \
  --artifact ./orchids-server-linux-amd64 \
  --checksum ./orchids-server-linux-amd64.sha256 \
  --build-info ./orchids-server-linux-amd64.build-info.txt'
```

## Host access

The wrappers in `.rsh/` run a command or copy files to the host:

```sh
./.rsh/run 'systemctl status orchids-2api'        # run from the repository root
./.rsh/scp.sh dist/orchids-server-linux-amd64* root@3.15.148.113:/root/release/
```

`.rsh/` is **not tracked**. It used to be, with the host's root password as a
literal default in `.rsh/run`, which put that password in the public repository
history — so the directory is now in `.gitignore` and the password is read from
the environment or from an untracked file:

- `RSH_PASS`, or
- `.rsh/pass` (mode 0600), used when `RSH_PASS` is unset.

Both wrappers refuse to run with neither set. Anything that needs to reach the
host in CI should use a deploy key, not this password.

## Bring up Caddy

```sh
install -m 0644 deploy/Caddyfile /etc/caddy/Caddyfile   # merge, do not clobber other sites
caddy validate --config /etc/caddy/Caddyfile
caddy fmt --overwrite /etc/caddy/Caddyfile
systemctl enable --now caddy
```

`us1.daige.tech` is Cloudflare-proxied, so the browser sees Cloudflare's edge
certificate while Caddy presents the Let's Encrypt certificate for the origin.
Caddy needs 80/443 reachable from the internet for ACME renewals; the access log
goes to `/var/log/caddy/access.log` (owned by the `caddy` user).

## Keep the backend off the public internet

`orchids-server` listens on `:3002` for every interface and has no bind-address
setting, so "only Caddy should expose the backend publicly" is enforced in the
packet filter instead:

```sh
install -m 0644 deploy/orchids-guard.nft /etc/orchids-guard.nft
install -m 0644 deploy/orchids-3002-loopback.service /etc/systemd/system/
systemctl daemon-reload && systemctl enable --now orchids-3002-loopback.service
nft list table inet orchids_guard      # iifname != "lo" tcp dport 3002 drop
```

The guard unit is ordered `Before=orchids-2api.service caddy.service`, so it is
in place before either can accept traffic. To remove it later:
`systemctl disable --now orchids-3002-loopback.service`.
