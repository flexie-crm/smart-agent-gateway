// Diagnostic tracing for the background-completion delivery path (Mode C): a
// nudge over the socket, the client attaching to that run, and the result message
// painting. When a result fails to paint live (it is saved but not shown until a
// reload), these traces pin the loss to the exact step instead of guessing at it.
//
// Off by default, so it adds nothing to normal use. Turn it on in the browser
// console and reload:
//     localStorage.fx_trace_completions = '1'
// Then run a multi-background prompt; filter the console by "[completion]".
// Turn it off with:  delete localStorage.fx_trace_completions
export function traceCompletion(event: string, data?: Record<string, unknown>): void {
  try {
    if (localStorage.getItem('fx_trace_completions') !== '1') return
  } catch {
    return
  }
  const ts = new Date().toISOString().slice(11, 23) // HH:MM:SS.mmm
  // eslint-disable-next-line no-console
  console.log(`[completion ${ts}] ${event}`, data ?? '')
}
