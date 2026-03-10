package Storage

import "os"

// SafeRename renames src to dst, removing dst first if it exists.
// On Unix, os.Rename atomically overwrites dst. On Windows, os.Rename
// fails if dst exists — this function handles both cases.
func SafeRename(src, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		if err := os.Remove(dst); err != nil {
			return err
		}
	}
	return os.Rename(src, dst)
}
