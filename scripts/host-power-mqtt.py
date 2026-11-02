#!/usr/bin/env python3
"""Publish a host's CPU (and NVIDIA GPU, if any) power to the nats-mqtt leaf, with HA discovery.

Usage: host-power-mqtt.py <mqtt-user> <password-file>    (as root: RAPL is root-only)
"""
import json
import socket
import ssl
import struct
import subprocess
import sys
import time

HOST = "mqtt.skaal.dev"
PORT = 8883
RAPL = "/sys/class/powercap/intel-rapl:0"
SAMPLE_SECONDS = 30

SENSORS = [
    ("cpu_power", "CPU power", "W", "power"),
    ("gpu_power", "GPU power", "W", "power"),
    ("gpu_pstate", "GPU P-state", None, None),
]


def cpu_power():
    def read(name):
        with open(f"{RAPL}/{name}") as f:
            return int(f.read())

    a = read("energy_uj")
    time.sleep(SAMPLE_SECONDS)
    b = read("energy_uj")
    if b < a:
        b += read("max_energy_range_uj")
    return round((b - a) / SAMPLE_SECONDS / 1e6, 1)


def gpu():
    try:
        out = subprocess.run(
            ["nvidia-smi", "--query-gpu=power.draw,pstate", "--format=csv,noheader,nounits"],
            capture_output=True, text=True, check=True, timeout=20,
        ).stdout
    except (OSError, subprocess.SubprocessError):
        return {}
    power, pstate = (x.strip() for x in out.splitlines()[0].split(","))
    return {"gpu_power": round(float(power), 1), "gpu_pstate": pstate}


def utf8(s):
    b = s.encode()
    return struct.pack(">H", len(b)) + b


def packet(header, body):
    n, length = len(body), b""
    while True:
        n, digit = divmod(n, 128)
        length += bytes([digit | (0x80 if n else 0)])
        if not n:
            return bytes([header]) + length + body


def publish(sock, topic, payload):
    sock.sendall(packet(0x31, utf8(topic) + json.dumps(payload).encode()))


def main():
    user, password_file = sys.argv[1:3]
    with open(password_file) as f:
        password = f.read().strip()
    state_topic = f"{user}/power"
    state = {"cpu_power": cpu_power(), **gpu()}

    ctx = ssl.create_default_context()
    with socket.create_connection((HOST, PORT), timeout=15) as raw:
        with ctx.wrap_socket(raw, server_hostname=HOST) as sock:
            # Level 4 is MQTT 3.1.1, the only version nats-server accepts.
            sock.sendall(packet(0x10, utf8("MQTT") + bytes([4, 0xC2]) + struct.pack(">H", 60)
                                + utf8(f"{user}-power") + utf8(user) + utf8(password)))
            ack = sock.recv(4)
            if len(ack) != 4 or ack[3] != 0:
                sys.exit(f"MQTT CONNECT refused: {ack!r}")

            for key, name, unit, device_class in SENSORS:
                if key not in state:
                    continue
                config = {
                    "name": name,
                    "unique_id": f"{user}_{key}",
                    "state_topic": state_topic,
                    "value_template": f"{{{{ value_json.{key} }}}}",
                    "expire_after": 300,
                    "device": {"identifiers": [user], "name": socket.gethostname()},
                }
                if unit:
                    config |= {"unit_of_measurement": unit, "device_class": device_class,
                               "state_class": "measurement"}
                publish(sock, f"homeassistant/sensor/{user}/{key}/config", config)

            publish(sock, state_topic, state)
            sock.sendall(b"\xe0\x00")


if __name__ == "__main__":
    main()
