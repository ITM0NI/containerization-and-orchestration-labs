#!/usr/bin/env python3
"""Проверить правила из Kubernetes через promtool в одноразовом контейнере того же образа."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile


def get(kind, name=None, selector=None):
    command = ['kubectl', '-n', 'monitoring' if kind != 'prometheusrule' else 'shop', 'get', kind]
    if name:
        command.append(name)
    if selector:
        command += ['-l', selector]
    return json.loads(subprocess.check_output(command + ['-o', 'json'], text=True))


def main():
    rules = get('prometheusrule', 'shop-network')['spec']['groups'][0]['rules']
    pod = get('pods', selector='prometheus=monitoring-prometheus')['items'][0]
    container = next(c for c in pod['spec']['containers'] if c['name'] == 'prometheus')
    up = [
        {'series': f'up{{namespace="kube-system",service="hubble-metrics",instance="node-{n}"}}', 'values': '1+0x20'}
        for n in ('a', 'b')
    ]
    nodes = [{'series': f'kube_node_info{{node="{n}"}}', 'values': '1+0x20'} for n in ('a', 'b')]

    def counter(series):
        return {'series': series, 'values': '0+5x20'}

    cases = [
        ('normal', up, [], None),
        ('normal DNS search-list NXDOMAIN', up, [counter('hubble_dns_responses_total{destination_namespace="shop",rcode="No Error"}'), counter('hubble_dns_responses_total{destination_namespace="shop",rcode="Non-Existent Domain"}')], None),
        ('outsider drop is expected', up, [counter('hubble_drop_total{source_namespace="shop",source="shop/outsider",destination="shop/postgres-1",reason="POLICY_DENIED"}')], None),
        ('critical database path blocked', up, [counter('hubble_drop_total{source_namespace="shop",source="shop/api-test",destination="shop/postgres-1",reason="POLICY_DENIED"}')], 'ShopCriticalPathPolicyDrop'),
        ('DNS response error', up, [counter('hubble_dns_responses_total{destination_namespace="shop",rcode="Refused"}')], 'ShopDNSErrors'),
        ('DNS policy drop', up, [counter('hubble_drop_total{source_namespace="shop",source="shop/client",destination="kube-system/coredns-test",reason="POLICY_DENIED"}')], 'ShopDNSErrors'),
        ('one target scrape failed', [dict(up[0], values='0+0x20'), up[1]], [], 'ShopHubbleMetricsUnavailable'),
        ('one target disappeared', up[:1], [], 'ShopHubbleMetricsUnavailable'),
        ('all targets disappeared', [], [], 'ShopHubbleMetricsUnavailable'),
    ]
    tests = []
    for name, targets, counters, expected in cases:
        checks = []
        for rule in rules:
            checks.append({
                'eval_time': '2m', 'alertname': rule['alert'],
                'exp_alerts': [{'exp_labels': rule['labels'], 'exp_annotations': rule['annotations']}]
                if rule['alert'] == expected else [],
            })
        tests.append({'name': name, 'interval': '15s', 'input_series': nodes + targets + counters,
                      'alert_rule_test': checks})
    spec = {'rule_files': ['/tests/rules.json'], 'evaluation_interval': '15s', 'tests': tests}
    with tempfile.TemporaryDirectory() as directory:
        Path(directory, 'rules.json').write_text(json.dumps({'groups': [{'name': 'shop.network', 'rules': rules}]}))
        Path(directory, 'tests.json').write_text(json.dumps(spec))
        result = subprocess.run(['docker', 'run', '--rm', '--user', f'{os.getuid()}:{os.getgid()}',
                                 '-v', directory + ':/tests', '--entrypoint=/bin/promtool',
                                 container['image'], 'test', 'rules', '/tests/tests.json'], text=True)
        return result.returncode


if __name__ == '__main__':
    sys.exit(main())
