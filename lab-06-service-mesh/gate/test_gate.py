import json
import math
import unittest
from unittest.mock import MagicMock, patch
import gate

GOOD = dict(requests=100, error_rate=0, p95_seconds=.1, age_seconds=5, targets_up=1)

class DecisionTests(unittest.TestCase):
    def test_two_good_checks_promote(self):
        self.assertEqual(gate.decide(10, GOOD, 0)[:2], (10, 1))
        self.assertEqual(gate.decide(10, GOOD, 1)[:2], (25, 0))
        for old, new in [(25, 50), (50, 75), (75, 100)]:
            self.assertEqual(gate.decide(old, GOOD, 1)[:2], (new, 0))

    def test_error_and_latency_independently_rollback(self):
        for stats in [dict(GOOD, error_rate=.051), dict(GOOD, p95_seconds=.601)]:
            self.assertEqual(gate.decide(10, stats, 1)[:2], (0, 0))

    def test_boundaries_and_middle_hold(self):
        for stats in [dict(GOOD, error_rate=.01), dict(GOOD, error_rate=.05), dict(GOOD, p95_seconds=.6), dict(GOOD, p95_seconds=.301)]:
            self.assertEqual(gate.decide(25, stats, 1)[:2], (25, 0))
        self.assertEqual(gate.decide(10, dict(GOOD, p95_seconds=.3, requests=50, age_seconds=30), 1)[0], 25)

    def test_no_data_resets_good_streak(self):
        for stats in [None, {}, dict(GOOD, requests=49), dict(GOOD, age_seconds=31), dict(GOOD, targets_up=0), dict(GOOD, p95_seconds=None)]:
            self.assertEqual(gate.decide(10, stats, 1)[:2], (10, 0))

    def test_empty_histogram_nan_with_few_requests_is_no_data(self):
        self.assertEqual(gate.decide(10, dict(GOOD, requests=0, p95_seconds=math.nan), 1)[:2], (10, 0))

    def test_nonfinite_and_invalid_values_never_promote(self):
        for key in GOOD:
            for value in [math.nan, math.inf, -math.inf]:
                with self.subTest(key=key, value=value), self.assertRaises(ValueError):
                    gate.decide(10, dict(GOOD, **{key: value}), 1)
        for stats in [dict(GOOD, requests=-1), dict(GOOD, error_rate=-.1), dict(GOOD, error_rate=1.1), dict(GOOD, p95_seconds=-1), dict(GOOD, age_seconds=-1)]:
            with self.assertRaises(ValueError):
                gate.decide(10, stats, 1)

class IOTests(unittest.TestCase):
    def response(self, body):
        response = MagicMock()
        response.__enter__.return_value = response
        response.read.return_value = json.dumps(body).encode()
        return response

    def test_prometheus_errors_propagate(self):
        with patch.object(gate, 'urlopen', side_effect=OSError('offline')):
            with self.assertRaises(OSError): gate.query('http://prom', 'up')
        for body in [{'status':'error'}, {'status':'success','data':{'resultType':'matrix','result':[]}}, {'status':'success','data':{'resultType':'vector','result':[{}, {}]}}]:
            with patch.object(gate, 'urlopen', return_value=self.response(body)):
                with self.assertRaises(ValueError): gate.query('http://prom', 'up')

    def test_empty_vector_is_missing_and_nan_preserved(self):
        with patch.object(gate, 'urlopen', return_value=self.response({'status':'success','data':{'resultType':'vector','result':[]}})):
            self.assertIsNone(gate.query('http://prom', 'up'))
        with patch.object(gate, 'urlopen', return_value=self.response({'status':'success','data':{'resultType':'vector','result':[{'value':[1, 'NaN']} ]}})):
            self.assertTrue(math.isnan(gate.query('http://prom', 'up')))

    def test_only_order_v2_is_queried_and_failed_target_visible(self):
        with patch.object(gate, 'query', return_value=1) as query:
            gate.read_stats('http://prom')
        expressions = [call.args[1] for call in query.call_args_list]
        self.assertTrue(any('path="/order"' in q and 'version="v2"' in q for q in expressions))
        self.assertTrue(any('min(' in q and 'up{' in q for q in expressions))

    def test_helm_failure_is_fatal_and_only_weight_is_set(self):
        import subprocess
        with patch.object(gate.subprocess, 'run', side_effect=subprocess.CalledProcessError(1, 'helm')):
            with self.assertRaises(subprocess.CalledProcessError): gate.apply_weight('./shop', 25)
        with patch.object(gate.subprocess, 'run', return_value=MagicMock(stdout='')) as run, patch.object(gate, 'current_weight', return_value=25):
            gate.apply_weight('./shop', 25)
        args = run.call_args.args[0]
        self.assertIn('--reuse-values', args)
        self.assertIn('kind-lab6', args)
        self.assertEqual([a for a in args if a.startswith('traffic.')], ['traffic.v2Weight=25'])

    def test_wrong_applied_weight_is_fatal(self):
        with patch.object(gate.subprocess, 'run'), patch.object(gate, 'current_weight', return_value=10):
            with self.assertRaises(RuntimeError): gate.apply_weight('./shop', 25)

class LoopTests(unittest.TestCase):
    def test_each_step_waits_for_new_window(self):
        clock, reads, changes = [0], [], []
        def read(_):
            reads.append(clock[0])
            return GOOD
        def apply(_, weight): changes.append((clock[0], weight))
        with patch.object(gate, 'current_weight', return_value=10), patch.object(gate, 'read_stats', side_effect=read), patch.object(gate, 'apply_weight', side_effect=apply), patch.object(gate.time, 'monotonic', side_effect=lambda: clock[0]), patch.object(gate.time, 'sleep', side_effect=lambda n: clock.__setitem__(0, clock[0]+n)), patch('builtins.print'):
            self.assertEqual(gate.run('http://prom', './shop'), 100)
        self.assertGreaterEqual(reads[0], 60)
        self.assertEqual([weight for _, weight in changes], [25, 50, 75, 100])
        for (before, _), (after, _) in zip(changes, changes[1:]):
            self.assertGreaterEqual(after-before, 75)

    def test_missing_data_exits_after_120_seconds_without_changes(self):
        clock = [0]
        with patch.object(gate, 'current_weight', return_value=10), patch.object(gate, 'read_stats', return_value=None), patch.object(gate, 'apply_weight') as apply, patch.object(gate.time, 'monotonic', side_effect=lambda: clock[0]), patch.object(gate.time, 'sleep', side_effect=lambda n: clock.__setitem__(0, clock[0]+n)), patch('builtins.print'):
            with self.assertRaisesRegex(RuntimeError, '120 seconds'):
                gate.run('http://prom', './shop')
        apply.assert_not_called()
        self.assertEqual(clock[0], 180)

if __name__ == '__main__': unittest.main()
