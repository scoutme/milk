package tags

import (
	"strings"
	"testing"
)

// --- PerceptTagPair ---

func TestPerceptTagPair_WithNonce(t *testing.T) {
	open, close_ := PerceptTagPair("abc123")
	if open != "<milk:percept:abc123>" {
		t.Errorf("open: want %q, got %q", "<milk:percept:abc123>", open)
	}
	if close_ != "</milk:percept:abc123>" {
		t.Errorf("close: want %q, got %q", "</milk:percept:abc123>", close_)
	}
}

func TestPerceptTagPair_EmptyNonce(t *testing.T) {
	open, close_ := PerceptTagPair("")
	if open != "<milk:percept>" {
		t.Errorf("open: want %q, got %q", "<milk:percept>", open)
	}
	if close_ != "</milk:percept>" {
		t.Errorf("close: want %q, got %q", "</milk:percept>", close_)
	}
}

// --- ConsumerHintFrom ---

func TestConsumerHintFrom_FirstHint(t *testing.T) {
	body, hint := ConsumerHintFrom("@primary: some fact", "primary", "escalation")
	if body != "some fact" || hint != "primary" {
		t.Errorf("want (some fact, primary), got (%q, %q)", body, hint)
	}
}

func TestConsumerHintFrom_SecondHint(t *testing.T) {
	body, hint := ConsumerHintFrom("@escalation: other fact", "primary", "escalation")
	if body != "other fact" || hint != "escalation" {
		t.Errorf("want (other fact, escalation), got (%q, %q)", body, hint)
	}
}

func TestConsumerHintFrom_NoHint(t *testing.T) {
	body, hint := ConsumerHintFrom("plain fact", "primary", "escalation")
	if body != "plain fact" || hint != "" {
		t.Errorf("want (plain fact, ), got (%q, %q)", body, hint)
	}
}

func TestConsumerHintFrom_NoSpaceAfterColon_NoMatch(t *testing.T) {
	body, hint := ConsumerHintFrom("@primary:no space", "primary", "escalation")
	if body != "@primary:no space" || hint != "" {
		t.Errorf("want original string and empty hint, got (%q, %q)", body, hint)
	}
}

func TestConsumerHintFrom_EmptyNameSkipped(t *testing.T) {
	// Empty name must not produce a spurious match for "@: " prefix.
	_, hint := ConsumerHintFrom("@: fact", "", "escalation")
	if hint != "" {
		t.Errorf("empty name should not match, got hint %q", hint)
	}
}

// --- NextCodeSpanEnd ---

func TestNextCodeSpanEnd_SingleBacktick(t *testing.T) {
	s := "`hello`"
	end := NextCodeSpanEnd(s, 0)
	if end != len(s) {
		t.Errorf("want %d, got %d", len(s), end)
	}
}

func TestNextCodeSpanEnd_TripleBacktick(t *testing.T) {
	s := "```code```"
	end := NextCodeSpanEnd(s, 0)
	if end != len(s) {
		t.Errorf("want %d, got %d", len(s), end)
	}
}

func TestNextCodeSpanEnd_NoClose(t *testing.T) {
	end := NextCodeSpanEnd("`unclosed", 0)
	if end != -1 {
		t.Errorf("want -1 for unclosed span, got %d", end)
	}
}

func TestNextCodeSpanEnd_LongerRunBehavior(t *testing.T) {
	// NextCodeSpanEnd uses a simplified rule: a closing run must not be FOLLOWED
	// by a backtick, but it does not check whether it is PRECEDED by one.
	// Full CommonMark would close `hello``world` at position 14; this
	// implementation closes it at position 8 (the second backtick of ``).
	// This is acceptable for milk's purpose of protecting tags inside code spans.
	s := "`hello``world`"
	end := NextCodeSpanEnd(s, 0)
	if end != 8 {
		t.Errorf("want 8 (simplified close on second backtick of double run), got %d", end)
	}
}

func TestNextCodeSpanEnd_MidString(t *testing.T) {
	s := "abc`inner`def"
	end := NextCodeSpanEnd(s, 3)
	if end != 10 {
		t.Errorf("want 10, got %d", end)
	}
}

