#!/usr/bin/env python3
"""Fixed-rate load without retries. Late/unsubmitted slots remain failed CSV rows."""
import argparse
import concurrent.futures
import csv
import json
import math
import time
import threading
from urllib.error import HTTPError, URLError
from urllib.request import Request, urlopen

FIELDS = ['time', 'status', 'seconds', 'ok', 'version']


def request(url, timeout, version=False):
    started = time.monotonic()
    row = dict(time=time.time(), status=0, seconds=0, ok=0, version='')
    req = Request(url) if version else Request(url, data=b'{"item":"lab6-tea"}', headers={'Content-Type': 'application/json'})
    try:
        with urlopen(req, timeout=timeout) as response:
            body = response.read().decode()
            row['status'] = response.status
            if version:
                row['version'] = body.strip()
                row['ok'] = int(response.status == 200 and row['version'] in ('v1', 'v2'))
            else:
                row['ok'] = int(response.status == 201)
    except HTTPError as error:
        row['status'] = error.code
        error.close()
    except (URLError, OSError, TimeoutError):
        pass
    row['seconds'] = time.monotonic() - started
    return row


def summary(rows):
    durations = sorted(float(r['seconds']) for r in rows if int(r['status']) != 0)
    def percentile(p):
        return durations[max(0, math.ceil(len(durations)*p)-1)] if durations else None
    return dict(requests=len(rows), errors=sum(not int(r['ok']) for r in rows),
                p50=percentile(.5), p95=percentile(.95),
                v1=sum(r['version'] == 'v1' and int(r['ok']) == 1 for r in rows),
                v2=sum(r['version'] == 'v2' and int(r['ok']) == 1 for r in rows))


def run(url, rate, duration, output, timeout):
    rows, pending = [], []
    capacity = threading.BoundedSemaphore(32)
    def send():
        try:
            return request(url, timeout)
        finally:
            capacity.release()
    start = time.monotonic()
    with concurrent.futures.ThreadPoolExecutor(max_workers=32) as pool:
        for slot in range(math.ceil(rate * duration)):
            due = start + slot / rate
            time.sleep(max(0, due - time.monotonic()))
            if time.monotonic() - due > 1 / rate or not capacity.acquire(blocking=False):
                rows.append(dict(time=time.time(), status=0, seconds=0, ok=0, version=''))
            else:
                pending.append(pool.submit(send))
        rows.extend(f.result() for f in pending)
    rows.sort(key=lambda row: row['time'])
    with open(output, 'w') as f:
        writer = csv.DictWriter(f, fieldnames=FIELDS)
        writer.writeheader()
        writer.writerows(rows)
    print(json.dumps(summary(rows)), flush=True)
    return rows


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--url', required=True)
    parser.add_argument('--rate', type=float, default=10)
    parser.add_argument('--duration', type=float, default=180)
    parser.add_argument('--timeout', type=float, default=5)
    parser.add_argument('--output', required=True)
    parser.add_argument('--versions', type=int)
    args = parser.parse_args()
    if args.rate <= 0 or args.duration <= 0 or args.timeout <= 0 or (args.versions is not None and args.versions <= 0):
        parser.error('rate, duration, timeout and versions must be positive')
    if args.versions:
        rows = [request(args.url.rstrip('/') + '/version', args.timeout, version=True) for _ in range(args.versions)]
        with open(args.output, 'w') as f:
            writer = csv.DictWriter(f, fieldnames=FIELDS)
            writer.writeheader()
            writer.writerows(rows)
        print(json.dumps(summary(rows)), flush=True)
    else:
        run(args.url.rstrip('/') + '/order', args.rate, args.duration, args.output, args.timeout)

if __name__ == '__main__':
    main()
