package corpusfs

import "regexp"

// smilBackupPattern matches the backup name of the SMIL caption patch:
// `<smil>.bak.<YYYYmmdd_HHMMSS>`, with an optional `.<n>` suffix for a second
// change in the same second (SPEC §7.1, §8.6.14). The match is case-sensitive.
var smilBackupPattern = regexp.MustCompile(`\.smil\.bak\.\d{8}_\d{6}(?:\.\d+)?$`)

// IsSMILBackup reports whether relPath names a SMIL caption-patch backup.
// Discovery skips these on every backend and in watch mode, whether or not
// write-back is enabled: a backup is a copy of an older SMIL, and to index it
// would add one stale document for each patched video (SPEC §7.1). The test
// uses the name only, so it never reads the file.
func IsSMILBackup(relPath string) bool {
	return smilBackupPattern.MatchString(relPath)
}
