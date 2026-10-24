import csv
import importlib.util
import io
import pathlib
import socket
import tempfile
import unittest
from unittest.mock import patch
from urllib.error import HTTPError

spec = importlib.util.spec_from_file_location('load', pathlib.Path(__file__).with_name('load.py'))
load = importlib.util.module_from_spec(spec)
spec.loader.exec_module(load)

class LoadTests(unittest.TestCase):
    def test_500_is_error_without_retry(self):
        with patch.object(load, 'urlopen', side_effect=HTTPError('http://api', 500, '', {}, io.BytesIO(b'failed'))) as call:
            row = load.request('http://api/order', 1)
        self.assertEqual(row['status'], 500)
        self.assertEqual(row['ok'], 0)
        self.assertEqual(call.call_count, 1)

    def test_timeout_is_error_without_retry(self):
        with patch.object(load, 'urlopen', side_effect=socket.timeout()) as call:
            row = load.request('http://api/order', 1)
        self.assertEqual(row['status'], 0)
        self.assertEqual(row['ok'], 0)
        self.assertEqual(call.call_count, 1)

    def test_unknown_version_is_not_success(self):
        response = unittest.mock.MagicMock()
        response.__enter__.return_value = response
        response.status = 200
        response.read.return_value = b'v3\n'
        with patch.object(load, 'urlopen', return_value=response):
            row = load.request('http://api/version', 1, version=True)
        self.assertEqual(row['ok'], 0)

    def test_load_uses_absolute_schedule_and_counts_missed_slots(self):
        with tempfile.TemporaryDirectory() as d:
            output = str(pathlib.Path(d)/'load.csv')
            with patch.object(load, 'request', return_value={'time': 1, 'status': 201, 'seconds': .01, 'ok': 1, 'version': ''}) as req:
                load.run('http://api/order', rate=20, duration=.2, output=output, timeout=1)
            with open(output) as f:
                rows = list(csv.DictReader(f))
            self.assertEqual(len(rows), 4)
            self.assertEqual(req.call_count, 4)

    def test_late_slots_are_counted_as_errors_not_silently_skipped(self):
        with tempfile.TemporaryDirectory() as d:
            output = str(pathlib.Path(d)/'late.csv')
            with patch.object(load.time, 'monotonic', side_effect=[0] + [1]*8), patch.object(load.time, 'sleep'), patch.object(load, 'request') as request:
                rows = load.run('http://api/order', rate=20, duration=.2, output=output, timeout=1)
            request.assert_not_called()
            self.assertEqual(len(rows), 4)
            self.assertEqual(load.summary(rows)['errors'], 4)

if __name__ == '__main__':
    unittest.main()
