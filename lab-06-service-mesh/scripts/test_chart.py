import pathlib
import subprocess
import unittest

CHART = str(pathlib.Path(__file__).resolve().parents[1] / 'shop')

class ChartTests(unittest.TestCase):
    def render(self, *settings):
        args = ['helm', 'template', 'shop', CHART, '-n', 'shop']
        for setting in settings: args += ['--set', setting]
        return subprocess.run(args, text=True, capture_output=True)

    def test_baseline_has_no_route_or_proxy_monitor(self):
        result = self.render('mesh.enabled=false', 'api.v2.replicas=0', 'traffic.v2Weight=0')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn('kind: HTTPRoute', result.stdout)
        self.assertNotIn('name: api-proxy-metrics', result.stdout)
        self.assertNotIn('linkerd.io/inject: "enabled"', result.stdout)
        self.assertIn('name: postgres-app', result.stdout)

    def test_mesh_routes_and_separate_identities(self):
        result = self.render('traffic.v2Weight=10')
        self.assertEqual(result.returncode, 0, result.stderr)
        for text in ['kind: HTTPRoute', 'weight: 90', 'weight: 10', 'serviceAccountName: api-v1', 'serviceAccountName: api-v2', 'podTargetLabels: [version]']:
            self.assertIn(text, result.stdout)

    def test_invalid_weights_rejected(self):
        for weight in ['-1', '101', '0.5', 'oops', 'true']:
            result = self.render('traffic.v2Weight=' + weight)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn('traffic.v2Weight', result.stderr)

    def test_retries_opt_in(self):
        self.assertNotIn('retry.linkerd.io/http', self.render().stdout)
        self.assertIn('retry.linkerd.io/limit: "3"', self.render('mesh.retries.enabled=true').stdout)

if __name__ == '__main__': unittest.main()
