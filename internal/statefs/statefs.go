// Package statefs creates and repairs the local state hierarchy with
// owner-only permissions.
//
// Everything dir2mcp keeps under StateDir is derived from the corpus: OCR
// output, transcripts, translations, summaries, contextual text, and index
// payloads that carry titles, snippets, speakers and media references. That is
// the corpus in a different shape, so it inherits the corpus's confidentiality
// rather than a cache's convenience. meta.sqlite, bearer tokens and support
// bundles were already owner-only; this is the rest of the tree catching up
// (#726).
//
// Two properties the mode arguments alone did not give:
//
//   - `MkdirAll` does nothing to a directory that ALREADY exists, so a tree
//     first created 0755 by an older build, or by whichever code path happened
//     to run first, stayed 0755 forever. StateDir was created 0700 by
//     `service` and `up --daemon` and 0755 by `up` and `reindex`, so its mode
//     depended on how the operator started the daemon. `Harden` repairs an
//     existing path instead of assuming a fresh one.
//
//   - 0700 and 0600 are umask-safe (a umask can only remove bits, and these
//     grant none outside the owner), so the result does not depend on the
//     ambient umask the way 0755 did.
//
// Not everything under a state path belongs here. Artifacts written FOR
// something else to read — a launchd plist, a systemd unit, an export the
// operator asked for — keep their own modes, and their call sites say so.
package statefs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

const (
	// DirMode is the mode for every directory in the state hierarchy.
	DirMode fs.FileMode = 0o700
	// FileMode is the mode for every corpus-derived file in it.
	FileMode fs.FileMode = 0o600
)

// MkdirAll creates path and any missing parents, owner-only.
//
// Existing directories keep their mode, which is what `Harden` is for: this
// call cannot distinguish a parent the operator owns and shares deliberately
// (a home directory) from one that is ours to tighten.
func MkdirAll(path string) error {
	return os.MkdirAll(path, DirMode)
}

// MkdirAllHardened creates path owner-only and tightens it if it already
// existed with a wider mode. Use for directories that are ours: the state root
// and the derived-content trees under it.
func MkdirAllHardened(path string) error {
	if err := MkdirAll(path); err != nil {
		return err
	}
	return Harden(path)
}

// WriteFile writes data to path owner-only, truncating any existing file.
//
// os.WriteFile leaves an existing file's mode alone, so a file first written
// 0644 stays 0644 through every later rewrite. This chmods after the write to
// close that.
func WriteFile(path string, data []byte) error {
	if err := os.WriteFile(path, data, FileMode); err != nil {
		return err
	}
	return chmodIfWider(path, FileMode)
}

// Create opens path for writing, owner-only, truncating an existing file.
func Create(path string) (*os.File, error) {
	return OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC)
}

