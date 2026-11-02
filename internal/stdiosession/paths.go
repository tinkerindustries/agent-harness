package stdiosession

import "path/filepath"

// sameDir reports whether two spellings name the same directory. A client may
// name the working directory through a symlink or a relative path, while a run
// the server starts for a background completion carries the session row's
// resolved workspace, so comparing the two as text refuses a continuation that
// names the directory it already works in.
func sameDir(a, b string) bool {
	return resolveDir(a) == resolveDir(b)
}

// resolveDir reduces a directory to the form two spellings of it share:
// absolute, cleaned, and with symlinks followed. A path that cannot be made
// absolute is returned as it was given, and one whose symlinks cannot be
// followed is returned absolute and cleaned.
func resolveDir(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return abs
	}
	return resolved
}
