package wyoming

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// sentenceEnders end a sentence, alone or in a run, when whitespace follows.
const sentenceEnders = ".!?…"

// splitter cuts streamed text into segments to speak: complete sentences,
// merged up to maxChars runes. It holds only text that is not yet a complete
// sentence.
type splitter struct {
	maxChars int
	buffer   string
}

// span is one sentence in the buffer, without its surrounding whitespace.
type span struct {
	start, end int
}

func newSplitter(maxChars int) *splitter {
	return &splitter{maxChars: maxChars}
}

// Add appends text and returns every sentence it completes, merging those
// that fit together. A sentence is complete once the whitespace after its
// end has arrived. The whole buffer is scanned again on each call, so a rune
// split between two calls is judged once it is whole.
func (s *splitter) Add(text string) []string {
	s.buffer = strings.TrimLeftFunc(s.buffer+text, unicode.IsSpace)

	sentences := completeSentences(s.buffer)
	if len(sentences) == 0 {
		return nil
	}

	segments := s.merge(sentences)
	last := sentences[len(sentences)-1]
	s.buffer = strings.TrimLeftFunc(s.buffer[last.end:], unicode.IsSpace)

	return segments
}

// Rest returns the text that holds no complete sentence yet.
func (s *splitter) Rest() string {
	return s.buffer
}

// merge joins consecutive sentences while the joined text, with the
// whitespace between them, stays within maxChars. A sentence that does not
// fit starts the next segment, even when it is longer than maxChars alone.
func (s *splitter) merge(sentences []span) []string {
	var segments []string
	current := sentences[0]
	for _, next := range sentences[1:] {
		if utf8.RuneCountInString(s.buffer[current.start:next.end]) <= s.maxChars {
			current.end = next.end
			continue
		}
		segments = append(segments, s.buffer[current.start:current.end])
		current = next
	}

	return append(segments, s.buffer[current.start:current.end])
}

// completeSentences finds the sentences in text that have ended: at a run of
// sentenceEnders followed by whitespace, or at a newline.
func completeSentences(text string) []span {
	var sentences []span
	start := -1
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		switch {
		case r == '\n':
			if start >= 0 {
				sentences = append(sentences, trimmedSpan(text, start, i))
				start = -1
			}
			i += size
		case unicode.IsSpace(r):
			i += size
		case strings.ContainsRune(sentenceEnders, r):
			if start < 0 {
				start = i
			}
			i = endOfRun(text, i)
			if followedBySpace(text, i) {
				sentences = append(sentences, span{start: start, end: i})
				start = -1
			}
		default:
			if start < 0 {
				start = i
			}
			i += size
		}
	}

	return sentences
}

func endOfRun(text string, i int) int {
	for i < len(text) {
		r, size := utf8.DecodeRuneInString(text[i:])
		if !strings.ContainsRune(sentenceEnders, r) {
			return i
		}
		i += size
	}

	return i
}

// followedBySpace reports whether a whole whitespace rune starts at i. A rune
// cut off at the end of text decodes as an error, so it waits.
func followedBySpace(text string, i int) bool {
	if i >= len(text) {
		return false
	}
	r, _ := utf8.DecodeRuneInString(text[i:])

	return unicode.IsSpace(r)
}

func trimmedSpan(text string, start, end int) span {
	return span{start: start, end: start + len(strings.TrimRightFunc(text[start:end], unicode.IsSpace))}
}