// --- StripTagsByPrefix ---

func TestStripTagsByPrefix_Basic(t *testing.T) {
	const openPrefix = "<milk:need:"
	input := "hello " + openPrefix + "n1>goal</milk:need:n1> world"
	got := StripTagsByPrefix(input, openPrefix)
	if got != "hello  world" {
		t.Errorf("want %q, got %q", "hello  world", got)
	}
}

func TestStripTagsByPrefix_NoTags(t *testing.T) {
	got := StripTagsByPrefix("no tags here", "<milk:need:")
	if got != "no tags here" {
		t.Errorf("want unchanged, got %q", got)
	}
}

func TestStripTagsByPrefix_CodeSpanPreserved(t *testing.T) {
	const openPrefix = "<milk:need:"
	input := "`" + openPrefix + "n1>goal</milk:need:n1>`"
	got := StripTagsByPrefix(input, openPrefix)
	if got != input {
		t.Errorf("tag inside code span should pass through, want %q, got %q", input, got)
	}
}

func TestStripTagsByPrefix_UnclosedTag(t *testing.T) {
	const openPrefix = "<milk:need:"
	input := "before " + openPrefix + "n1>dangling"
	got := StripTagsByPrefix(input, openPrefix)
	if strings.Contains(got, "milk:need") {
		t.Errorf("open tag should be stripped from output, got %q", got)
	}
	if strings.Contains(got, "dangling") {
		t.Errorf("unclosed tag body should be dropped, got %q", got)
	}
}

func TestStripTagsByPrefix_MismatchedCloseTagFallback(t *testing.T) {
	// tags.StripTagsByPrefix has a close-any fallback: when the exact nonce close
	// tag is absent, it matches any </PREFIX: close tag. This preserves text after
	// the mismatched close and is unique to this package (claude.stripTagsByPrefix
	// truncates instead).
	const openPrefix = "<milk:need:"
	input := "before " + openPrefix + "n1>goal</milk:need:other> after"
	got := StripTagsByPrefix(input, openPrefix)
	if strings.Contains(got, "goal") {
		t.Errorf("tag body should be stripped by fallback close, got %q", got)
	}
	if strings.Contains(got, "milk:need") {
		t.Errorf("tag markup should be stripped, got %q", got)
	}
	if !strings.Contains(got, "before") || !strings.Contains(got, "after") {
		t.Errorf("surrounding text should be preserved, got %q", got)
	}
}

func TestStripTagsByPrefix_MultipleTagsStripped(t *testing.T) {
	const openPrefix = "<milk:need:"
	input := "a " + openPrefix + "n1>g1</milk:need:n1> b " + openPrefix + "n2>g2</milk:need:n2> c"
	got := StripTagsByPrefix(input, openPrefix)
	if strings.Contains(got, "g1") || strings.Contains(got, "g2") {
		t.Errorf("both tag bodies should be stripped, got %q", got)
	}
	if !strings.Contains(got, "a") || !strings.Contains(got, "b") || !strings.Contains(got, "c") {
		t.Errorf("surrounding text should be preserved, got %q", got)
	}
}

// --- StripPerceptTags ---

func TestStripPerceptTags_Basic(t *testing.T) {
	const nonce = "xtest1"
	open, close_ := PerceptTagPair(nonce)
	got := StripPerceptTags("hello " + open + "fact" + close_ + " world")
	if got != "hello  world" {
		t.Errorf("want %q, got %q", "hello  world", got)
	}
}

func TestStripPerceptTags_StaleNonceAlsoStripped(t *testing.T) {
	// StripPerceptTags strips ALL <milk:percept:*> tags regardless of nonce.
	input := "a <milk:percept:bx7201>stale fact</milk:percept:bx7201> b"
	got := StripPerceptTags(input)
	if strings.Contains(got, "stale fact") || strings.Contains(got, "milk:percept") {
		t.Errorf("stale nonce tag should be stripped, got %q", got)
	}
}

