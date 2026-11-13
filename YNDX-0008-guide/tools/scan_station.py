#!/usr/bin/env python3
"""Найти станцию в локальной сети по открытым портам 5555 (ADB) / 2222 (SSH).

Пример:
    py scan_station.py 192.168.0.0/24
    py scan_station.py 192.168.1.0/24 --ports 5555,2222
"""
import argparse
import ipaddress
import socket
from concurrent.futures import ThreadPoolExecutor


def probe(ip, ports):
    hits = []
    for port in ports:
        s = socket.socket()
        s.settimeout(0.35)
        try:
            if s.connect_ex((str(ip), port)) == 0:
                hits.append(port)
        finally:
            s.close()
    return (str(ip), hits) if hits else None


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("subnet", nargs="?", default="192.168.0.0/24")
    ap.add_argument("--ports", default="5555,2222")
    args = ap.parse_args()
    ports = [int(p) for p in args.ports.split(",") if p.strip()]
    net = ipaddress.ip_network(args.subnet, strict=False)
    ips = list(net.hosts())
    found = 0
    with ThreadPoolExecutor(max_workers=128) as ex:
        for res in ex.map(lambda ip: probe(ip, ports), ips):
            if res:
                found += 1
                print(res[0], res[1], flush=True)
    if not found:
        print("ничего не найдено; проверьте подсеть и что станция в той же сети")


if __name__ == "__main__":
    main()
