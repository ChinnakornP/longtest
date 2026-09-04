/**
 * Delimiters for content that came off the application under test.
 *
 * Anything the browser read from a page reaches an AI CLI wrapped in these
 * markers, and the system prompt states that their contents are data to be
 * described, never instructions to follow. The wrapper strips any occurrence
 * of the markers from the payload itself, so a page cannot close the block
 * early and continue as if it were the operator speaking.
 */
export const UNTRUSTED_START = '<<<UNTRUSTED_PAGE_CONTENT>>>';
export const UNTRUSTED_END = '<<<END_UNTRUSTED_PAGE_CONTENT>>>';

export function wrapUntrusted(source: string, content: string): string {
  const safe = content.split(UNTRUSTED_START).join('').split(UNTRUSTED_END).join('');
  return [`${UNTRUSTED_START} source=${JSON.stringify(source)}`, safe, UNTRUSTED_END].join('\n');
}
