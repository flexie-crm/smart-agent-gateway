//go:build windows

package config

// Windows carries no inference engine, so a personal installation there has no
// local models: no node is started, the Machines screen is not offered, and
// nothing on the product hints at a capability that is not present.
//
// WHY, in one line: a graphics build is compiled for ONE compute capability and
// there are seven of them (KB/35), so an installer either asks a person which
// card they own or ships something three orders of magnitude slower than the one
// they expected. Both are worse than saying plainly that models are hosted.
//
// This is about the ENGINE, not about the platform's ability to use models.
// Windows reaches every hosted vendor exactly as the other platforms do, and a
// deployment running on Windows still has machines JOIN it over the network:
// what is absent is an engine inside this application.
const EngineBundled = false
