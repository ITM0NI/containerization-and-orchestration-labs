#!/usr/bin/env python3
"""Проверка матрицы из реальных сетевых namespace; stdout — CSV, stderr — диагностика."""
import csv
import json
import subprocess
import sys
import tempfile
import time


def verdict(returncode, resolved):
    if not resolved or returncode not in (0, 1):
        return 'ERROR'
    return 'ALLOW' if returncode == 0 else 'DENY'


def kubectl(*args, check=True):
    return subprocess.run(['kubectl', '--context', 'kind-lab5', *args], text=True, capture_output=True,
                          timeout=35, check=check)


def pods(namespace, selector):
    data = json.loads(kubectl('-n', namespace, 'get', 'pods', '-l', selector, '-o', 'json').stdout)
    result = [p for p in data['items'] if not p['metadata'].get('deletionTimestamp')
              and p['status'].get('phase') == 'Running'
              and all(c.get('ready') for c in p['status'].get('containerStatuses', []))]
    if not result:
        raise RuntimeError(f'No ready pods: {namespace}/{selector}')
    return result


def diagnostic(namespace, pod):
    name = pod['metadata']['name']
    if pod['spec']['containers'][0]['image'].startswith('nicolaka/netshoot:'):
        return (namespace, name, pod['spec']['containers'][0]['name'])
    nonroot = pod['spec'].get('securityContext', {}).get('runAsNonRoot', False)
    base = 'netcheck-nonroot' if nonroot else 'netcheck'
    container = base
    terminated = {c['name'] for c in pod.get('status', {}).get('ephemeralContainerStatuses', [])
                  if 'terminated' in c.get('state', {})}
    number = 1
    # Ephemeral-контейнер после остановки VM не перезапускается; нужно новое имя.
    while container in terminated:
        container = f'{base}-{number}'
        number += 1
    if not any(c['name'] == container for c in pod['spec'].get('ephemeralContainers', [])):
        with tempfile.NamedTemporaryFile(mode='w', suffix='.json') as profile:
            json.dump({'securityContext': {'runAsUser': 65532, 'runAsNonRoot': True}}, profile)
            profile.flush()
            kubectl('-n', namespace, 'debug', name, '--image=nicolaka/netshoot:v0.14',
                    f'--container={container}', '--profile=general', f'--custom={profile.name}',
                    '--', 'sleep', 'infinity')
    source = (namespace, name, container)
    for _ in range(30):
        if execute(source, 'true').returncode == 0:
            return source
        time.sleep(2)
    raise RuntimeError(f'Diagnostic container not started: {namespace}/{name}')


def execute(source, *command):
    namespace, name, container = source
    return kubectl('-n', namespace, 'exec', name, '-c', container, '--', *command, check=False)


def main():
    if kubectl('config', 'current-context').stdout.strip() != 'kind-lab5':
        raise RuntimeError('Refusing checks outside kind-lab5')
    sources = []
    for role in ('client', 'api', 'worker', 'postgres', 'outsider'):
        for pod in pods('shop', f'app={role}'):
            sources.append((role, pod, diagnostic('shop', pod)))
    impostor = pods('untrusted', 'app=client')[0]
    sources.append(('impostor', impostor, diagnostic('untrusted', impostor)))
    api_service = json.loads(kubectl('-n', 'shop', 'get', 'svc', 'api', '-o', 'json').stdout)
    api_ip = api_service['spec']['clusterIP']
    node = json.loads(kubectl('get', 'node', 'lab5-control-plane', '-o', 'json').stdout)
    node_ip = next(a['address'] for a in node['status']['addresses'] if a['type'] == 'InternalIP')
    rows = []
    writer = csv.writer(sys.stdout)
    writer.writerow(['source', 'destination', 'port', 'expected', 'actual', 'match'])

    def record(source, destination, port, expected, actual):
        row = [f'{source[0]}/{source[1]}', destination, port, expected, actual, expected == actual]
        rows.append(row)
        writer.writerow(row)
        sys.stdout.flush()

    def tcp(source, host, port, expected):
        resolved = execute(source, 'getent', 'ahostsv4', host).returncode == 0
        result = execute(source, 'nc', '-zv', '-w', '2', host, str(port))
        actual = verdict(result.returncode, resolved)
        record(source, host, f'TCP/{port}', expected, actual)
        if actual != expected:
            print(f'{source} -> {host}:{port}: {result.stderr.strip()}', file=sys.stderr)

    for role, pod, source in sources:
        for protocol in ('UDP', 'TCP'):
            args = ['dig', '+time=1', '+tries=1', '+short']
            if protocol == 'TCP':
                args.append('+tcp')
            result = execute(source, *args, 'api.shop.svc.cluster.local', 'A')
            actual = 'ALLOW' if result.returncode == 0 and api_ip in result.stdout.splitlines() else 'ERROR'
            record(source, 'CoreDNS', f'{protocol}/53', 'ALLOW', actual)
        for target, host, port in (
            ('api', 'api.shop.svc.cluster.local', 8080),
            ('worker', 'worker.shop.svc.cluster.local', 8080),
            ('postgres', 'postgres-rw.shop.svc.cluster.local', 5432),
        ):
            if role == target:
                # Loopback не блокируется NetworkPolicy: для реплик ниже проверяется другой pod.
                continue
            allowed = (role == 'client' and target == 'api') or (role in ('api', 'worker') and target == 'postgres')
            tcp(source, host, port, 'ALLOW' if allowed else 'DENY')
        if role in ('api', 'worker'):
            peer = next(p for r, p, _ in sources if r == role and p['metadata']['name'] != pod['metadata']['name'])
            tcp(source, peer['status']['podIP'], 8080, 'DENY')
        if role != 'impostor':
            tcp(source, 'kubernetes.default.svc.cluster.local', 443, 'ALLOW' if role == 'postgres' else 'DENY')
        else:
            # Нельзя обойти pod/namespace selectors через NodePort и SNAT.
            tcp(source, node_ip, api_service['spec']['ports'][0]['nodePort'], 'DENY')

    prometheus = diagnostic('monitoring', pods('monitoring', 'prometheus=monitoring-prometheus')[0])
    tcp(prometheus, 'api.shop.svc.cluster.local', 8080, 'ALLOW')
    tcp(prometheus, 'worker.shop.svc.cluster.local', 8080, 'ALLOW')
    tcp(prometheus, 'postgres-rw.shop.svc.cluster.local', 5432, 'DENY')
    operator = diagnostic('cnpg-system', pods('cnpg-system', 'app.kubernetes.io/name=cloudnative-pg')[0])
    database = next(p for role, p, _ in sources if role == 'postgres')
    tcp(operator, database['status']['podIP'], 8000, 'ALLOW')
    failed = [r for r in rows if not r[-1]]
    print(f'Matrix: {len(rows) - len(failed)}/{len(rows)} matched', file=sys.stderr)
    return 1 if failed else 0


if __name__ == '__main__':
    sys.exit(main())
