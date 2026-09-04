/**
 * Locator fallback chain: the executor's only view of "where".
 *
 * ADR-004 defines the chain order. These tests cover every kind, the
 * ambiguous case, the missing-ref case, and the raw-locator escape hatch.
 * They use a stub `Page.locator` so they run without spinning up Chromium
 * — the integration tests cover the actual browser interaction.
 */

import { describe, expect, it } from 'vitest';
import type { Locator } from '@qa/schema';
import { indexApplicationMap, locatorToPlaywright, resolveTarget } from '../src/locator.ts';
import type { Locator as PlaywrightLocator } from 'playwright';

interface StubLocator {
  count: () => Promise<number>;
  readonly description: string;
}

class StubPage {
  constructor(private readonly stubs: ReadonlyMap<string, StubLocator>) {}
  // The root selector is whatever the resolver uses to anchor the chain —
  // we do not need to verify it, just always return a usable locator.
  locator(selector: string): unknown {
    const stub = this.stubs.get(selector) ?? this.stubs.get('__root__');
    return {
      count: stub === undefined ? (async () => 0) : stub.count,
      locator: (next: string) => this.locator(next),
      getByTestId: (value: string) => this.lookup(`testId:${value}`),
      getByRole: (role: string, opts: { name: string; exact: boolean }) => this.lookup(`role:${role}:${opts.name}`),
      getByLabel: (value: string) => this.lookup(`label:${value}`),
      getByPlaceholder: (value: string) => this.lookup(`placeholder:${value}`),
      getByText: (value: string) => this.lookup(`text:${value}`),
      getByAltText: (value: string) => this.lookup(`altText:${value}`),
    };
  }
  private lookup(key: string): unknown {
    const stub = this.stubs.get(key);
    return {
      count: async () => (stub === undefined ? 0 : stub.count()),
      locator: (next: string) => this.locator(next),
      getByTestId: (value: string) => this.lookup(`testId:${value}`),
      getByRole: (role: string, opts: { name: string; exact: boolean }) => this.lookup(`role:${role}:${opts.name}`),
      getByLabel: (value: string) => this.lookup(`label:${value}`),
      getByPlaceholder: (value: string) => this.lookup(`placeholder:${value}`),
      getByText: (value: string) => this.lookup(`text:${value}`),
      getByAltText: (value: string) => this.lookup(`altText:${value}`),
    };
  }
}

const appMap = {
  version: 1 as const,
  baseUrl: 'https://demo.example.test',
  pages: [
    {
      id: 'page.employees',
      path: '/employees',
      title: 'Employees',
      elements: [
        { ref: 'emp.btn.add', type: 'button' as const, label: 'Add Employee', locators: [{ kind: 'testId' as const, value: 'add-emp' }] as Locator[], lastSeenRunId: '00000000-0000-0000-0000-000000000001' },
        { ref: 'emp.btn.add.role', type: 'button' as const, label: 'Add Employee', locators: [{ kind: 'role' as const, value: 'button', name: 'Add Employee' }] as Locator[], lastSeenRunId: '00000000-0000-0000-0000-000000000001' },
        { ref: 'emp.btn.add.label', type: 'button' as const, label: 'Add Employee', locators: [{ kind: 'label' as const, value: 'Add Employee' }] as Locator[], lastSeenRunId: '00000000-0000-0000-0000-000000000001' },
        { ref: 'emp.btn.add.text', type: 'button' as const, label: 'Add Employee', locators: [{ kind: 'text' as const, value: 'Add Employee' }] as Locator[], lastSeenRunId: '00000000-0000-0000-0000-000000000001' },
        { ref: 'emp.btn.add.css', type: 'button' as const, label: 'Add Employee', locators: [{ kind: 'css' as const, value: '.add-button' }] as Locator[], lastSeenRunId: '00000000-0000-0000-0000-000000000001' },
        { ref: 'emp.btn.add.placeholder', type: 'input' as const, label: 'Search', locators: [{ kind: 'placeholder' as const, value: 'Search employees' }] as Locator[], lastSeenRunId: '00000000-0000-0000-0000-000000000001' },
        { ref: 'emp.btn.add.altText', type: 'image' as const, label: 'Logo', locators: [{ kind: 'altText' as const, value: 'Company logo' }] as Locator[], lastSeenRunId: '00000000-0000-0000-0000-000000000001' },
        { ref: 'emp.btn.add.title', type: 'button' as const, label: 'Help', locators: [{ kind: 'title' as const, value: 'Click for help' }] as Locator[], lastSeenRunId: '00000000-0000-0000-0000-000000000001' },
      ],
    },
  ],
  workflows: [],
};

