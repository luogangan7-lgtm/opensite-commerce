"""Drill metric witnesses must detect wrong counts, duplicates and invalid age."""
import io
from pathlib import Path
import sys
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / 'scripts'))
import ops_drill


class MetricWitnessTests(unittest.TestCase):
    def check(self, pending='1', dead='1', age='2.5', extra=''):
        text = (f'opensite_outbox_pending {pending}\n'
                f'opensite_outbox_dead_letters {dead}\n'
                f'opensite_outbox_oldest_pending_age_seconds {age}\n' + extra)
        with patch.object(ops_drill, 'live_url', return_value='http://worker'), \
                patch.object(ops_drill.urllib.request, 'urlopen', return_value=io.BytesIO(text.encode())):
            ops_drill.worker_metrics()

    def test_positive_witness(self):
        self.check()

    def test_counts_are_complete_samples(self):
        for pending, dead in [('10', '1'), ('1', '11'), ('1.1', '1'), ('-1', '1')]:
            with self.subTest(pending=pending, dead=dead):
                with self.assertRaises(ops_drill.OpsError):
                    self.check(pending, dead)

    def test_duplicate_sample_is_not_a_positive_witness(self):
        with self.assertRaises(ops_drill.OpsError):
            self.check(extra='opensite_outbox_pending 10\n')

    def test_age_is_a_finite_nonnegative_sample(self):
        for age in ['NaN', 'Inf', '-1', 'invalid']:
            with self.subTest(age=age):
                with self.assertRaises(ops_drill.OpsError):
                    self.check(age=age)


if __name__ == '__main__':
    unittest.main()
