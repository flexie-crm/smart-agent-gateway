import { useCallback, useEffect, useRef, useState } from 'react'

/**
 * Recording what somebody says, so they can talk instead of typing.
 *
 * The whole of it is a MediaRecorder and a stopwatch, with one rule that is
 * easy to get wrong and unpleasant when you do: the microphone must be RELEASED
 * on every path out. A track left running is a browser tab showing a recording
 * indicator long after the person stopped, which is alarming and reasonably so.
 * So the tracks are stopped when the recording ends, when it is cancelled, and
 * when the component goes away mid-recording.
 */

/** The state a recording can be in, which is what the composer renders from. */
export type RecorderState = 'idle' | 'starting' | 'recording' | 'stopping'

/**
 * How many bars the meter holds. The oldest falls off the left as it fills.
 *
 * Enough of them that the strip reads as a waveform rather than as a row of
 * ticks: they are laid out with the space between them shared out, so the count
 * sets the density and the container sets the width.
 */
export const METER_BARS = 110

export interface Recorder {
  state: RecorderState
  /** How long it has been running, in whole seconds. */
  seconds: number
  /**
   * How loud it has been, newest last, each 0..1.
   *
   * It is what somebody looks at to know the microphone is hearing them, which
   * is the one thing a timer alone cannot tell them: a muted input counts up
   * just as happily as a working one.
   */
  levels: number[]
  /** Why it could not start, in words a person can act on. */
  error: string | null
  start: () => Promise<void>
  /** stop ends the recording and hands back what was said. */
  stop: () => Promise<File | null>
  /** cancel throws the recording away and releases the microphone. */
  cancel: () => void
}

/**
 * The container to record in, picked from what this browser will actually
 * produce rather than from what we would prefer.
 *
 * Opus in WebM everywhere it exists; Safari has neither and does MP4. The
 * EXTENSION matters as much as the type: the transcription endpoint reads it to
 * know how to decode the bytes, so the file has to arrive called what it is.
 */
function container(): { mimeType: string; extension: string } | null {
  const candidates = [
    { mimeType: 'audio/webm;codecs=opus', extension: 'webm' },
    { mimeType: 'audio/webm', extension: 'webm' },
    { mimeType: 'audio/mp4', extension: 'mp4' },
    { mimeType: 'audio/ogg;codecs=opus', extension: 'ogg' },
  ]
  for (const c of candidates) {
    if (MediaRecorder.isTypeSupported(c.mimeType)) return c
  }
  return null
}

/** canRecord is whether this browser offers a microphone at all. */
export function canRecord(): boolean {
  return (
    typeof window !== 'undefined' &&
    typeof window.MediaRecorder !== 'undefined' &&
    Boolean(navigator.mediaDevices?.getUserMedia)
  )
}

