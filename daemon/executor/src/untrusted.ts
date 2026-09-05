/**
 * @fileoverview Framing for content that came off the application under test.
 *
 * This is the TypeScript half of a boundary whose other half is Go
 * (`daemon/security/untrusted.go`). The executor frames what it read off a
 * page; the daemon frames what it read off a download or a network log; both
 * end up in one prompt, and a model reading two different framings is a model
 * being taught that the framing is negotiable.
 *
 * The two implementations are held to byte-for-byte agreement by a shared
 * vector file — `daemon/security/testdata/untrusted-vectors.json` — checked
 * from both `daemon/security/parity_test.go` and `test/untrusted.test.ts`.
 * Change one side and the other side's test fails.
 */

export const UNTRUSTED_START = '<<<UNTRUSTED_PAGE_CONTENT';
export const UNTRUSTED_END = '<<<END_UNTRUSTED_PAGE_CONTENT';
export const UNTRUSTED_CLOSE = '>>>';

/** Per-block cap. Mirrors DefaultMaxBytes in the Go implementation. */
export const UNTRUSTED_MAX_BYTES = 16 * 1024;

const ESC = '\x1b';
const BEL = '\x07';

/** Where a block of untrusted content came from. Mirrors security.Kind. */
export type UntrustedKind =
  | 'dom_text'
  | 'dom_html'
  | 'accessibility_tree'
  | 'console_log'
  | 'network_log'
  | 'http_response_body'
  | 'downloaded_file'
  | 'filename'
  | 'page_title'
  | 'url'
  | 'unknown';

export interface UntrustedBlock {
  /** Per-run frame id. A page cannot observe it, so it cannot forge a frame. */
  nonce: string;
  kind?: UntrustedKind;
  /** Origin of the bytes. Itself untrusted, so it is emitted JSON-quoted. */
  source: string;
  content: string;
  maxBytes?: number;
}

const utf8 = new TextEncoder();
const utf8Decode = new TextDecoder('utf-8', { fatal: false });

/**
 * Render a block into the exact text that may be placed in a prompt.
 *
 * Deterministic for a given block: the same input produces the same bytes,
 * which is what lets the injection corpus assert that a prompt's instruction
 * region never varies with page content.
 */
export function wrapBlock(block: UntrustedBlock): string {
  const max = block.maxBytes && block.maxBytes > 0 ? block.maxBytes : UNTRUSTED_MAX_BYTES;

  let safe = sanitize(block.content, block.nonce);
  const rawLen = utf8.encode(safe).length;
  let truncated = false;
  if (rawLen > max) {
    safe = truncateUtf8(safe, max);
    truncated = true;
  }

  const kind = block.kind ?? 'unknown';
  const header =
    `${UNTRUSTED_START} id=${quote(block.nonce)} kind=${quote(kind)} ` +
    `source=${quote(block.source)} bytes=${rawLen} truncated=${truncated}${UNTRUSTED_CLOSE}`;
  const body = safe.endsWith('\n') ? safe : `${safe}\n`;
  return `${header}\n${body}${UNTRUSTED_END} id=${quote(block.nonce)}${UNTRUSTED_CLOSE}`;
}

/**
 * @deprecated Prefer {@link wrapBlock}. A block without a nonce is strictly
 * weaker — the closing marker carries nothing for the model to check the
 * opening one against — so every call site should be converted as the daemon
 * starts threading a run nonce through (T09).
 */
export function wrapUntrusted(source: string, content: string): string {
  return wrapBlock({ nonce: '', source, content });
}

/** JSON string literal. `>` is escaped so an attribute cannot emit `>>>`. */
function quote(s: string): string {
  let out = '"';
  for (const ch of s) {
    const code = ch.codePointAt(0) as number;
    switch (ch) {
      case '"':
        out += '\\"';
        break;
      case '\\':
        out += '\\\\';
        break;
      case '\n':
        out += '\\n';
        break;
      case '\r':
        out += '\\r';
        break;
      case '\t':
        out += '\\t';
        break;
      case '>':
        out += '\\u003e';
        break;
      default:
        if (code < 0x20 || code === 0x7f || isInvisible(code)) {
          out += '\\ufffd';
        } else {
          out += ch;
        }
    }
  }
  return `${out}"`;
}

function sanitize(content: string, nonce: string): string {
  let s = stripMarkers(content);
  if (nonce !== '') s = s.split(nonce).join('');
  s = stripAnsi(s);
  return stripControlAndInvisible(s);
}

