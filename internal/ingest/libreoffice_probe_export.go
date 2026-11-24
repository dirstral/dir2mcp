package ingest

import "time"

// SofficeProbeBinaryForTest exposes the binary the LibreOffice version probe
// runs for bin on the given GOOS, for tests in the tests/ tree.
func SofficeProbeBinaryForTest(bin, goos string) string {
	return sofficeProbeBinary(bin, goos)
}

// LibreOfficeWorkDirUsableWithinForTest exposes the bounded work-directory
// probe with injected filesystem calls, for tests in the tests/ tree.
func LibreOfficeWorkDirUsableWithinForTest(root string, limit time.Duration, mk func(string) (string, error), rm func(string) error) bool {
	return libreofficeWorkDirUsableWithin(root, limit, mk, rm)
}
