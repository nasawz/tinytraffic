# Deploying without Docker

One Linux server, one binary, nginx in front, Let's Encrypt for HTTPS.
These are the files used for the production install.

| File | Install to |
|---|---|
| `tinytraffic.service` | `/etc/systemd/system/tinytraffic.service` |
| `nginx.conf` | `/etc/nginx/conf.d/tinytraffic.conf` (replace the domain) |
| `renew-cert.sh` | `/usr/local/bin/tinytraffic-renew-cert` (`chmod 755`) |
| `tinytraffic-renew.service`, `.timer` | `/etc/systemd/system/` |

## 1. Build

DuckDB needs cgo, so build on a machine with the target's OS (the server
itself works). The dashboard must be built first, because it is embedded:

```sh
bun install && bun run build      # tracker + dashboard + bin/tinytraffic
```

On a server without bun, build the dashboard elsewhere and copy `web/dist`
and `web/static` over, then `CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o tinytraffic ./cmd/tinytraffic`.

## 2. Run

```sh
useradd --system --home-dir /opt/tinytraffic --shell /sbin/nologin tinytraffic
mkdir -p /opt/tinytraffic/{bin,data,geoip}
install -m 755 tinytraffic /opt/tinytraffic/bin/
cp node_modules/@ip-location-db/dbip-city-mmdb/*.mmdb /opt/tinytraffic/geoip/   # optional, for locations
chown -R tinytraffic: /opt/tinytraffic
```

`/etc/tinytraffic.env` (mode 600):

```
TT_ADDR=127.0.0.1:8080
TT_DATA_DIR=/opt/tinytraffic/data
TT_TRUST_PROXY=true
TT_MEMORY_LIMIT=512MB
TT_GEOIP_DB=/opt/tinytraffic/geoip/dbip-city-ipv4.mmdb,/opt/tinytraffic/geoip/dbip-city-ipv6.mmdb
TT_PASSWORD=choose-a-long-random-password
```

```sh
systemctl enable --now tinytraffic
```

## 3. HTTPS

Open TCP **80 and 443** in the cloud provider's firewall or security group
(the server's own firewall is not enough). Then:

```sh
dnf install -y certbot            # or apt install certbot
mkdir -p /var/www/acme
# a first nginx config with only the port-80 server and the acme-challenge location
certbot certonly --webroot -w /var/www/acme -d tinytraffic.example.com \
  --non-interactive --agree-tos --register-unsafely-without-email
# now install the full nginx.conf and reload
nginx -t && systemctl reload nginx
```

## 4. Renewal

`renew-cert.sh` runs `certbot renew` (which only acts within 30 days of
expiry), reloads nginx only when a certificate was issued, logs to
`/var/log/tinytraffic-renew.log` and the journal, and exits non-zero if the
certificate has under 14 days left, so a broken renewal shows up as a failed
unit.

```sh
systemctl enable --now tinytraffic-renew.timer     # 03:xx and 15:xx daily
tinytraffic-renew-cert --dry-run                   # rehearse against staging
tinytraffic-renew-cert --force                     # really renew now
systemctl list-timers tinytraffic-renew.timer
journalctl -u tinytraffic-renew
```

## Updating

Upload the new source (or binary), rebuild, then
`systemctl restart tinytraffic`. The data in `/opt/tinytraffic/data` is kept.
