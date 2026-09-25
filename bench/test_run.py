"""Unit tests for the runner's index readiness check."""
import unittest

import run


def status(**ix):
    return {"snapshot": {"indexing": ix}}


class IndexReadiness(unittest.TestCase):
    def test_done_needs_every_chunk_embedded_and_no_errors(self):
        self.assertTrue(run.index_done(status(running=False, errors=0, chunks_total=4, embedded_ok=4, embedded_pending=0)))

    def test_pending_zero_is_not_enough(self):
        # A chunk whose embedding failed is not pending either.
        self.assertFalse(run.index_done(status(running=False, errors=0, chunks_total=4, embedded_ok=3, embedded_pending=0)))

    def test_errors_mean_a_partial_corpus(self):
        st = status(running=False, errors=1, chunks_total=4, embedded_ok=4, embedded_pending=0)
        self.assertFalse(run.index_done(st))
        self.assertTrue(run.index_failed(st))

    def test_running_and_empty_are_not_done(self):
        self.assertFalse(run.index_done(status(running=True, errors=0, chunks_total=4, embedded_ok=4)))
        self.assertFalse(run.index_done(status(running=False, errors=0, chunks_total=0, embedded_ok=0)))
        self.assertFalse(run.index_failed(status(running=True, errors=2)))


if __name__ == "__main__":
    unittest.main()
