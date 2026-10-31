package exec

import "path/filepath"

// ResolverReadPaths grants the resolver's symlink target directory on systemd
// hosts. Network access alone cannot make DNS work when /etc/resolv.conf points
// into an otherwise unreadable /run directory.
func ResolverReadPaths() []string {
	const resolv = "/etc/resolv.conf"
	target, err := filepath.EvalSymlinks(resolv)
	if err != nil || target == resolv {
		return nil // /etc is already granted by the executor.
	}
	return []string{filepath.Dir(target)}
}
