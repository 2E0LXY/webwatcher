//go:build !windows

package agent

// restrictToUser: SaveToken has already set mode 0600, which is the whole of the protection on Unix.
func restrictToUser(string) error { return nil }