function stripMarkers(s: string): string {
  for (const marker of [UNTRUSTED_END, UNTRUSTED_START]) {
    s = removeFold(s, marker);
  }
  return s;
}

/**
 * Remove every case-insensitive occurrence of `marker`, along with the
 * attribute tail up to a nearby `>>>`. The 256-character bound matters: an
 * unterminated `<<<UNTRUSTED` must not swallow the rest of the page.
 */
function removeFold(s: string, marker: string): string {
  const lowerMarker = marker.toLowerCase();
  let out = '';
  for (;;) {
    const i = s.toLowerCase().indexOf(lowerMarker);
    if (i < 0) return out + s;
    out += s.slice(0, i);
    let rest = s.slice(i + marker.length);
    const j = rest.indexOf(UNTRUSTED_CLOSE);
    if (j >= 0 && j <= 256) rest = rest.slice(j + UNTRUSTED_CLOSE.length);
    s = rest;
  }
}

/** CSI and OSC escape sequences: a page must not repaint the operator's terminal. */
function stripAnsi(s: string): string {
  let out = '';
  for (let i = 0; i < s.length; ) {
    if (s[i] !== ESC) {
      out += s[i];
      i++;
      continue;
    }
    i++;
    if (i >= s.length) break;
    if (s[i] === '[') {
      // CSI: parameter bytes until a final byte in @-~.
      i++;
      while (i < s.length && (s.charCodeAt(i) < 0x40 || s.charCodeAt(i) > 0x7e)) i++;
      if (i < s.length) i++;
    } else if (s[i] === ']') {
      // OSC: until BEL or the two-character string terminator.
      i++;
      while (i < s.length && s[i] !== BEL) {
        if (s[i] === ESC && s[i + 1] === '\\') {
          i++;
          break;
        }
        i++;
      }
      if (i < s.length) i++;
    } else {
      i++;
    }
  }
  return out;
}

function stripControlAndInvisible(s: string): string {
  let out = '';
  for (const ch of s) {
    const code = ch.codePointAt(0) as number;
    if (ch === '\n' || ch === '\t') {
      out += ch;
    } else if (ch === '\r') {
      // A lone CR can hide a line from a terminal reader.
      out += '\n';
    } else if (code < 0x20 || (code >= 0x7f && code <= 0x9f)) {
      // C0 and C1 controls carry no meaning in page text.
    } else if (isInvisible(code)) {
      // Bidi overrides, zero-width characters and tag characters are the
      // standard way to hide an instruction from a human reviewer while
      // leaving it legible to a tokenizer.
    } else {
      out += ch;
    }
  }
  return out;
}

const formatCharacter = /\p{Cf}/u;

function isInvisible(code: number): boolean {
  return (
    code === 0x00ad ||
    (code >= 0x200b && code <= 0x200f) ||
    (code >= 0x202a && code <= 0x202e) ||
    (code >= 0x2060 && code <= 0x2064) ||
    (code >= 0x2066 && code <= 0x2069) ||
    code === 0xfeff ||
    (code >= 0xfe00 && code <= 0xfe0f) ||
    (code >= 0xe0000 && code <= 0xe007f) ||
    (code >= 0xe0100 && code <= 0xe01ef) ||
    // Remaining Unicode format characters (category Cf) — U+061C, U+180E, the
    // interlinear annotations at U+FFF9..FFFB, the musical controls at
    // U+1D173..1D17A. Matches unicode.Is(unicode.Cf, r) on the Go side.
    formatCharacter.test(String.fromCodePoint(code))
  );
}

const TRUNCATION_NOTE = '\n[truncated by qa-daemon]';

/** Truncate to `max` UTF-8 bytes without splitting a code point. */
function truncateUtf8(s: string, max: number): string {
  const noteLen = utf8.encode(TRUNCATION_NOTE).length;
  const budget = max - noteLen;
  if (budget <= 0) return TRUNCATION_NOTE;

  const bytes = utf8.encode(s);
  if (bytes.length <= budget) return s;
  let cut = budget;
  // 0b10xxxxxx is a continuation byte; back up to the start of the code point.
  while (cut > 0 && ((bytes[cut] ?? 0) & 0xc0) === 0x80) cut--;
  return utf8Decode.decode(bytes.subarray(0, cut)) + TRUNCATION_NOTE;
}
