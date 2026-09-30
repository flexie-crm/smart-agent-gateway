import { afterEach, describe, expect, it } from 'vitest'
import { cleanup, render } from '@testing-library/react'
import { Reasoning, ReasoningTrigger } from '../reasoning'
import { ToolRow } from '../tool-timeline'
import { ConfirmBlock } from '../confirm-block'
import { TIMELINE_CHEVRON } from '../timeline-chevron'

// Every open-and-close arrow in the chat's timeline is one size. They were
// 20, 14 and 14 pixels, each chosen where it was drawn, and one of them was
// sized by an inline style that beat its own class. So what is asserted is the
// shared class AND the absence of anything inline that would override it.

afterEach(cleanup)

function arrowIn(container: HTMLElement): SVGElement {
  const arrow = container.querySelector('svg.lucide-chevron-down, svg.lucide-chevron-right') as SVGElement | null
  if (!arrow) throw new Error('no arrow was drawn, so this proves nothing')
  return arrow
}

function expectTheTimelineSize(arrow: SVGElement) {
  for (const cls of TIMELINE_CHEVRON.split(' ')) {
    expect(arrow.getAttribute('class')).toContain(cls)
  }
  expect(arrow.getAttribute('style') ?? '').not.toMatch(/width|height/)
  expect(arrow.getAttribute('class')).not.toMatch(/\bsize-(3|3\.5|4)\b/)
}

describe('the arrows in the chat timeline', () => {
  it('are 18 pixels on the thinking row', () => {
    const { container } = render(
      <Reasoning hasReasoning duration={9}>
        <ReasoningTrigger />
      </Reasoning>,
    )
    expectTheTimelineSize(arrowIn(container))
  })

  it('are 18 pixels on a tool row', () => {
    const { container } = render(
      <ToolRow tool={{ id: '42', name: 'terminal', friendly_name: 'Terminal', status: 'completed', duration_ms: 670 }} />,
    )
    expectTheTimelineSize(arrowIn(container))
  })

  it('are 18 pixels on an approval card\'s details', () => {
    const { container } = render(
      <ConfirmBlock
        confirmation={{
          token: 't', title: 'Run a command', description: 'It will run this.',
          details: { command: 'git status', folder: '/tmp', timeout: 30 },
        }}
        onRespond={() => {}}
      />,
    )
    expectTheTimelineSize(arrowIn(container))
  })

  it('is 18 pixels', () => {
    expect(TIMELINE_CHEVRON).toContain('size-[18px]')
  })
})
