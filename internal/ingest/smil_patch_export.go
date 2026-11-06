package ingest

// Test seam for the SPEC §8.6.14 SMIL caption patch, for the tests/ tree
// (AGENTS.md: no new _test.go under internal/). Production code never sets it.

// SetSMILBeforeCommitForTest sets a function that replaceSMIL runs just
// before its final check that the SMIL on disk still holds the bytes it was
// read with. A test uses it to rewrite the SMIL inside that window. Nil
// removes it.
func (s *Service) SetSMILBeforeCommitForTest(fn func(abs string)) {
	s.smilBeforeCommit = fn
}