func TestStripPerceptTags_CodeSpanPreserved(t *testing.T) {
	open, close_ := PerceptTagPair("xtest2")
	input := "`" + open + "fact" + close_ + "`"
	got := StripPerceptTags(input)
	if got != input {
		t.Errorf("tag inside code span should pass through, got %q", got)
	}
}

func TestStripPerceptTags_NoTags(t *testing.T) {
	got := StripPerceptTags("no percept tags here")
	if got != "no percept tags here" {
		t.Errorf("want unchanged, got %q", got)
	}
}

// --- TagWriter helpers ---

func needTagPair(nonce string) (open, close_ string) {
	closePrefix := "</" + NeedOpenPrefix[1:]
	return NeedOpenPrefix + nonce + ">", closePrefix + nonce + ">"
}

func newTestTagWriter(out *strings.Builder, nonce string, captured *[]string) *TagWriter {
	return &TagWriter{
		W:           out,
		OpenPrefix:  NeedOpenPrefix,
		OnTag:       func(s string) { *captured = append(*captured, s) },
		RecordNonce: nonce,
	}
}

// --- TagWriter ---

func TestTagWriter_SingleWrite(t *testing.T) {
	const nonce = "tw001"
	var out strings.Builder
	var needs []string
	tw := newTestTagWriter(&out, nonce, &needs)

	open, close_ := needTagPair(nonce)
	tw.Write([]byte("before " + open + "implement auth" + close_ + " after")) //nolint:errcheck

	if out.String() != "before  after" {
		t.Errorf("output: want %q, got %q", "before  after", out.String())
	}
	if len(needs) != 1 || needs[0] != "implement auth" {
		t.Errorf("tags: want [implement auth], got %v", needs)
	}
}

func TestTagWriter_WrongNonceStrippedNotRecorded(t *testing.T) {
	const nonce = "tw002"
	var out strings.Builder
	var needs []string
	tw := newTestTagWriter(&out, nonce, &needs)

	open, close_ := needTagPair("othernonce")
	tw.Write([]byte("text " + open + "goal" + close_ + " end")) //nolint:errcheck
	tw.Flush()                                                  //nolint:errcheck

	if len(needs) != 0 {
		t.Errorf("wrong-nonce tag must not be recorded, got %v", needs)
	}
	if strings.Contains(out.String(), "milk:need") {
		t.Errorf("wrong-nonce tag should be stripped from output, got %q", out.String())
	}
	if !strings.Contains(out.String(), "text") || !strings.Contains(out.String(), "end") {
		t.Errorf("surrounding text must be preserved, got %q", out.String())
	}
}

func TestTagWriter_SplitAcrossWrites(t *testing.T) {
	const nonce = "tw003"
	var out strings.Builder
	var needs []string
	tw := newTestTagWriter(&out, nonce, &needs)

	open, close_ := needTagPair(nonce)
	mid := len(open) / 2
	tw.Write([]byte("hello " + open[:mid]))                              //nolint:errcheck
	tw.Write([]byte(open[mid:] + "split goal" + close_[:len(close_)/2])) //nolint:errcheck
	tw.Write([]byte(close_[len(close_)/2:] + " end"))                    //nolint:errcheck

	if out.String() != "hello  end" {
		t.Errorf("output: want %q, got %q", "hello  end", out.String())
	}
	if len(needs) != 1 || needs[0] != "split goal" {
		t.Errorf("tags: want [split goal], got %v", needs)
	}
}

func TestTagWriter_CodeSpanPreserved(t *testing.T) {
	const nonce = "tw004"
	var out strings.Builder
	var needs []string
	tw := newTestTagWriter(&out, nonce, &needs)

	open, close_ := needTagPair(nonce)
	input := "before `" + open + "goal" + close_ + "` after"
	tw.Write([]byte(input)) //nolint:errcheck
	tw.Flush()              //nolint:errcheck

	if len(needs) != 0 {
		t.Errorf("tag inside code span should not be captured, got %v", needs)
	}
	if !strings.Contains(out.String(), open) {
		t.Errorf("tag inside code span should pass through, got %q", out.String())
	}
}

