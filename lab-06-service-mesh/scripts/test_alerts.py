"""Render chart rules and exercise normal, failure and baseline series with promtool."""
import json
import pathlib
import subprocess
import tempfile
import textwrap
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[1]
NAMES = ['ShopCanaryHighErrorRate', 'ShopCanaryHighLatency', 'ShopMeshMetricsUnavailable']


def series(metric, labels, values):
    return dict(series=metric + '{' + ','.join(k+'="'+v+'"' for k,v in labels.items()) + '}', values=values)


def scenario(kind):
    labels = dict(namespace='shop', service='api-v2', version='v2', path='/order')
    data = [series('kube_deployment_spec_replicas', dict(namespace='shop', deployment='api-v1'), '2+0x24'),
            series('kube_deployment_spec_replicas', dict(namespace='shop', deployment='api-v2'), '1+0x24')]
    if kind != 'absent':
        for pod in ['a','b','c']:
            data.append(series('up', dict(namespace='shop', service='api-proxy-metrics', pod=pod), ('0' if kind == 'scrape-failed' and pod=='b' else '1')+'+0x24'))
    data.append(series('shop_http_requests_total', dict(labels, code='201'), ('0+18x24' if kind=='errors' else '0+20x24')))
    if kind == 'errors':
        data.append(series('shop_http_requests_total', dict(labels, code='500'), '0+2x24'))
    for le in ['0.3', '0.6', '1', '+Inf']:
        values = '0+0x24' if kind=='latency' and le in ['0.3','0.6'] else '0+20x24'
        data.append(series('shop_http_request_duration_seconds_bucket', dict(labels, le=le), values))
    expected = {'errors':NAMES[0], 'latency':NAMES[1], 'absent':NAMES[2], 'scrape-failed':NAMES[2]}.get(kind)
    return dict(name=kind, interval='15s', input_series=data, alert_rule_test=[
        dict(eval_time='3m', alertname=name, exp_alerts=[dict(exp_labels=dict(severity='warning' if name==NAMES[2] else 'critical'), exp_annotations=dict(summary={NAMES[0]:'Canary v2 returns more than 5% errors', NAMES[1]:'Canary v2 p95 exceeds 600 ms', NAMES[2]:'Mesh proxy metrics are missing or scrape failed'}[name]))] if expected==name else [])
        for name in NAMES])


class AlertTests(unittest.TestCase):
    def check_rules(self, enabled):
        args = ['helm','template','shop',str(ROOT/'shop'),'-n','shop','--show-only','templates/mesh-prometheus-rule.yaml','--set','mesh.enabled='+str(enabled).lower()]
        rendered = subprocess.run(args, check=True, text=True, capture_output=True).stdout
        rules = textwrap.dedent(rendered.split('spec:\n',1)[1])
        with tempfile.TemporaryDirectory(prefix='lab6-alerts-') as directory:
            directory = pathlib.Path(directory)
            (directory/'rules.yaml').write_text(rules)
            tests = [scenario(kind) for kind in ['normal','errors','latency','absent','scrape-failed']] if enabled else [scenario('normal')]
            if not enabled:
                tests[0]['input_series'] = []
            (directory/'test.yaml').write_text(json.dumps(dict(rule_files=['rules.yaml'], evaluation_interval='15s', tests=tests)))
            subprocess.run(['promtool','check','rules',str(directory/'rules.yaml')], check=True)
            subprocess.run(['promtool','test','rules',str(directory/'test.yaml')], check=True, cwd=directory)

    def test_mesh_alerts(self): self.check_rules(True)
    def test_disabled_mesh_is_not_an_outage(self): self.check_rules(False)

if __name__ == '__main__': unittest.main()
