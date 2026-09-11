//go:build !windows

package config

// EngineBundled says this build carries an inference engine, so a personal
// installation can run a model on its own hardware.
//
// It is a CONSTANT and not a setting, because it is a fact about what was
// compiled rather than a choice somebody makes afterwards. A build tag is the
// only honest home for it: on a platform we do not ship an engine for there is
// no file to look for, no probe to run and no state that could make the answer
// come out differently on the second read.
//
// What reads it: the posture, which is how the console knows not to offer local
// models, and the personal boot, which is how nothing is started to serve them.
// See KB/36 for why Windows is the platform without one.
const EngineBundled = true
