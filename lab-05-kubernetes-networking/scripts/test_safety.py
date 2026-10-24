import os
from pathlib import Path
import subprocess
import tempfile
import unittest


class SafetyTest(unittest.TestCase):
    def run_script(self, script, context='production', target='postgres-1', **extra_env):
        with tempfile.TemporaryDirectory() as directory:
            fake = Path(directory) / 'kubectl'
            fake.write_text('#!/bin/sh\nif [ "$1 $2" = "config current-context" ]; then echo "' + context + '"; else\n'
                            "printf '%s\\n' \"kubectl $*\" >> \"$COMMAND_LOG\"\necho " + target + ' 10.0.0.1 node\nfi\n')
            fake.chmod(0o755)
            helm = Path(directory) / 'helm'
            helm.write_text('#!/bin/sh\necho "helm $*" >> "$COMMAND_LOG"\n')
            helm.chmod(0o755)
            log = Path(directory) / 'commands.log'
            env = dict(os.environ, PATH=directory + ':' + os.environ['PATH'], HOME=directory,
                       COMMAND_LOG=str(log), **extra_env)
            result = subprocess.run(['bash', str(Path(__file__).with_name(script)), '1', '1'],
                                    env=env, text=True, capture_output=True, timeout=5)
            result.commands = log.read_text().splitlines() if log.exists() else []
            return result

    def test_chaos_refuses_other_cluster(self):
        result = self.run_script('chaos-pods.sh')
        self.assertEqual(result.returncode, 2)
        self.assertIn('Refusing', result.stderr)

    def test_chaos_never_selects_postgres(self):
        result = self.run_script('chaos-pods.sh', context='kind-lab5')
        self.assertEqual(result.returncode, 2)
        self.assertEqual(result.stdout.strip(), 'timestamp,pod,ip,node')

    def test_break_refuses_other_cluster(self):
        result = self.run_script('break-network.sh')
        self.assertEqual(result.returncode, 2)
        self.assertIn('Refusing', result.stderr)

    def test_chaos_pins_get_and_delete_context(self):
        result = self.run_script('chaos-pods.sh', context='kind-lab5', target='api-test')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(result.commands), 2)
        self.assertTrue(all('--context kind-lab5' in command for command in result.commands), result.commands)

    def test_break_pins_context_despite_helm_environment(self):
        result = self.run_script('break-network.sh', context='kind-lab5', BREAK_CASE='1',
                                 HELM_KUBECONTEXT='production')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('--kube-context kind-lab5', result.commands[0])
        self.assertIn('helm --kube-context kind-lab5', result.stdout)

    def test_break_rejects_unknown_case(self):
        result = self.run_script('break-network.sh', context='kind-lab5', BREAK_CASE='5')
        self.assertEqual(result.returncode, 2)
        self.assertIn('BREAK_CASE', result.stderr)


if __name__ == '__main__':
    unittest.main()
