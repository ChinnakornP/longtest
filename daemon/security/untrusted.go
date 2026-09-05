package security

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Markers that frame page-derived content inside a prompt.
//
// They are a constant, not a secret: a page that guesses them still cannot
// forge a frame, because [Wrap] strips every occurrence of either marker from
// the payload and because the opening and closing markers both carry a
// per-run nonce the page has no way to observe.
const (
	MarkerStart = "<<<UNTRUSTED_PAGE_CONTENT"
	MarkerEnd   = "<<<END_UNTRUSTED_PAGE_CONTENT"
	MarkerClose = ">>>"
)

// DefaultMaxBytes bounds one untrusted block. A page can serve a megabyte of
// text; a prompt that carries it costs money, buries the real instructions,
// and gives an injection more room to work. The cap is applied per block, and
// truncation is announced inside the frame so the model can see that it is
// looking at a fragment.
const DefaultMaxBytes = 16 << 10

// Kind labels where a block of untrusted content came from. It is part of the
// frame header so a failure analyst reading a prompt can tell a page's visible
// text from a header the site controls just as fully.
type Kind string

// The channels page-derived bytes arrive on. Each is a place where a page
// controls the content and something in the pipeline reads it.
const (
	KindDOMText       Kind = "dom_text"
	KindDOMHTML       Kind = "dom_html"
	KindAccessibility Kind = "accessibility_tree"
	KindConsole       Kind = "console_log"
	KindNetwork       Kind = "network_log"
	KindHTTPBody      Kind = "http_response_body"
	KindDownload      Kind = "downloaded_file"
	KindFilename      Kind = "filename"
	KindPageTitle     Kind = "page_title"
	KindURL           Kind = "url"
	// KindAgentOutput is a model's own answer being shown back to it — a
	// validator's report on a rejected out.json, most of the time. It belongs
	// here because a model that was hijacked on its first attempt would
	// otherwise get its injected instructions handed back as trusted
	// feedback, which is the same hole through a longer path.
	KindAgentOutput Kind = "agent_output"
)

// Block is one framed piece of untrusted content.
type Block struct {
	// Nonce ties the opening marker to the closing one. It must be unique per
	// run and unpredictable to the page; see [NewNonce].
	Nonce string
	// Kind says which channel the bytes arrived on.
	Kind Kind
	// Source is the origin of the bytes — a URL, a file name, a console
	// channel. It is itself untrusted and is emitted JSON-quoted.
	Source string
	// Content is the raw payload. Wrap sanitises it; callers pass it through
	// unmodified.
	Content string
	// MaxBytes overrides DefaultMaxBytes when non-zero.
	MaxBytes int
}

// Wrap renders a block into the exact text that may be placed in a prompt.
//
// The output is stable for a given (nonce, kind, source, content): the same
// bytes in produce the same bytes out, which is what lets the injection corpus
// assert that a prompt's instruction region never varies with page content.
//
// Sanitisation, in order:
//
//  1. Frame markers are removed, case-insensitively, so a page that guesses
//     the delimiter cannot close the block early and continue as the operator.
//  2. The nonce is removed, in case it leaked into the page some other way.
//  3. ANSI escape sequences and C0/C1 control characters go — a CLI that
//     renders a prompt to a terminal must not be steerable by a web page.
//  4. Invisible Unicode (bidi overrides, tag characters, zero-width joiners)
//     goes: it is the standard way to hide an instruction from a human
//     reviewer while leaving it legible to a tokenizer.
//  5. The result is truncated at a rune boundary to MaxBytes.
func Wrap(b Block) string {
	limit := b.MaxBytes
	if limit <= 0 {
		limit = DefaultMaxBytes
	}

	safe := sanitizeUntrusted(b.Content, b.Nonce)
	rawLen := len(safe)
	truncated := false
	if len(safe) > limit {
		safe = truncateRunes(safe, limit)
		truncated = true
	}

	kind := b.Kind
	if kind == "" {
		kind = "unknown"
	}

	var sb strings.Builder
	sb.WriteString(MarkerStart)
	fmt.Fprintf(&sb, " id=%s kind=%s source=%s bytes=%d truncated=%t",
		quote(b.Nonce), quote(string(kind)), quote(b.Source), rawLen, truncated)
	sb.WriteString(MarkerClose)
	sb.WriteByte('\n')
	sb.WriteString(safe)
	if !strings.HasSuffix(safe, "\n") {
		sb.WriteByte('\n')
	}
	sb.WriteString(MarkerEnd)
	fmt.Fprintf(&sb, " id=%s", quote(b.Nonce))
	sb.WriteString(MarkerClose)
	return sb.String()
}