func TestTagWriter_FlushTrailingBytes(t *testing.T) {
	const nonce = "tw005"
	var out strings.Builder
	var needs []string
	tw := newTestTagWriter(&out, nonce, &needs)

	open, _ := needTagPair(nonce)
	partial := open[:len(open)/2]
	before := "trailing text "
	tw.Write([]byte(before + partial)) //nolint:errcheck
	if out.String() != before {
		t.Errorf("before flush: want %q, got %q", before, out.String())
	}
	tw.Flush() //nolint:errcheck
	if out.String() != before+partial {
		t.Errorf("after flush: want %q, got %q", before+partial, out.String())
	}
}

func TestTagWriter_FlushUnclosedTag(t *testing.T) {
	const nonce = "tw006"
	var out strings.Builder
	var needs []string
	tw := newTestTagWriter(&out, nonce, &needs)

	open, _ := needTagPair(nonce)
	tw.Write([]byte("prefix " + open + "unclosed content")) //nolint:errcheck
	tw.Flush()                                              //nolint:errcheck
	if out.String() != "prefix " {
		t.Errorf("want %q, got %q", "prefix ", out.String())
	}
}

// --- PerceptWriter helpers ---

func newTestPerceptWriter(out *strings.Builder, nonce string, percepts *[]string) *PerceptWriter {
	return &PerceptWriter{
		W:           out,
		OnPercept:   func(s, _ string) { *percepts = append(*percepts, s) },
		RecordNonce: nonce,
	}
}

// --- PerceptWriter ---

func TestPerceptWriter_SingleWrite(t *testing.T) {
	const nonce = "pw001"
	var out strings.Builder
	var percepts []string
	pw := newTestPerceptWriter(&out, nonce, &percepts)

	open, close_ := PerceptTagPair(nonce)
	pw.Write([]byte("before " + open + "my fact" + close_ + " after")) //nolint:errcheck

	if out.String() != "before  after" {
		t.Errorf("output: want %q, got %q", "before  after", out.String())
	}
	if len(percepts) != 1 || percepts[0] != "my fact" {
		t.Errorf("percepts: want [my fact], got %v", percepts)
	}
}

func TestPerceptWriter_LegacyTagPassesThrough(t *testing.T) {
	// <milk:percept> (no nonce) has no colon after "percept" — PerceptOpenPrefix
	// is "<milk:percept:" so the legacy form never matches and must pass through.
	const nonce = "pw002"
	var out strings.Builder
	var percepts []string
	pw := newTestPerceptWriter(&out, nonce, &percepts)

	pw.Write([]byte("before <milk:percept>legacy</milk:percept> after")) //nolint:errcheck
	pw.Flush()                                                           //nolint:errcheck

	if len(percepts) != 0 {
		t.Errorf("legacy tag should not be captured, got %v", percepts)
	}
	if !strings.Contains(out.String(), "<milk:percept>legacy</milk:percept>") {
		t.Errorf("legacy tag should pass through unchanged, got %q", out.String())
	}
}

func TestPerceptWriter_StaleNonceStrippedNotRecorded(t *testing.T) {
	const currentNonce = "pw003"
	var out strings.Builder
	var percepts []string
	pw := newTestPerceptWriter(&out, currentNonce, &percepts)

	input := "before <milk:percept:bx7201>stale fact</milk:percept:bx7201> after"
	pw.Write([]byte(input)) //nolint:errcheck
	pw.Flush()              //nolint:errcheck

	if len(percepts) != 0 {
		t.Errorf("stale-nonce tag must not be recorded, got %v", percepts)
	}
	if strings.Contains(out.String(), "milk:percept") {
		t.Errorf("stale-nonce tag must be stripped from output, got %q", out.String())
	}
	if !strings.Contains(out.String(), "before") || !strings.Contains(out.String(), "after") {
		t.Errorf("surrounding text must be preserved, got %q", out.String())
	}
}

