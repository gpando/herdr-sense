# herdr-sense

A tiny MQTT subscriber for a Raspberry Pi with a Sense HAT. It listens to the
topic `herdr/agent/status` (plain-text payload: `working` | `done` | `idle` |
`blocked`) and lights the 8x8 LED matrix with a solid color per status.

The MQTT broker (mosquitto) runs on a **different machine** — the host running
the herdr-telegram plugin — so the broker address is user-provided.

## Requirements

- Raspberry Pi with a Sense HAT (framebuffer driver loaded, `RPi-Sense FB`)
- An MQTT broker reachable over the LAN (mosquitto on the plugin host)
- Cross-compiled from any machine with Go — the Pi needs nothing but the binary

### Broker configuration (mosquitto 2.x)

mosquitto 2.x only listens on `localhost` by default. For LAN access, add to
`mosquitto.conf` on the plugin host:

```
listener 1883 0.0.0.0
allow_anonymous true
```

**Caveat:** this trusts the whole LAN — anyone on the network can publish to
the topic. Only do this on a trusted home/lab network.

## Build

Cross-compile from any machine (pure Go, no CGO needed):

```sh
# 64-bit OS (Raspberry Pi OS 64-bit)
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -ldflags="-s -w" -o herdr-sense

# 32-bit OS
GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -ldflags="-s -w" -o herdr-sense
```

## Deploy

Pipe the binary over SSH:

```sh
cat herdr-sense | ssh user@pi "cat > ~/herdr-sense && chmod +x ~/herdr-sense"
```

Then move it to a system path (as required by the systemd unit):

```sh
ssh user@pi "sudo mv ~/herdr-sense /usr/local/bin/herdr-sense"
```

## Run

The framebuffer requires elevated permission — run as root, or as a user in
the `video` group:

```sh
sudo herdr-sense -broker=192.168.1.10:1883
```

On startup the matrix shows a very dim gray until the first status arrives
(retained messages are delivered on subscribe, so this is usually instant).

## Flags

| Flag          | Default                | Description                                    |
| ------------- | ---------------------- | ---------------------------------------------- |
| `-broker`     | `localhost:1883`       | MQTT broker as `host:port` (dialled via `tcp://`) |
| `-topic`      | `herdr/agent/status`   | MQTT topic carrying the agent status           |
| `-client-id`  | `herdr-sense`          | MQTT client ID                                 |

## Status → color mapping

| Payload   | Color  | RGB           |
| --------- | ------ | ------------- |
| `working` | green  | (0, 200, 0)   |
| `done`    | blue   | (0, 102, 255) |
| `idle`    | amber  | (255, 140, 0) |
| `blocked` | red    | (220, 0, 0)   |
| (startup) | dim gray | (16, 16, 16) |
| (connection lost) | dim gray | (48, 48, 48) |

Unknown payloads are logged and ignored (the current color is kept).

## systemd service

Edit the `-broker=` value in `deploy/herdr-sense.service`, then on the Pi:

```sh
sudo cp deploy/herdr-sense.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now herdr-sense
journalctl -u herdr-sense -f
```