// quote renders s as a JSON string literal. Source and kind are attacker
// controlled, so they may not be interpolated raw into the header line.
func quote(s string) string {
	var sb strings.Builder
	sb.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			sb.WriteString(`\"`)
		case '\\':
			sb.WriteString(`\\`)
		case '\n':
			sb.WriteString(`\n`)
		case '\r':
			sb.WriteString(`\r`)
		case '\t':
			sb.WriteString(`\t`)
		case '>':
			// Escaped so a crafted source or kind cannot emit `>>>` and make
			// the header line's terminator ambiguous to a reader scanning for
			// it. JSON-compatible, so the attribute still parses.
			sb.WriteString(`\u003e`)
		default:
			if r < 0x20 || r == 0x7f || isInvisible(r) || r == utf8.RuneError {
				fmt.Fprintf(&sb, `\u%04x`, 0xfffd)
				continue
			}
			sb.WriteRune(r)
		}
	}
	sb.WriteByte('"')
	return sb.String()
}

func sanitizeUntrusted(content, nonce string) string {
	s := stripMarkers(content)
	if nonce != "" {
		s = strings.ReplaceAll(s, nonce, "")
	}
	s = stripANSI(s)
	s = stripControlAndInvisible(s)
	return s
}

// stripMarkers removes both frame markers case-insensitively, and any
// `>>>`-terminated tail that followed one, so `<<<uNtRuStEd_PaGe_CoNtEnT id=x>>>`
// cannot survive as something a downstream reader mistakes for a real frame.
func stripMarkers(s string) string {
	for _, marker := range []string{MarkerEnd, MarkerStart} {
		s = removeFold(s, marker)
	}
	return s
}

func removeFold(s, marker string) string {
	lowerMarker := strings.ToLower(marker)
	var sb strings.Builder
	for {
		i := strings.Index(strings.ToLower(s), lowerMarker)
		if i < 0 {
			sb.WriteString(s)
			return sb.String()
		}
		sb.WriteString(s[:i])
		rest := s[i+len(marker):]
		// Swallow the marker's attribute tail up to and including the closing
		// `>>>`, but only when it is close by: an unterminated `<<<UNTRUSTED`
		// must not eat the rest of the page.
		if j := strings.Index(rest, MarkerClose); j >= 0 && j <= 256 {
			rest = rest[j+len(MarkerClose):]
		}
		s = rest
	}
}

// stripANSI removes CSI/OSC escape sequences. A prompt file is read by a CLI
// that may echo it; a page must not be able to move the cursor, clear the
// screen, or set a terminal title in the operator's shell.
func stripANSI(s string) string {
	var sb strings.Builder
	sb.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] != 0x1b {
			sb.WriteByte(s[i])
			i++
			continue
		}
		i++
		if i >= len(s) {
			break
		}
		switch s[i] {
		case '[': // CSI: params until a byte in @-~
			i++
			for i < len(s) && (s[i] < 0x40 || s[i] > 0x7e) {
				i++
			}
			if i < len(s) {
				i++
			}
		case ']': // OSC: until BEL or ST
			i++
			for i < len(s) && s[i] != 0x07 {
				if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
					i++
					break
				}
				i++
			}
			if i < len(s) {
				i++
			}
		default:
			i++
		}
	}
	return sb.String()
}

func stripControlAndInvisible(s string) string {
	var sb strings.Builder
	sb.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			sb.WriteRune(r)
		case r == '\r':
			// Normalise: a lone CR can hide a line from a terminal reader.
			sb.WriteRune('\n')
		case r < 0x20 || (r >= 0x7f && r <= 0x9f):
			// Dropped: C0 and C1 controls carry no meaning in page text.
		case isInvisible(r):
			// Dropped: see the doc comment on Wrap.
		case r == utf8.RuneError:
			sb.WriteRune('�')
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// isInvisible reports whether r renders as nothing to a human but is still a
// token to a model. These are the smuggling channels: bidi overrides reorder
// visible text, tag characters are a full invisible ASCII alphabet, and
// variation selectors have been used to encode arbitrary bytes.
func isInvisible(r rune) bool {
	switch {
	case r == 0x00ad: // soft hyphen
		return true
	case r >= 0x200b && r <= 0x200f: // zero-width + LRM/RLM
		return true
	case r >= 0x202a && r <= 0x202e: // bidi embedding/override
		return true
	case r >= 0x2060 && r <= 0x2064: // word joiner, invisible operators
		return true
	case r >= 0x2066 && r <= 0x2069: // bidi isolates
		return true
	case r == 0xfeff: // BOM / zero-width no-break space
		return true
	case r >= 0xfe00 && r <= 0xfe0f: // variation selectors
		return true
	case r >= 0xe0000 && r <= 0xe007f: // tag characters
		return true
	case r >= 0xe0100 && r <= 0xe01ef: // variation selectors supplement
		return true
	case unicode.Is(unicode.Cf, r):
		return true
	}
	return false
}

func truncateRunes(s string, limit int) string {
	const note = "\n[truncated by qa-daemon]"
	budget := limit - len(note)
	if budget <= 0 {
		return note
	}
	if len(s) <= budget {
		return s
	}
	cut := budget
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + note
}
