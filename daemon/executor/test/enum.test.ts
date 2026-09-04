/**
 * Action/assertion enum coverage.
 *
 * The contract freezes `STEP_ACTION_VALUES` and `ASSERTION_TYPE_VALUES` at v1.
 * The runtime must accept every member and reject everything else. We do not
 * need Chromium for these — the dispatch table is plain TypeScript — but
 * the runner does, so we wire a stub `page` that records what was asked of
 * it.
 */

import { describe, expect, it } from 'vitest';
import { STEP_ACTION_VALUES, ASSERTION_TYPE_VALUES } from '@qa/schema';
import { runTestCase, ExecutorError } from '../src/runner.ts';
import { Session } from '../src/session.ts';
import type { ApplicationMap, TestCase } from '@qa/schema';

/** Minimal app map covering all the refs the test cases below reference. */
const appMap: ApplicationMap = {
  version: 1,
  baseUrl: 'http://localhost:9999',
  pages: [
    {
      id: 'page.employees',
      path: '/employees',
      title: 'Employees',
      elements: [
        { ref: 'emp.btn.add', type: 'button', locators: [{ kind: 'testId', value: 'add-emp' }], lastSeenRunId: '00000000-0000-0000-0000-000000000001' },
        { ref: 'emp.btn.save', type: 'button', locators: [{ kind: 'testId', value: 'employee-save' }], lastSeenRunId: '00000000-0000-0000-0000-000000000001' },
        { ref: 'emp.table', type: 'table', locators: [{ kind: 'testId', value: 'employee-table' }], lastSeenRunId: '00000000-0000-0000-0000-000000000001' },
        { ref: 'emp.input.first', type: 'input', locators: [{ kind: 'testId', value: 'employee-first-name' }], lastSeenRunId: '00000000-0000-0000-0000-000000000001' },
      ],
    },
  ],
  workflows: [],
};

/** A test case whose every step uses one of the v1 actions. */
const baseTestCase = (action: typeof STEP_ACTION_VALUES[number], target?: { ref: string }): TestCase => {
  let step: TestCase['steps'][number];
  const t = target ?? { ref: 'emp.btn.add' };
  if (action === 'navigate') {
    step = { action, url: '/employees' };
  } else if (action === 'screenshot') {
    step = { action };
  } else if (action === 'press') {
    step = { action, target: t, key: 'Enter' };
  } else if (action === 'select') {
    step = { action, target: t, value: 'option' };
  } else if (action === 'fill') {
    step = { action, target: t, value: 'x' };
  } else if (action === 'waitFor') {
    step = { action, target: t, state: 'visible' };
  } else {
    step = { action, target: t };
  }
  return {
    version: 1,
    id: `TC-${action}`,
    name: `exercise ${action}`,
    priority: 'medium',
    category: 'functional',
    preconditions: [],
    steps: [step],
    assertions: [{ type: 'visible', target: { ref: 'emp.btn.add' } }],
  };
};

describe('action enum v1', () => {
  it.each(STEP_ACTION_VALUES)('runs action "%s" without enum error', async (action) => {
    const session = new SessionStub();
    try {
      const result = await runTestCase(session.asSession(), {
        testCase: baseTestCase(action),
        appMap,
        artifactDir: '/tmp/qe-test',
        storageKeyPrefix: 'orgs/test/runs/2026-09-04/run-1',
        fixtureCredentials: {},
      });
      // We never assert the verdict — the stub page answers every locator
      // as "1 match". What matters is that the runner did not throw
      // UNKNOWN_ACTION.
      expect(result.result).toBeDefined();
    } catch (error) {
      if (error instanceof ExecutorError) {
        expect(error.code).not.toBe('UNKNOWN_ACTION');
      } else {
        throw error;
      }
    }
  });
});