const indexed = indexApplicationMap(appMap);

describe('indexApplicationMap', () => {
  it('indexes refs across pages and remembers the page that owns them', () => {
    expect(indexed.byRef.has('emp.btn.add')).toBe(true);
    expect(indexed.byRef.get('emp.btn.add')?.pagePath).toBe('/employees');
    expect(indexed.pageByPath.get('/employees')?.id).toBe('page.employees');
  });
});

describe('resolveTarget', () => {
  it('resolves a testId locator to a single element', async () => {
    const page = new StubPage(new Map([
      ['testId:add-emp', { count: async () => 1, description: 'testId' }],
    ]));
    const result = await resolveTarget(page as unknown as { locator: (selector: string) => PlaywrightLocator }, indexed, { ref: 'emp.btn.add' });
    expect('locator' in result).toBe(true);
    if ('locator' in result) {
      expect(result.description).toBe('testId:add-emp');
      expect(result.unstable).toBe(false);
    }
  });

  it('resolves a role+name locator', async () => {
    const page = new StubPage(new Map([
      ['role:button:Add Employee', { count: async () => 1, description: 'role' }],
    ]));
    const result = await resolveTarget(page as unknown as { locator: (selector: string) => PlaywrightLocator }, indexed, { ref: 'emp.btn.add.role' });
    expect('locator' in result).toBe(true);
    if ('locator' in result) expect(result.description).toBe('role:button:Add Employee');
  });

  it('resolves a label locator', async () => {
    const page = new StubPage(new Map([
      ['label:Add Employee', { count: async () => 1, description: 'label' }],
    ]));
    const result = await resolveTarget(page as unknown as { locator: (selector: string) => PlaywrightLocator }, indexed, { ref: 'emp.btn.add.label' });
    expect('locator' in result).toBe(true);
  });

  it('resolves a text locator', async () => {
    const page = new StubPage(new Map([
      ['text:Add Employee', { count: async () => 1, description: 'text' }],
    ]));
    const result = await resolveTarget(page as unknown as { locator: (selector: string) => PlaywrightLocator }, indexed, { ref: 'emp.btn.add.text' });
    expect('locator' in result).toBe(true);
  });

  it('resolves a css locator', async () => {
    const page = new StubPage(new Map([
      ['html', { count: async () => 1, description: 'root' }],
      ['.add-button', { count: async () => 1, description: 'css' }],
    ]));
    const result = await resolveTarget(page as unknown as { locator: (selector: string) => PlaywrightLocator }, indexed, { ref: 'emp.btn.add.css' });
    expect('locator' in result).toBe(true);
  });

  it('resolves a placeholder locator', async () => {
    const page = new StubPage(new Map([
      ['placeholder:Search employees', { count: async () => 1, description: 'placeholder' }],
    ]));
    const result = await resolveTarget(page as unknown as { locator: (selector: string) => PlaywrightLocator }, indexed, { ref: 'emp.btn.add.placeholder' });
    expect('locator' in result).toBe(true);
  });

  it('resolves an altText locator', async () => {
    const page = new StubPage(new Map([
      ['altText:Company logo', { count: async () => 1, description: 'altText' }],
    ]));
    const result = await resolveTarget(page as unknown as { locator: (selector: string) => PlaywrightLocator }, indexed, { ref: 'emp.btn.add.altText' });
    expect('locator' in result).toBe(true);
  });

  it('resolves a title locator via [title="..."]', async () => {
    const page = new StubPage(new Map([
      ['html', { count: async () => 1, description: 'root' }],
      ['[title="Click for help"]', { count: async () => 1, description: 'title' }],
    ]));
    const result = await resolveTarget(page as unknown as { locator: (selector: string) => PlaywrightLocator }, indexed, { ref: 'emp.btn.add.title' });
    expect('locator' in result).toBe(true);
    if ('locator' in result) expect(result.description).toBe('title:Click for help');
  });

  it('reports an unknown ref as no_match', async () => {
    const page = new StubPage(new Map([
      ['html', { count: async () => 1, description: 'root' }],
    ]));
    const result = await resolveTarget(page as unknown as { locator: (selector: string) => PlaywrightLocator }, indexed, { ref: 'does.not.exist' });
    expect('locator' in result).toBe(false);
    if (!('locator' in result)) {
      expect(result.reason).toBe('no_match');
      expect(result.tried).toEqual(['ref:does.not.exist']);
    }
  });

  it('reports ambiguous when a chain entry matches more than one element', async () => {
    const page = new StubPage(new Map([
      ['testId:add-emp', { count: async () => 2, description: 'testId' }],
    ]));
    const result = await resolveTarget(page as unknown as { locator: (selector: string) => PlaywrightLocator }, indexed, { ref: 'emp.btn.add' });
    expect('locator' in result).toBe(false);
    if (!('locator' in result)) {
      expect(result.reason).toBe('ambiguous');
      expect(result.tried[0]).toBe('testId:add-emp');
    }
  });

  it('falls back to the next chain entry when the first finds nothing', async () => {
    // element with two locators: first finds nothing, second finds one
    const map = {
      version: 1 as const,
      baseUrl: 'https://demo.example.test',
      pages: [
        {
          id: 'page.employees',
          path: '/employees',
          title: 'Employees',
          elements: [
            {
              ref: 'emp.btn.add',
              type: 'button' as const,
              label: 'Add Employee',
              locators: [
                { kind: 'testId' as const, value: 'missing' },
                { kind: 'text' as const, value: 'Add Employee' },
              ] as Locator[],
              lastSeenRunId: '00000000-0000-0000-0000-000000000001',
            },
          ],
        },
      ],
      workflows: [],
    };
    const local = indexApplicationMap(map);
    const page = new StubPage(new Map([
      ['testId:missing', { count: async () => 0, description: 'testId' }],
      ['text:Add Employee', { count: async () => 1, description: 'text' }],
    ]));
    const result = await resolveTarget(page as unknown as { locator: (selector: string) => PlaywrightLocator }, local, { ref: 'emp.btn.add' });
    expect('locator' in result).toBe(true);
    if ('locator' in result) {
      expect(result.description).toBe('text:Add Employee');
    }
  });

  it('flags a raw-locator target as unstable: true', async () => {
    const page = new StubPage(new Map([
      ['.btn', { count: async () => 1, description: 'raw' }],
    ]));
    const result = await resolveTarget(page as unknown as { locator: (selector: string) => PlaywrightLocator }, indexed, { locator: '.btn', unstable: true });
    expect('locator' in result).toBe(true);
    if ('locator' in result) {
      expect(result.unstable).toBe(true);
      expect(result.description).toBe('raw(.btn)');
    }
  });
});

describe('locatorToPlaywright', () => {
  it('returns a css locator for kind="css"', () => {
    const fakeRoot = {
      locator: (selector: string) => ({ selector }),
    } as unknown as Parameters<typeof locatorToPlaywright>[0];
    const out = locatorToPlaywright(fakeRoot, { kind: 'css', value: '.x' });
    expect(out).toEqual({ selector: '.x' });
  });
});
