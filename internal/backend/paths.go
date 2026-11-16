package backend

import (
	"path"
	"path/filepath"
)

// PosixPaths supplies POSIX path semantics: the correct choice for any
// backend whose environment is a Linux/macOS container or sandbox.
var PosixPaths = PathOps{
	Join:  path.Join,
	Dir:   path.Dir,
	Base:  path.Base,
	IsAbs: path.IsAbs,
	Clean: path.Clean,
}

// NativePaths supplies the host process's path semantics. It is correct for
// LocalBackend and wrong for remote backends, where the host's separators
// and drive-letter rules do not apply.
var NativePaths = PathOps{
	Join:  filepath.Join,
	Dir:   filepath.Dir,
	Base:  filepath.Base,
	IsAbs: filepath.IsAbs,
	Clean: filepath.Clean,
}
