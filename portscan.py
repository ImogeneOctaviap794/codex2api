#!/usr/bin/env python3
"""快速端口扫描器 - 扫描目标主机常见端口"""

import socket
import concurrent.futures
import sys
import time

TARGET = "43.155.82.249"
# 常见端口列表
COMMON_PORTS = [
    20, 21, 22, 23, 25, 53, 80, 110, 111, 135, 139, 143, 443, 445,
    993, 995, 1433, 1521, 2049, 2181, 3000, 3306, 3389, 4443, 5000,
    5432, 5672, 5900, 6379, 6443, 7001, 7443, 8000, 8008, 8080, 8081,
    8443, 8888, 9000, 9090, 9200, 9300, 9443, 10000, 15672, 27017,
    28017, 50000, 50070,
]
TIMEOUT = 2  # 秒
MAX_WORKERS = 50


def scan_port(host: str, port: int) -> tuple[int, bool, str]:
    try:
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
            s.settimeout(TIMEOUT)
            result = s.connect_ex((host, port))
            if result == 0:
                try:
                    banner = s.recv(1024).decode(errors="ignore").strip()
                except Exception:
                    banner = ""
                return port, True, banner
    except Exception:
        pass
    return port, False, ""


def main():
    host = sys.argv[1] if len(sys.argv) > 1 else TARGET
    print(f"🔍 扫描目标: {host}")
    print(f"   端口数量: {len(COMMON_PORTS)}, 超时: {TIMEOUT}s, 并发: {MAX_WORKERS}")
    print("-" * 50)

    start = time.time()
    open_ports = []

    with concurrent.futures.ThreadPoolExecutor(max_workers=MAX_WORKERS) as pool:
        futures = {pool.submit(scan_port, host, p): p for p in COMMON_PORTS}
        for future in concurrent.futures.as_completed(futures):
            port, is_open, banner = future.result()
            if is_open:
                info = f"  ({banner[:60]})" if banner else ""
                print(f"  ✅ {port:>5}/tcp  OPEN{info}")
                open_ports.append((port, banner))

    elapsed = time.time() - start
    print("-" * 50)
    if open_ports:
        print(f"共发现 {len(open_ports)} 个开放端口:")
        for p, b in sorted(open_ports):
            info = f" - {b[:80]}" if b else ""
            print(f"  {p}/tcp{info}")
    else:
        print("未发现开放端口。")
    print(f"耗时: {elapsed:.1f}s")


if __name__ == "__main__":
    main()