describe('assertion enum v1', () => {
  // The runner drives assertions after a successful step. We give it a
  // trivially-passes step and exercise one assertion type per test.
  it.each(ASSERTION_TYPE_VALUES)('runs assertion "%s" without enum error', async (type) => {
    const session = new SessionStub();
    const testCase: TestCase = {
      version: 1,
      id: `TC-A-${type}`,
      name: `exercise ${type}`,
      priority: 'medium',
      category: 'validation',
      preconditions: [],
      steps: [{ action: 'navigate', url: '/employees' }],
      assertions: [{
        type,
        ...(type === 'textEquals' || type === 'textContains' ? { target: { ref: 'emp.btn.add' }, value: 'x' } : {}),
        ...(type === 'urlMatches' ? { value: '^/employees$' } : {}),
        ...(type === 'elementCount' ? { target: { ref: 'emp.btn.add' }, value: 1 } : {}),
        ...(type === 'httpStatusNot' ? { value: 500 } : {}),
        ...(type === 'visible' || type === 'hidden' ? { target: { ref: 'emp.btn.add' } } : {}),
        ...(type === 'noConsoleError' ? {} : {}),
      }] as TestCase['assertions'],
    };
    try {
      const result = await runTestCase(session.asSession(), {
        testCase,
        appMap,
        artifactDir: '/tmp/qe-test',
        storageKeyPrefix: 'orgs/test/runs/2026-09-04/run-1',
        fixtureCredentials: {},
      });
      expect(result.result).toBeDefined();
    } catch (error) {
      if (error instanceof ExecutorError) {
        expect(error.code).not.toBe('UNKNOWN_ASSERTION_TYPE');
      } else {
        throw error;
      }
    }
  });
});

/**
 * Hand-rolled session stub. The real `Session` would spin up Chromium; for
 * enum-coverage tests we only need an object whose `getPage`, `getRequests`
 * etc. behave well enough for the runner to walk through every code path
 * without throwing `INTERNAL`.
 */
class SessionStub {
  asSession(): Session {
    const fake = {
      baseUrl: 'http://localhost:9999',
      traceDir: '/tmp/qe-test-trace',
      getPage: () => makeFakePage(),
      getRequests: () => [],
      getConsoleMessages: () => [],
      currentUrl: () => 'http://localhost:9999/employees',
      captureStorageState: async () => undefined,
      finalizeTrace: async () => '/tmp/qe-test-trace/trace.zip',
      close: async () => undefined,
      isOpen: () => true,
    };
    return fake as unknown as Session;
  }
}

function makeFakePage(): import('playwright').Page {
  const locator = makeFakeLocator();
  const fakePage = {
    url: () => 'http://localhost:9999/employees',
    goto: async () => undefined,
    locator: () => locator,
    on: () => fakePage,
    screenshot: async () => Buffer.from([]),
    waitForURL: async () => undefined,
    getByLabel: (_: string) => locator,
    getByRole: (_: string) => locator,
    getByPlaceholder: (_: string) => locator,
    getByText: (_: string) => locator,
    getByAltText: (_: string) => locator,
    keyboard: { press: async () => undefined },
  } as unknown as import('playwright').Page;
  return fakePage;
}

function makeFakeLocator(): import('playwright').Locator {
  return {
    count: async () => 1,
    first: () => makeFakeLocator(),
    click: async () => undefined,
    fill: async () => undefined,
    selectOption: async () => undefined,
    isChecked: async () => false,
    hover: async () => undefined,
    press: async () => undefined,
    waitFor: async () => undefined,
    textContent: async () => 'X',
    isVisible: async () => true,
    locator: () => makeFakeLocator(),
    getByRole: () => makeFakeLocator(),
    getByTestId: () => makeFakeLocator(),
    getByLabel: () => makeFakeLocator(),
    getByPlaceholder: () => makeFakeLocator(),
    getByText: () => makeFakeLocator(),
    getByAltText: () => makeFakeLocator(),
  } as unknown as import('playwright').Locator;
}