func TestPerceptWriter_CodeSpanPreserved(t *testing.T) {
	const nonce = "pw004"
	var out strings.Builder
	var percepts []string
	pw := newTestPerceptWriter(&out, nonce, &percepts)

	open, close_ := PerceptTagPair(nonce)
	input := "before `" + open + "fact" + close_ + "` after"
	pw.Write([]byte(input)) //nolint:errcheck
	pw.Flush()              //nolint:errcheck

	if len(percepts) != 0 {
		t.Errorf("tag inside code span should not be captured, got %v", percepts)
	}
	if !strings.Contains(out.String(), open) {
		t.Errorf("tag inside code span should pass through, got %q", out.String())
	}
}

func TestPerceptWriter_SplitAcrossWrites(t *testing.T) {
	const nonce = "pw005"
	var out strings.Builder
	var percepts []string
	pw := newTestPerceptWriter(&out, nonce, &percepts)

	open, close_ := PerceptTagPair(nonce)
	mid := len(open) / 2
	pw.Write([]byte("hello " + open[:mid]))                              //nolint:errcheck
	pw.Write([]byte(open[mid:] + "split fact" + close_[:len(close_)/2])) //nolint:errcheck
	pw.Write([]byte(close_[len(close_)/2:] + " end"))                    //nolint:errcheck

	if out.String() != "hello  end" {
		t.Errorf("output: want %q, got %q", "hello  end", out.String())
	}
	if len(percepts) != 1 || percepts[0] != "split fact" {
		t.Errorf("percepts: want [split fact], got %v", percepts)
	}
}

func TestPerceptWriter_FlushTrailingBytes(t *testing.T) {
	const nonce = "pw006"
	var out strings.Builder
	var percepts []string
	pw := newTestPerceptWriter(&out, nonce, &percepts)

	open, _ := PerceptTagPair(nonce)
	partial := open[:len(open)/2]
	before := "trailing text "
	pw.Write([]byte(before + partial)) //nolint:errcheck
	if out.String() != before {
		t.Errorf("before flush: want %q, got %q", before, out.String())
	}
	pw.Flush() //nolint:errcheck
	if out.String() != before+partial {
		t.Errorf("after flush: want %q, got %q", before+partial, out.String())
	}
}

func TestPerceptWriter_FlushUnclosedTag(t *testing.T) {
	const nonce = "pw007"
	var out strings.Builder
	var percepts []string
	pw := newTestPerceptWriter(&out, nonce, &percepts)

	open, _ := PerceptTagPair(nonce)
	pw.Write([]byte("prefix " + open + "unclosed content")) //nolint:errcheck
	pw.Flush()                                              //nolint:errcheck
	if out.String() != "prefix " {
		t.Errorf("want %q, got %q", "prefix ", out.String())
	}
}

func TestPerceptWriter_ConsumerHint(t *testing.T) {
	const nonce = "pw008"
	type captured struct{ body, hint string }
	var got []captured
	var out strings.Builder
	pw := &PerceptWriter{
		W:           &out,
		OnPercept:   func(body, hint string) { got = append(got, captured{body, hint}) },
		RecordNonce: nonce,
		AgentNames:  []string{"primary", "escalation"},
	}

	open, close_ := PerceptTagPair(nonce)
	pw.Write([]byte(open + "@primary: route simple tasks" + close_)) //nolint:errcheck
	pw.Flush()                                                       //nolint:errcheck

	if len(got) != 1 || got[0].body != "route simple tasks" || got[0].hint != "primary" {
		t.Errorf("want [{route simple tasks primary}], got %v", got)
	}
}

func TestPerceptWriter_MultipleTagsInOneWrite(t *testing.T) {
	const nonce = "pw009"
	var out strings.Builder
	var percepts []string
	pw := newTestPerceptWriter(&out, nonce, &percepts)

	open, close_ := PerceptTagPair(nonce)
	input := "a " + open + "fact one" + close_ + " b " + open + "fact two" + close_ + " c"
	pw.Write([]byte(input)) //nolint:errcheck
	pw.Flush()              //nolint:errcheck

	if len(percepts) != 2 || percepts[0] != "fact one" || percepts[1] != "fact two" {
		t.Errorf("want [fact one fact two], got %v", percepts)
	}
	if strings.Contains(out.String(), "milk:percept") {
		t.Errorf("tags must be stripped from output, got %q", out.String())
	}
}
