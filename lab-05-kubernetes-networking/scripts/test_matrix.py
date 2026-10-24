import unittest
from unittest.mock import patch
from check_matrix import diagnostic, kubectl, verdict


class VerdictTest(unittest.TestCase):
    def test_success_is_allowed(self):
        self.assertEqual(verdict(0, True), 'ALLOW')

    def test_failed_connect_after_resolution_is_denied(self):
        self.assertEqual(verdict(1, True), 'DENY')

    def test_dns_failure_is_not_a_policy_proof(self):
        self.assertEqual(verdict(1, False), 'ERROR')

    def test_exec_or_missing_tool_error_is_not_a_policy_proof(self):
        self.assertEqual(verdict(127, True), 'ERROR')


class ContextTest(unittest.TestCase):
    @patch('check_matrix.subprocess.run')
    def test_commands_pin_lab5_context(self, run):
        kubectl('-n', 'shop', 'get', 'pods')
        self.assertEqual(run.call_args.args[0][:3], ['kubectl', '--context', 'kind-lab5'])


class DiagnosticTest(unittest.TestCase):
    @patch('check_matrix.execute')
    @patch('check_matrix.kubectl')
    def test_terminated_ephemeral_container_is_not_reused(self, kubectl, execute):
        execute.return_value.returncode = 0
        pod = {'metadata': {'name': 'api-test'}, 'spec': {
            'containers': [{'image': 'shop-api:lab5.1'}],
            'ephemeralContainers': [{'name': 'netcheck'}]},
            'status': {'ephemeralContainerStatuses': [
                {'name': 'netcheck', 'state': {'terminated': {'exitCode': 255}}}]}}
        source = diagnostic('shop', pod)
        self.assertNotEqual(source[2], 'netcheck')
        self.assertIn(f'--container={source[2]}', kubectl.call_args.args)

    @patch('check_matrix.execute')
    @patch('check_matrix.kubectl')
    def test_running_ephemeral_container_is_reused(self, kubectl, execute):
        execute.return_value.returncode = 0
        pod = {'metadata': {'name': 'api-test'}, 'spec': {
            'containers': [{'image': 'shop-api:lab5.1'}],
            'ephemeralContainers': [{'name': 'netcheck'}]},
            'status': {'ephemeralContainerStatuses': [
                {'name': 'netcheck', 'state': {'running': {}}}]}}
        self.assertEqual(diagnostic('shop', pod), ('shop', 'api-test', 'netcheck'))
        kubectl.assert_not_called()


if __name__ == '__main__':
    unittest.main()
