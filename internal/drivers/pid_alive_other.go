//go:build !unix

package drivers

// pidAlive cannot tell here: every staging folder is kept.
func pidAlive(int) bool { return true }
