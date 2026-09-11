package app

import "github.com/rs/zerolog"

// A running commentary of work crossing a process boundary.
//
// A fleet happens in three places at once: a Gateway dispatches, a worker runs
// an agent, and a master hears about it. Each keeps its own log, and none of
// them on its own tells you what happened. So every step says the same shape of
// thing, tagged the same way, and `tail -f` on both processes reads as one
// story:
//
//	fleet  dispatched      fleet=3 agents=3
//	worker took a job      job=job_x kind=agent.run
//	worker running agent   fleet=3 del=13 agent=researcher
//	worker agent finished  fleet=3 del=13 outcome=done
//	worker reported        fleet=3 del=13 kind=done
//	master report          fleet=3 del=13 progress=1/3
//	master report          fleet=3 del=13 progress=3/3 complete=true
//	master batch closed    fleet=3
//	master turn scheduled  fleet=3
//
// Off unless SAG_VERBOSE_EVENTS is set, because it is one line per event per
// agent and a fleet of twenty is loud. It is a switch rather than a log level
// so it can be turned on without turning on everything else.
func (a *App) event() *zerolog.Event {
	if !a.Config.VerboseEvents {
		nop := zerolog.Nop()
		return nop.Info()
	}
	return a.Log.Info().Str("ev", "fleet")
}
