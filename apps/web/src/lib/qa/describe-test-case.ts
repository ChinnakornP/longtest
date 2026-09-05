import type { ApplicationMap, Assertion, Element, Step, Target } from '@qa/schema';

/** ref -> element, flattened from every page's element list. */
export type ElementIndex = Map<string, Element>;

export function buildElementIndex(appMap: ApplicationMap | undefined): ElementIndex {
  const index: ElementIndex = new Map();
  for (const page of appMap?.pages ?? []) {
    for (const element of page.elements) {
      index.set(element.ref, element);
    }
  }
  return index;
}

/** A short, human-facing label for a step/assertion target — the element's label when the application map still has it, otherwise its ref or a note that it is an unstable ad-hoc selector. */
export function targetLabel(target: Target | undefined, elements: ElementIndex): string {
  if (!target) return '';
  if ('ref' in target) {
    const element = elements.get(target.ref);
    return element?.label ? `"${element.label}" (${target.ref})` : target.ref;
  }
  return `custom selector "${target.locator}" (unstable — not from the application map)`;
}

/** One readable sentence per step, for a reviewer who never wants to read the raw JSON. */
export function describeStep(step: Step, elements: ElementIndex): string {
  switch (step.action) {
    case 'navigate':
      return `Navigate to ${step.url}`;
    case 'click':
      return `Click ${targetLabel(step.target, elements)}`;
    case 'fill':
      return `Fill ${targetLabel(step.target, elements)} with "${step.value}"`;
    case 'select':
      return `Select "${step.value}" in ${targetLabel(step.target, elements)}`;
    case 'check':
      return `${step.checked === false ? 'Uncheck' : 'Check'} ${targetLabel(step.target, elements)}`;
    case 'hover':
      return `Hover over ${targetLabel(step.target, elements)}`;
    case 'press':
      return step.target ? `Press "${step.key}" on ${targetLabel(step.target, elements)}` : `Press "${step.key}"`;
    case 'waitFor':
      return `Wait for ${targetLabel(step.target, elements)} to be ${step.state}`;
    case 'screenshot':
      return step.name ? `Take a screenshot ("${step.name}")` : 'Take a screenshot';
    default:
      return (step as { action: string }).action;
  }
}

/** One readable sentence per assertion, mirroring describeStep. */
export function describeAssertion(assertion: Assertion, elements: ElementIndex): string {
  switch (assertion.type) {
    case 'visible':
      return `${targetLabel(assertion.target, elements)} is visible`;
    case 'hidden':
      return `${targetLabel(assertion.target, elements)} is hidden`;
    case 'textEquals':
      return `${targetLabel(assertion.target, elements)} text equals "${assertion.value}"`;
    case 'textContains':
      return `${targetLabel(assertion.target, elements)} text contains "${assertion.value}"`;
    case 'urlMatches':
      return `URL matches /${assertion.value}/`;
    case 'elementCount': {
      const operator = assertion.operator ?? 'eq';
      const comparator = operator === 'eq' ? 'equals' : operator === 'gte' ? 'is at least' : 'is at most';
      return `Count of ${targetLabel(assertion.target, elements)} ${comparator} ${assertion.value}`;
    }
    case 'httpStatusNot':
      return `HTTP status is not ${assertion.value}`;
    case 'noConsoleError':
      return 'No console error is logged';
    default:
      return (assertion as { type: string }).type;
  }
}
