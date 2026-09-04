import { cleanup } from '@testing-library/react';
import '@testing-library/jest-dom/vitest';
import { afterEach } from 'vitest';

// vitest.config.ts does not set `test.globals`, so @testing-library/react
// cannot auto-detect the test framework and register its own cleanup - do it
// explicitly, or a render from one test leaks into the next test's DOM (seen
// as spurious "multiple elements found" failures).
afterEach(() => {
  cleanup();
});

// jsdom has no ResizeObserver. @tanstack/react-virtual (RunEventLog) uses one
// to size its scroll container; a no-op stub is enough for tests that only
// need a stable, non-crashing render (the container height comes from the
// clientHeight/offsetHeight stubs each such test sets itself).
if (typeof globalThis.ResizeObserver === 'undefined') {
  class ResizeObserverStub {
    observe(): void {}
    unobserve(): void {}
    disconnect(): void {}
  }
  globalThis.ResizeObserver = ResizeObserverStub as unknown as typeof ResizeObserver;
}