export function useRecorder(): Recorder {
  const [state, setState] = useState<RecorderState>('idle')
  const [seconds, setSeconds] = useState(0)
  const [error, setError] = useState<string | null>(null)

  const [levels, setLevels] = useState<number[]>([])

  const recorderRef = useRef<MediaRecorder | null>(null)
  const streamRef = useRef<MediaStream | null>(null)
  const chunksRef = useRef<Blob[]>([])
  const extensionRef = useRef('webm')
  const audioRef = useRef<AudioContext | null>(null)
  const frameRef = useRef<number | null>(null)

  /** release stops the microphone. Every path out goes through here. */
  const release = useCallback(() => {
    if (frameRef.current !== null) cancelAnimationFrame(frameRef.current)
    frameRef.current = null
    void audioRef.current?.close().catch(() => {})
    audioRef.current = null
    streamRef.current?.getTracks().forEach((t) => t.stop())
    streamRef.current = null
    recorderRef.current = null
    chunksRef.current = []
    setLevels([])
  }, [])

  // A component that goes away mid-recording must not leave the microphone on.
  useEffect(() => release, [release])

  // The stopwatch, which exists so somebody can see that it is listening.
  useEffect(() => {
    if (state !== 'recording') return
    const started = Date.now()
    setSeconds(0)
    const id = window.setInterval(() => {
      setSeconds(Math.floor((Date.now() - started) / 1000))
    }, 250)
    return () => window.clearInterval(id)
  }, [state])

  /**
   * meter watches how loud the microphone is and keeps the last stretch of it.
   *
   * It reads the waveform rather than the frequency bins and takes the RMS,
   * which is loudness as an ear judges it; a spectrum would be prettier and
   * would not answer the question being asked, which is "can it hear me".
   *
   * Sampled on a timer rather than every animation frame. Sixty state updates a
   * second to move some bars is a waste of a laptop battery, and the eye cannot
   * tell.
   */
  const meter = useCallback((stream: MediaStream) => {
    let context: AudioContext
    try {
      context = new AudioContext()
    } catch {
      return // No meter. The recording itself is unaffected.
    }
    audioRef.current = context
    const analyser = context.createAnalyser()
    analyser.fftSize = 1024
    context.createMediaStreamSource(stream).connect(analyser)

    const samples = new Uint8Array(analyser.fftSize)
    let last = 0
    const tick = (now: number) => {
      frameRef.current = requestAnimationFrame(tick)
      if (now - last < 55) return
      last = now

      analyser.getByteTimeDomainData(samples)
      let sum = 0
      for (const sample of samples) {
        const centred = (sample - 128) / 128
        sum += centred * centred
      }
      // Root mean square, then a gentle curve: quiet speech should still show
      // as something, and a shout should not peg every bar to the ceiling.
      const rms = Math.sqrt(sum / samples.length)
      const level = Math.min(1, Math.pow(rms * 3.2, 0.7))
      setLevels((prev) => [...prev, level].slice(-METER_BARS))
    }
    frameRef.current = requestAnimationFrame(tick)
  }, [])

  const start = useCallback(async () => {
    setError(null)
    const chosen = container()
    if (!canRecord() || !chosen) {
      setError('This browser cannot record audio.')
      return
    }
    setState('starting')
    try {
      const stream = await navigator.mediaDevices.getUserMedia({ audio: true })
      const recorder = new MediaRecorder(stream, { mimeType: chosen.mimeType })
      chunksRef.current = []
      recorder.ondataavailable = (e) => {
        if (e.data.size > 0) chunksRef.current.push(e.data)
      }
      streamRef.current = stream
      recorderRef.current = recorder
      extensionRef.current = chosen.extension
      recorder.start()
      meter(stream)
      setState('recording')
    } catch (failure) {
      release()
      setState('idle')
      // The common one by far is the person saying no, or having said no once
      // and the browser remembering. Both read the same from here.
      const denied =
        failure instanceof DOMException &&
        (failure.name === 'NotAllowedError' || failure.name === 'SecurityError')
      setError(
        denied
          ? 'The microphone is blocked for this site. Allow it in your browser to talk instead of typing.'
          : 'The microphone could not be started.',
      )
    }
  }, [release, meter])

  const stop = useCallback(async (): Promise<File | null> => {
    const recorder = recorderRef.current
    if (!recorder || recorder.state === 'inactive') {
      release()
      setState('idle')
      return null
    }
    setState('stopping')

    const blob = await new Promise<Blob>((resolve) => {
      recorder.onstop = () => resolve(new Blob(chunksRef.current, { type: recorder.mimeType }))
      recorder.stop()
    })
    const extension = extensionRef.current
    release()
    setState('idle')

    if (blob.size === 0) return null
    return new File([blob], `recording.${extension}`, { type: blob.type })
  }, [release])

  const cancel = useCallback(() => {
    const recorder = recorderRef.current
    if (recorder && recorder.state !== 'inactive') {
      // No onstop handler: whatever was said is being thrown away.
      recorder.onstop = null
      recorder.stop()
    }
    release()
    setState('idle')
    setError(null)
  }, [release])

  return { state, seconds, levels, error, start, stop, cancel }
}

/** clock is the elapsed time as somebody reads it. */
export function clock(seconds: number): string {
  const m = Math.floor(seconds / 60)
  const s = seconds % 60
  return `${m}:${String(s).padStart(2, '0')}`
}