// OpenFile opens path with the caller's flags at the owner-only mode, and
// tightens an existing file whose mode is wider.
func OpenFile(path string, flag int) (*os.File, error) {
	file, err := os.OpenFile(path, flag, FileMode) //nolint:gosec // mode is FileMode
	if err != nil {
		return nil, err
	}
	if err := chmodFileIfWider(file, FileMode); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

// Harden tightens one path to the owner-only mode for its kind.
//
// A path we do not own cannot be chmod'd, and that is reported rather than
// ignored: continuing would leave corpus-derived content readable by other
// local accounts while the logs claimed the state directory was private.
func Harden(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	want := FileMode
	if info.IsDir() {
		want = DirMode
	}
	return chmodInfoIfWider(path, info, want)
}

// ErrNotRegular reports a path that must be a plain file and is not: a
// symlink, a FIFO, a socket, a device node or a directory.
var ErrNotRegular = errors.New("not a regular file")

// HardenSecret tightens an existing secret file to the owner-only mode and
// reports the permissions it had beforehand, so the caller can tell the
// operator that the secret had been readable by other local accounts.
// Tightening does not undo an exposure, so silently repairing a credential
// without saying anything would be the wrong kind of quiet.
//
// Two things this does that `Harden` does not, both because a credential is
// not derived content:
//
//   - It refuses a path that is not a regular file, without following it.
//     `HardenTree` deliberately skips symlinks, because for a cache the mode
//     that matters is the target's and the target may live outside the tree
//     on purpose. A symlinked credential is the case that must not be
//     trusted: it is read from outside the state directory at whatever mode
//     that target happens to have, and rewriting it truncates the target. An
//     operator who wants the token to live elsewhere has `--auth file:<path>`,
//     which is operator-managed by contract.
//   - A missing path is not an error: it reports exists=false so the caller
//     can create the file itself.
//
// Windows has no POSIX mode bits. Go synthesizes 0666/0444 from the read-only
// attribute and os.Chmod can only toggle that attribute, so the tightening
// degrades to a no-op there and the file's confidentiality rests on the NTFS
// ACLs it inherits from the state directory; no ACL repair is attempted. The
// regular-file refusal is enforced on every platform.
//
// The mode is checked and then changed by path, so a state directory another
// account can write to leaves a small window between the two. That account can
// replace the token outright anyway, which is the larger problem and not one a
// chmod can fix.
func HardenSecret(path string) (prior fs.FileMode, exists bool, err error) {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, false, nil
		}
		return 0, false, err
	}
	if !info.Mode().IsRegular() {
		return 0, true, fmt.Errorf("%s is %s: %w", path, info.Mode().String(), ErrNotRegular)
	}
	prior = info.Mode().Perm()
	if err := chmodInfoIfWider(path, info, FileMode); err != nil {
		return prior, true, err
	}
	return prior, true, nil
}

// WiderThanOwnerOnly reports whether mode grants any access beyond the owner.
// On Windows the bits are synthetic (see HardenSecret), so this describes the
// file the way Go reports it rather than the ACL that actually governs it.
func WiderThanOwnerOnly(mode fs.FileMode) bool {
	return mode.Perm()&^FileMode != 0
}

// HardenTree tightens root and everything under it.
//
// Called on the state directory at startup so a corpus indexed by an older
// build, or under a permissive umask, becomes private on the next run instead
// of only for newly written files. Paths that disappear mid-walk are skipped:
// a live daemon rotates temporaries under here.
func HardenTree(root string) error {
	if _, err := os.Stat(root); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		info, statErr := entry.Info()
		if statErr != nil {
			if errors.Is(statErr, fs.ErrNotExist) {
				return nil
			}
			return statErr
		}
		// Symlinks are not followed and not chmod'd: the mode that matters is
		// the target's, and the target may deliberately live outside the tree.
		if info.Mode()&fs.ModeSymlink != 0 {
			return nil
		}
		want := FileMode
		if entry.IsDir() {
			want = DirMode
		}
		if cErr := chmodInfoIfWider(path, info, want); cErr != nil {
			if errors.Is(cErr, fs.ErrNotExist) {
				return nil
			}
			return cErr
		}
		return nil
	})
}

// chmodIfWider tightens path when its current mode grants more than want.
func chmodIfWider(path string, want fs.FileMode) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	return chmodInfoIfWider(path, info, want)
}

func chmodInfoIfWider(path string, info fs.FileInfo, want fs.FileMode) error {
	current := info.Mode().Perm()
	// Only ever remove bits. A file the operator deliberately made MORE
	// restrictive (0400 on a read-only cache) stays that way.
	if current&^want == 0 {
		return nil
	}
	if err := os.Chmod(path, current&want); err != nil {
		return fmt.Errorf("tighten %s to owner-only: %w", path, err)
	}
	return nil
}

func chmodFileIfWider(file *os.File, want fs.FileMode) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	current := info.Mode().Perm()
	if current&^want == 0 {
		return nil
	}
	if err := file.Chmod(current & want); err != nil {
		return fmt.Errorf("tighten %s to owner-only: %w", file.Name(), err)
	}
	return nil
}
