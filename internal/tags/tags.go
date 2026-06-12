// Package tags provides streaming tag interceptors and strip helpers for the
// milk protocol tags (<milk:need:NONCE> and <milk:percept:NONCE>).
// Both the claude and local agent packages depend on this; keeping it here
// avoids code duplication and import cycles.
package tags

import (
	"io"
	"strings"
)

const NeedOpenPrefix = "<milk:need:"
const PerceptOpenPrefix = "<milk:percept:"
const EscalateOpenPrefix = "<milk:escalate:"

// PerceptTagPair returns the open and close tag strings for the given nonce.
// e.g. nonce "ab1c2d" → "<milk:percept:ab1c2d>", "</milk:percept:ab1c2d>"
func PerceptTagPair(nonce string) (open, close_ string) {
	if nonce == "" {
		return "<milk:percept>", "</milk:percept>"
	}
	return "<milk:percept:" + nonce + ">", "</milk:percept:" + nonce + ">"
}

// ConsumerHintFrom strips an optional "@<name>: " prefix from s where name is
// one of the provided agent names, and returns the remaining body and the matched
// name (or "" when no prefix matches).
func ConsumerHintFrom(s string, names ...string) (body, hint string) {
	for _, h := range names {
		if h == "" {
			continue
		}
		prefix := "@" + h + ": "
		if strings.HasPrefix(s, prefix) {
			return strings.TrimPrefix(s, prefix), h
		}
	}
	return s, ""
}

// NextCodeSpanEnd returns the index in s just past the closing backtick sequence
// that matches the opening sequence starting at s[start]. start must point at a
// backtick. Returns -1 if no matching close is found.
func NextCodeSpanEnd(s string, start int) int {
	n := 0
	for start+n < len(s) && s[start+n] == '`' {
		n++
	}
	close_ := strings.Repeat("`", n)
	pos := start + n
	for {
		idx := strings.Index(s[pos:], close_)
		if idx < 0 {
			return -1
		}
		idx += pos
		end := idx + n
		if end < len(s) && s[end] == '`' {
			pos = idx + 1
			continue
		}
		return end
	}
}

// StripTagsByPrefix removes all tags matching <PREFIX:*>…</PREFIX:*> from s,
// skipping over markdown code spans (backtick-delimited regions).
func StripTagsByPrefix(s, openPrefix string) string {
	closePrefix := "</" + openPrefix[1:]
	var result strings.Builder
	pos := 0
	for pos < len(s) {
		if s[pos] == '`' {
			end := NextCodeSpanEnd(s, pos)
			if end < 0 {
				result.WriteString(s[pos:])
				return strings.TrimSpace(result.String())
			}
			result.WriteString(s[pos:end])
			pos = end
			continue
		}
		rel := strings.Index(s[pos:], openPrefix)
		if rel < 0 {
			result.WriteString(s[pos:])
			break
		}
		open := pos + rel
		if bt := strings.IndexByte(s[pos:open], '`'); bt >= 0 {
			end := NextCodeSpanEnd(s, pos+bt)
			if end < 0 {
				result.WriteString(s[pos:])
				return strings.TrimSpace(result.String())
			}
			result.WriteString(s[pos:end])
			pos = end
			continue
		}
		result.WriteString(s[pos:open])
		openEnd := strings.Index(s[open:], ">")
		if openEnd < 0 {
			return strings.TrimSpace(result.String())
		}
		openEnd += open + 1
		noncePart := s[open+len(openPrefix) : openEnd-1]
		closeTag := closePrefix + noncePart + ">"
		closeIdx := strings.Index(s[openEnd:], closeTag)
		if closeIdx < 0 {
			closeAny := strings.Index(s[openEnd:], closePrefix)
			if closeAny < 0 {
				return strings.TrimSpace(result.String())
			}
			closeAny += openEnd
			closeEnd := strings.Index(s[closeAny:], ">")
			if closeEnd < 0 {
				return strings.TrimSpace(result.String())
			}
			pos = closeAny + closeEnd + 1
		} else {
			pos = openEnd + closeIdx + len(closeTag)
		}
	}
	return strings.TrimSpace(result.String())
}

