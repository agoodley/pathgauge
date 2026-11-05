// Package version reports build information for the pathgauge binary.
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Set at release build time with -ldflags "-X <pkg>.Version=... ".
var (
	Version = "dev"
	Commit  = ""
	Date    = ""
)

// Info describes the running build.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit,omitempty"`
	Date      string `json:"date,omitempty"`
	GoVersion string `json:"go_version"`
	Platform  string `json:"platform"`
}

// Get returns build information. When the binary was not built by the
// release pipeline it falls back to the VCS stamp embedded by `go build`.
func Get() Info {
	info := Info{
		Version:   Version,
		Commit:    Commit,
		Date:      Date,
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}
	if info.Commit != "" {
		return info
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return info
	}
	var revision, vcsTime string
	var modified bool
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.time":
			vcsTime = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	if revision != "" && modified {
		revision += "-dirty"
	}
	info.Commit = revision
	if info.Date == "" {
		info.Date = vcsTime
	}
	return info
}

func (i Info) String() string {
	s := fmt.Sprintf("pathgauge %s (%s, %s)", i.Version, i.Platform, i.GoVersion)
	if i.Commit != "" {
		s += " commit " + i.Commit
	}
	if i.Date != "" {
		s += " built " + i.Date
	}
	return s
}
