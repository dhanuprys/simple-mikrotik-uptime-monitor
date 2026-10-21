# MikroTik Monitor

A tiny heartbeat monitor for a router you cannot reach from outside.
The **router calls the server** once a minute; the server records it and works
out when the router was down and why.

- One static Go binary (~7 MB), standard library only, no dependencies
- Storage: one plain text file per ISO week (`/data/2026-W43.log`), ~450 KB/week
- Dashboard: one embedded HTML page, no external files, no CDN, works offline
- Tells apart **power loss / reboot**, **internet or link down**, and **monitor itself was offline**

## 1. Run the server

Using Docker Compose:
```sh
docker compose up -d
```

Or using Docker CLI directly with the prebuilt GHCR image:
```sh
docker run -d --name mikrotik-monitor --restart unless-stopped \
  -p 8080:8080 \
  -v monitor-data:/data \
  -e TOKEN=put-a-long-random-string-here \
  -e TZ=Asia/Makassar \
  ghcr.io/dhanuprys/mikrotik-uptime-monitor:latest
```

Or build locally:
```sh
docker build -t mikrotik-monitor .
```

Open `http://SERVER:8080/` for the dashboard.

> Use a **named volume** (as above). If you prefer a bind mount (`-v /srv/monitor:/data`),
> run `chown 65534:65534 /srv/monitor` first, because the container runs as a non-root user.
> A wrong owner is reported with a clear message at startup.

## 2. Configure the MikroTik

Replace `SERVER`, `8080` and `YOUR_TOKEN`. Paste into a terminal:

```
# Periodic heartbeat (every minute)
/system script add name=monitor-hb policy=read,write,test source={
  :local up [/system resource get uptime]
  :do { /tool fetch url="http://SERVER:8080/hb?key=YOUR_TOKEN&up=$up" keep-result=no } on-error={}
}
/system scheduler add name=monitor-hb interval=1m start-time=startup policy=read,write,test on-event="/system script run monitor-hb"

# Boot notification (runs once at router startup with retry until WAN connects)
/system script add name=monitor-boot policy=read,write,test source={
  :local up [/system resource get uptime]
  :local retry 0
  :while ($retry < 12) do={
    :do {
      /tool fetch url="http://SERVER:8080/boot?key=YOUR_TOKEN&up=$up" keep-result=no
      :set retry 99
    } on-error={
      :set retry ($retry + 1)
      :delay 5s
    }
  }
}
/system scheduler add name=monitor-boot interval=0 start-time=startup policy=read,write,test on-event="/system script run monitor-boot"
```

Test the heartbeat once by hand: `/system script run monitor-hb`, the dashboard should show
**ONLINE** within a second. Test the boot script: `/system script run monitor-boot`, you should see a
**Router boot** marker and timestamp in the dashboard. The `up=` value (router uptime) lets the server
distinguish a power loss from an internet outage.

## 3. Settings (environment variables)

| Variable     | Default   | Meaning |
|--------------|-----------|---------|
| `TOKEN`      | *(empty)* | Secret the router must send as `key=`. **Set it.** |
| `TZ`         | `UTC` (`Asia/Makassar` in the Dockerfile) | Timezone for week boundaries, day rows and labels |
| `DOWN_AFTER` | `180`     | Seconds without a heartbeat before the router counts as down. Keep it at least 3x the heartbeat interval (minimum 90). |
| `PORT`       | `8080`    | Listen port |
| `DATA_DIR`   | `./data` (`/data` in Docker) | Where the weekly files live |
| `LOG_HB`     | *(off)*   | `1` = log every heartbeat and boot event (handy while testing) |

If you change the scheduler interval, change `DOWN_AFTER` to match (e.g. 5 min interval, `DOWN_AFTER=900`).

## How it decides what happened

Every heartbeat carries the router's uptime. When heartbeats stop and later resume:

| Router event / uptime after the gap | Verdict |
|-------------------------------------|---------|
| Boot notification (`/boot`) sent    | **Router boot** logged & outage confirmed as **Power loss / reboot** |
| Kept counting up (about previous uptime + gap) | **Internet / link down**, router never lost power |
| Reset to a small number             | **Power loss / reboot** |
| Not reported                        | **Down (cause unknown)** |
| Reset, but no heartbeat gap         | **Quick reboot** (a short power blip) |

The server also writes its own "I'm alive" tick every 30 s. A period with neither
ticks nor heartbeats means *the monitor* was down (container restart, host reboot),
and is shown as **Monitor offline** and excluded from the availability percentage.
It is never blamed on the router.

## Data, backup, API

- Back up by copying the `/data` folder. Files are plain text, append-only, and a half-written last line (power cut) is repaired automatically at the next start.
  Lines: `H,<unix>,<router uptime seconds>` = heartbeat, `B,<unix>,<router uptime seconds>` = boot event, `T,<unix>` = monitor tick.
- `GET /hb?key=TOKEN&up=<uptime>` - heartbeat (called periodically by the router)
- `GET /boot?key=TOKEN&up=<uptime>` - boot notification (called by the router at startup)
- `GET /api/week?w=2026-W43` - week data as JSON (omit `w` for the current week)
- `GET /api/status` - current state as JSON (includes `last_boot`), handy for other tools
- `GET /healthz` - 200 when healthy, 500 if the disk is failing (Docker uses this)

The router should reach the server over a path you trust. The token travels in the URL,
so if the route is not private, put the server behind HTTPS (and use `https://` in the script).
The dashboard itself has no login; restrict it at your proxy if needed.

## Develop

```sh
go test -race ./...
DATA_DIR=./data TOKEN=test go run .
curl "http://localhost:8080/boot?key=test&up=15s"
curl "http://localhost:8080/hb?key=test&up=1d02:03:04"
```
