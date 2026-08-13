// Package version carries the build identity of backup-tower.
package version

// Version is overridden at build time via -ldflags.
var Version = "dev"

// UserAgent is sent to the container engine so its logs attribute API calls to us.
func UserAgent() string {
	return "backup-tower/" + Version
}