// StripPerceptTags removes all <milk:percept:*>…</milk:percept:*> occurrences from s,
// regardless of nonce. Markdown code spans (backtick-delimited) are skipped.
func StripPerceptTags(s string) string {
	return StripTagsByPrefix(s, PerceptOpenPrefix)
}

// TagWriter is a generic single-prefix tag interceptor for streaming output.
// It strips all matching tags from display output and calls OnTag with the body
// of tags whose nonce matches RecordNonce. Backtick code spans are passed through.
type TagWriter struct {
	W           io.Writer
	OpenPrefix  string
	OnTag       func(body string)
	RecordNonce string
	closeTag    string
	buf         strings.Builder
	inTag       bool
	codeOpen    int
	codeBtCount int
}

func (tw *TagWriter) Write(p []byte) (int, error) {
	n := len(p)
	closePrefix := "</" + tw.OpenPrefix[1:]
	for _, b := range p {
		if tw.codeOpen > 0 {
			if b == '`' {
				tw.codeBtCount++
				if tw.codeBtCount == tw.codeOpen {
					tw.codeOpen = 0
					tw.codeBtCount = 0
				}
			} else {
				tw.codeBtCount = 0
			}
			if _, err := tw.W.Write([]byte{b}); err != nil {
				return n, err
			}
			continue
		}
		if !tw.inTag && b == '`' {
			if tw.buf.Len() > 0 {
				s := tw.buf.String()
				tw.buf.Reset()
				if _, err := io.WriteString(tw.W, s); err != nil {
					return n, err
				}
			}
			tw.codeBtCount++
			if _, err := tw.W.Write([]byte{b}); err != nil {
				return n, err
			}
			continue
		}
		if !tw.inTag && tw.codeBtCount > 0 {
			tw.codeOpen = tw.codeBtCount
			tw.codeBtCount = 0
			if b == '`' {
				tw.codeOpen++
				if _, err := tw.W.Write([]byte{b}); err != nil {
					return n, err
				}
				continue
			}
			tw.codeBtCount = 0
			if _, err := tw.W.Write([]byte{b}); err != nil {
				return n, err
			}
			continue
		}
		if tw.inTag {
			tw.buf.WriteByte(b)
			s := tw.buf.String()
			if idx := strings.Index(s, tw.closeTag); idx >= 0 {
				raw := strings.TrimSpace(s[:idx])
				if tw.OnTag != nil && raw != "" && tw.closeTag == closePrefix+tw.RecordNonce+">" {
					tw.OnTag(raw)
				}
				tail := s[idx+len(tw.closeTag):]
				tw.buf.Reset()
				tw.closeTag = ""
				tw.inTag = false
				if tail != "" {
					if _, err := io.WriteString(tw.W, tail); err != nil {
						return n, err
					}
				}
			}
		} else {
			tw.buf.WriteByte(b)
			s := tw.buf.String()
			if idx := strings.Index(s, tw.OpenPrefix); idx >= 0 {
				afterPrefix := s[idx+len(tw.OpenPrefix):]
				closeAngle := strings.Index(afterPrefix, ">")
				if closeAngle < 0 {
					before := s[:idx]
					if before != "" {
						if _, err := io.WriteString(tw.W, before); err != nil {
							return n, err
						}
						tw.buf.Reset()
						tw.buf.WriteString(s[idx:])
					}
					continue
				}
				nonce := afterPrefix[:closeAngle]
				tw.closeTag = closePrefix + nonce + ">"
				before := s[:idx]
				if before != "" {
					if _, err := io.WriteString(tw.W, before); err != nil {
						return n, err
					}
				}
				tw.buf.Reset()
				tw.inTag = true
			} else if !strings.HasPrefix(tw.OpenPrefix, s) {
				if _, err := io.WriteString(tw.W, s); err != nil {
					return n, err
				}
				tw.buf.Reset()
			}
		}
	}
	return n, nil
}

func (tw *TagWriter) Flush() error {
	if tw.inTag || tw.buf.Len() == 0 {
		tw.buf.Reset()
		return nil
	}
	s := tw.buf.String()
	tw.buf.Reset()
	_, err := io.WriteString(tw.W, s)
	return err
}

