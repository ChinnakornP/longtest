import { render, screen } from '@testing-library/react';
import { beforeAll, describe, expect, it } from 'vitest';

import type { RunEventRecord } from '@/lib/api/qa-types';

import { RunEventLog } from './run-event-log';

function makeEvents(count: number): RunEventRecord[] {
  return Array.from({ length: count }, (_, i) => ({
    seq: i + 1,
    type: 'run.event',
    ts: new Date().toISOString(),
    payload: { phase: 'execute', level: 'info', code: 'test_passed', message: `event ${i}` },
  }));
}

describe('RunEventLog', () => {
  beforeAll(() => {
    // jsdom reports 0 for layout metrics; give the virtualizer a realistic viewport to size against.
    Object.defineProperty(HTMLElement.prototype, 'clientHeight', { configurable: true, value: 400 });
    Object.defineProperty(HTMLElement.prototype, 'offsetHeight', { configurable: true, value: 400 });
  });

  it('mounts far fewer rows than the event count for a 10,000-event run', () => {
    const events = makeEvents(10_000);
    const { container } = render(<RunEventLog events={events} />);

    const rows = container.querySelectorAll('[data-index]');
    expect(rows.length).toBeGreaterThan(0);
    // ~400px viewport / 32px rows + overscan is a few dozen rows, nowhere near 10,000 - this is the proof the log is virtualized rather than fully mounted.
    expect(rows.length).toBeLessThan(200);
  });

  it('renders the first events with their message text', () => {
    const events = makeEvents(50);
    render(<RunEventLog events={events} />);
    expect(screen.getByText('event 0')).toBeInTheDocument();
  });

  it('shows an empty state when there are no events yet', () => {
    render(<RunEventLog events={[]} />);
    expect(screen.getByText(/no events yet/i)).toBeInTheDocument();
  });
});
