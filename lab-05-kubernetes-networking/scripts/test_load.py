import csv
import http.server
import io
from pathlib import Path
import subprocess
import threading
import unittest


class Handler(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get('Content-Length', 0)))
        code = 201 if self.path == '/order' and body == b'{"item":"chaos-tea"}' else 500
        self.send_response(code)
        self.end_headers()
        self.wfile.write(b'{}')

    def log_message(self, *args):
        pass


class LoadTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        cls.thread = threading.Thread(target=cls.server.serve_forever, daemon=True)
        cls.thread.start()

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()
        cls.server.server_close()

    def run_load(self, url, duration='1'):
        return subprocess.run(
            ['sh', str(Path(__file__).with_name('load.sh')), url, duration, '0.05'],
            capture_output=True, text=True, timeout=8,
        )

    def test_records_successful_orders(self):
        result = self.run_load(f'http://127.0.0.1:{self.server.server_port}/order')
        self.assertEqual(result.returncode, 0, result.stderr)
        rows = list(csv.DictReader(io.StringIO(result.stdout)))
        self.assertTrue(rows)
        self.assertTrue(all(r['http_code'] == '201' and r['curl_exit'] == '0' for r in rows))

    def test_does_not_hide_http_failures(self):
        result = self.run_load(f'http://127.0.0.1:{self.server.server_port}/fail')
        rows = list(csv.DictReader(io.StringIO(result.stdout)))
        self.assertTrue(rows)
        self.assertTrue(all(r['http_code'] == '500' for r in rows))

    def test_does_not_hide_connection_failures(self):
        result = self.run_load('http://127.0.0.1:9/order')
        rows = list(csv.DictReader(io.StringIO(result.stdout)))
        self.assertTrue(rows)
        self.assertTrue(all(r['http_code'] == '000' and r['curl_exit'] != '0' for r in rows))

    def test_rejects_invalid_duration(self):
        result = self.run_load('http://127.0.0.1:9/order', '0')
        self.assertEqual(result.returncode, 2)
        self.assertIn('duration', result.stderr)


if __name__ == '__main__':
    unittest.main()