// PerceptWriter wraps an io.Writer and intercepts <milk:percept:*>…</milk:percept:*>
// tags in the byte stream. Tags matching RecordNonce have their body passed to OnPercept.
// ALL percept tags (any nonce) are stripped from display output.
// AgentNames lists the known agent names used to parse @<name>: consumer-hint prefixes;
// when empty, ConsumerHintFrom returns an empty hint for all percepts.
type PerceptWriter struct {
	W           io.Writer
	OnPercept   func(content, consumerHint string)
	RecordNonce string
	AgentNames  []string
	closeTag    string
	buf         strings.Builder
	inTag       bool
	codeOpen    int
	codeBtCount int
}

func (pw *PerceptWriter) Write(p []byte) (int, error) {
	n := len(p)
	for _, b := range p {
		if pw.codeOpen > 0 {
			if b == '`' {
				pw.codeBtCount++
				if pw.codeBtCount == pw.codeOpen {
					pw.codeOpen = 0
					pw.codeBtCount = 0
				}
			} else {
				pw.codeBtCount = 0
			}
			if _, err := pw.W.Write([]byte{b}); err != nil {
				return n, err
			}
			continue
		}
		if !pw.inTag && b == '`' {
			if pw.buf.Len() > 0 {
				s := pw.buf.String()
				pw.buf.Reset()
				if _, err := io.WriteString(pw.W, s); err != nil {
					return n, err
				}
			}
			pw.codeBtCount++
			if _, err := pw.W.Write([]byte{b}); err != nil {
				return n, err
			}
			continue
		}
		if !pw.inTag && pw.codeBtCount > 0 {
			pw.codeOpen = pw.codeBtCount
			pw.codeBtCount = 0
			if _, err := pw.W.Write([]byte{b}); err != nil {
				return n, err
			}
			continue
		}
		if pw.inTag {
			pw.buf.WriteByte(b)
			s := pw.buf.String()
			if idx := strings.Index(s, pw.closeTag); idx >= 0 {
				raw := strings.TrimSpace(s[:idx])
				if pw.OnPercept != nil && raw != "" && pw.closeTag == "</milk:percept:"+pw.RecordNonce+">" {
					body, hint := ConsumerHintFrom(raw, pw.AgentNames...)
					pw.OnPercept(body, hint)
				}
				tail := s[idx+len(pw.closeTag):]
				pw.buf.Reset()
				pw.closeTag = ""
				pw.inTag = false
				if tail != "" {
					if _, err := io.WriteString(pw.W, tail); err != nil {
						return n, err
					}
				}
			}
		} else {
			pw.buf.WriteByte(b)
			s := pw.buf.String()
			if idx := strings.Index(s, PerceptOpenPrefix); idx >= 0 {
				afterPrefix := s[idx+len(PerceptOpenPrefix):]
				closeAngle := strings.Index(afterPrefix, ">")
				if closeAngle < 0 {
					before := s[:idx]
					if before != "" {
						if _, err := io.WriteString(pw.W, before); err != nil {
							return n, err
						}
						pw.buf.Reset()
						pw.buf.WriteString(s[idx:])
					}
					continue
				}
				nonce := afterPrefix[:closeAngle]
				pw.closeTag = "</milk:percept:" + nonce + ">"
				before := s[:idx]
				if before != "" {
					if _, err := io.WriteString(pw.W, before); err != nil {
						return n, err
					}
				}
				pw.buf.Reset()
				pw.inTag = true
			} else if !strings.HasPrefix(PerceptOpenPrefix, s) {
				if _, err := io.WriteString(pw.W, s); err != nil {
					return n, err
				}
				pw.buf.Reset()
			}
		}
	}
	return n, nil
}

func (pw *PerceptWriter) Flush() error {
	if pw.inTag || pw.buf.Len() == 0 {
		pw.buf.Reset()
		return nil
	}
	s := pw.buf.String()
	pw.buf.Reset()
	_, err := io.WriteString(pw.W, s)
	return err
}
