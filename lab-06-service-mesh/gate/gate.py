#!/usr/bin/env python3
"""Canary gate. Only traffic.v2Weight changes; all writes target kind-lab6."""
import argparse
import json
import math
import subprocess
import sys
import time
from urllib.parse import urlencode
from urllib.request import urlopen

STEPS = (10, 25, 50, 75, 100)
CONTEXT = 'kind-lab6'
SELECTOR = 'namespace="shop",service="api-v2",version="v2"'
ORDER = SELECTOR + ',path="/order"'


def decide(weight, stats, healthy_checks):
    if stats is None or any(stats.get(k) is None for k in ('requests', 'age_seconds', 'targets_up')):
        return weight, 0, 'no-data'
    for key in ('requests', 'age_seconds', 'targets_up'):
        if not math.isfinite(stats[key]) or stats[key] < 0:
            raise ValueError('invalid ' + key)
    if stats['requests'] < 50 or stats['age_seconds'] > 30 or stats['targets_up'] != 1:
        return weight, 0, 'no-data'
    if any(stats.get(k) is None for k in ('error_rate', 'p95_seconds')):
        return weight, 0, 'no-data'
    for key in ('error_rate', 'p95_seconds'):
        if not math.isfinite(stats[key]) or stats[key] < 0:
            raise ValueError('invalid ' + key)
    if stats['error_rate'] > 1:
        raise ValueError('error_rate outside 0..1')
    if stats['error_rate'] > .05 or stats['p95_seconds'] > .6:
        return 0, 0, 'rollback'
    if stats['error_rate'] < .01 and stats['p95_seconds'] <= .3:
        healthy_checks += 1
        if healthy_checks >= 2:
            if weight not in STEPS[:-1]:
                raise ValueError('unexpected promotion weight')
            return STEPS[STEPS.index(weight) + 1], 0, 'promote'
        return weight, healthy_checks, 'healthy'
    return weight, 0, 'hold'


def query(address, expression):
    with urlopen(address.rstrip('/') + '/api/v1/query?' + urlencode({'query': expression}), timeout=10) as response:
        body = json.loads(response.read())
    if body.get('status') != 'success' or body.get('data', {}).get('resultType') != 'vector':
        raise ValueError('invalid Prometheus response')
    result = body['data']['result']
    if not result:
        return None
    if len(result) != 1:
        raise ValueError('expected one aggregated series')
    try:
        return float(result[0]['value'][1])
    except (KeyError, TypeError, IndexError) as error:
        raise ValueError('invalid Prometheus sample') from error


def read_stats(address):
    total = 'sum(increase(shop_http_requests_total{' + ORDER + '}[60s]))'
    errors = 'sum(increase(shop_http_requests_total{' + ORDER + ',code=~"5.."}[60s]))'
    up = 'up{' + SELECTOR + '}'
    expressions = {
        'requests': total,
        'error_rate': '(' + errors + ' or vector(0)) / ' + total,
        'p95_seconds': 'histogram_quantile(0.95, sum by(le)(rate(shop_http_request_duration_seconds_bucket{' + ORDER + '}[60s])))',
        'age_seconds': 'time() - min(timestamp(' + up + '))',
        'targets_up': 'min(' + up + ') * (count(' + up + ') == bool sum(kube_deployment_spec_replicas{namespace="shop",deployment="api-v2"}))',
    }
    return {key: query(address, expr) for key, expr in expressions.items()}


def current_weight():
    result = subprocess.run(['kubectl', '--context', CONTEXT, '-n', 'shop', 'get', 'httproute', 'api', '-o', 'json'], check=True, text=True, capture_output=True, timeout=30)
    route = json.loads(result.stdout)
    backends = route['spec']['rules'][0]['backendRefs']
    weights = {b['name']: b['weight'] for b in backends}
    if set(weights) != {'api-v1', 'api-v2'} or sum(weights.values()) != 100:
        raise ValueError('unexpected route')
    return int(weights['api-v2'])


def apply_weight(chart, weight):
    subprocess.run(['helm', '--kube-context', CONTEXT, 'upgrade', 'shop', chart,
                    '-n', 'shop', '--reuse-values', '--set', 'traffic.v2Weight=' + str(weight),
                    '--wait', '--timeout', '120s'], check=True, text=True, capture_output=True, timeout=150)
    if current_weight() != weight:
        raise RuntimeError('HTTPRoute did not apply requested weight')


def run(address, chart):
    weight = current_weight()
    healthy = 0
    changed = time.monotonic()
    missing_since = None
    while weight not in (0, 100):
        stats = None
        if time.monotonic() - changed < 60:
            next_weight, healthy, reason = weight, 0, 'new-window'
        else:
            stats = read_stats(address)
            next_weight, healthy, reason = decide(weight, stats, healthy)
            if reason == 'no-data':
                if missing_since is None:
                    missing_since = time.monotonic()
                if time.monotonic() - missing_since >= 120:
                    raise RuntimeError('no usable metrics for 120 seconds')
            else:
                missing_since = None
        old = weight
        if next_weight != weight:
            apply_weight(chart, next_weight)
            weight, changed = next_weight, time.monotonic()
        print(json.dumps(dict(time=time.time(), stats=stats, old=old, new=weight, reason=reason)), flush=True)
        if weight not in (0, 100):
            time.sleep(15)
    return weight


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--prometheus', default='http://127.0.0.1:19090')
    parser.add_argument('--chart', default='./shop')
    args = parser.parse_args()
    try:
        run(args.prometheus, args.chart)
    except (OSError, ValueError, KeyError, RuntimeError, subprocess.SubprocessError) as error:
        print('gate failed: ' + str(error), file=sys.stderr)
        return 1
    return 0

if __name__ == '__main__':
    sys.exit(main())
