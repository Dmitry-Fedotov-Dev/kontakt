# Kontakt
[Читать на русском](README.md)
<p align="center">
  <img width="427" height="900" alt="Kontakt Screenshot" src="https://github.com/user-attachments/assets/ee221aa9-4ef8-4bf3-8cf6-3c27cf632ed8" />
</p>

Pick up the phone and start talking to a random stranger.

## Key Features

- **Browser Protocol:** Runs directly in the browser via WebSocket ([RFC 7118](https://datatracker.ietf.org/doc/html/rfc7118)). Connect seamlessly from any browser-enabled device.
- **Microservice Architecture:** Powered by three Go services: **web**, **signal**, and **media** (managed via gRPC).
- **Privacy & Security:** Voice data is never stored anywhere. Users have the ability to permanently ban a peer via cookies (hardware-based bans planned for future updates).
- **High Efficiency:** 100 channels (50 simultaneous calls) consume as little as 0.22 CPU cores and 43 MB RAM. See [LOAD_REPORT.md](LOAD_REPORT.md) for benchmark details.
- **Bandwidth:** a channel costs about 69 kbps per direction (G.711 + RTP, measured from `kontakt_media_bytes_*_total`; about 80 kbps with IP/UDP headers). Capacity is **measured, not assumed**: Cloudflare Tunnel has no official Mbps limit — it depends on traffic, sessions and the host — so it comes from load tests (`k6/load.js`) and metrics. Quick Tunnel (`*.trycloudflare.com`) is for development and demos only: no uptime guarantee, 200 in-flight HTTP requests, a temporary hostname.

---

## Quick Start Guide

### Running on Mobile (Android / Termux)

#### 1. Termux Setup
Grant Termux storage permissions (you can leave other optional permissions disabled if prompted):

```bash
termux-setup-storage
```

#### 2. Installation & Run
Unzip the project archive into your home directory:

```bash
unzip ~/storage/downloads/kontakt.zip
```

Install required packages and run the service in your **first Termux tab/session**:

```bash
pkg install golang git cloudflared
./scripts/run.sh
```

In a **second Termux tab/session**, launch the Cloudflare tunnel:

```bash
./scripts/tunnel.sh quick
```

---

### Running on Windows Machines

Open a terminal inside the project directory:

**1. First Session:**
```bash
./scripts/run.sh
```

**2. Second Session:**
```bash
./scripts/tunnel.sh quick
```

---

### Accessing the App

Once both scripts are running, look for the public URL generated in the terminal output following this line:

```text
Your quick Tunnel has been created! Visit it at (it may take some time to be reachable):
```

## Open Radio (prototype)

A separate program on the same base: anyone can take a free frequency (87.5–108.0) and
broadcast, anyone can turn the dial and listen. The host queues mp3 files and switches the
microphone on (music ducks under the voice); between stations the listener hears static,
beat whistles and crackle.

```bash
./scripts/radio.sh            # http://localhost:27620 (or the next free port)
TUNNEL=1 ./scripts/radio.sh   # plus a temporary https://….trycloudflare.com address
```

The host's browser encodes G.711 μ-law (8 kHz, 64 kbit/s); the server only relays ready
160-byte frames to listeners of that frequency — no decoding or mixing. mp3 files are never
uploaded. Limits: `-max-stations`, `-max-listeners`; host stream capped at 64 kbit/s.
