package endpoint

import "time"

// SetAttachTimeout shortens how long a TCP test waits for its data
// connections and returns a function that restores it.
func SetAttachTimeout(d time.Duration) (restore func()) {
	old := attachTimeout
	attachTimeout = d
	return func() { attachTimeout = old }
}
