package wyoming

import (
	"strings"
	"testing"
	"unicode"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testMaxChars = 40

// accented is 18 runes but 36 bytes: two sentences of it joined are 39 runes
// and 75 bytes, so they merge only when counted in runes.
var accented = strings.Repeat("\u00e9", 18)

// Sentences of 19 and 20 runes, twice as many bytes, for the merge bound:
// joined, 19+1+20 runes are exactly testMaxChars and 20+1+20 one over.
var (
	accented19 = strings.Repeat("\u00e9", 18) + "."
	accented20 = strings.Repeat("\u00e9", 19) + "."
)

func TestSplitter(t *testing.T) {
	tests := []struct {
		name string
		adds []string
		want [][]string
		rest string
	}{
		{
			name: "complete sentences in one call merge",
			adds: []string{"One. Two. Three. Fo"},
			want: [][]string{{"One. Two. Three."}},
			rest: "Fo",
		},
		{
			name: "each call releases the sentence it completes",
			adds: []string{"One. T", "wo. T", "hree. F"},
			want: [][]string{{"One."}, {"Two."}, {"Three."}},
			rest: "F",
		},
		{
			name: "a complete sentence alone is not held",
			adds: []string{"One. T"},
			want: [][]string{{"One."}},
			rest: "T",
		},
		{
			name: "a sentence that does not fit starts the next segment",
			adds: []string{"First one here. Second one. Third. Fourth sentence is long. X"},
			want: [][]string{{"First one here. Second one. Third.", "Fourth sentence is long."}},
			rest: "X",
		},
		{
			name: "a sentence longer than the limit is released whole",
			adds: []string{"This sentence is much longer than forty characters in total. Next"},
			want: [][]string{{"This sentence is much longer than forty characters in total."}},
			rest: "Next",
		},
		{
			name: "a boundary at the end of the text waits",
			adds: []string{"Hello there.", " How"},
			want: [][]string{nil, {"Hello there."}},
			rest: "How",
		},
		{
			name: "Home Assistant's chunks",
			adds: []string{"Hello the", "re. How are", " you?"},
			want: [][]string{nil, {"Hello there."}, nil},
			rest: "How are you?",
		},
		{
			name: "runs of terminators and the ellipsis end a sentence",
			adds: []string{"Really?! Yes... Done… ok"},
			want: [][]string{{"Really?! Yes... Done…"}},
			rest: "ok",
		},
		{
			name: "a newline ends a sentence",
			adds: []string{"Line one\nLine two"},
			want: [][]string{{"Line one"}},
			rest: "Line two",
		},
		{
			name: "a newline at the end is enough",
			adds: []string{"Line one\n"},
			want: [][]string{{"Line one"}},
			rest: "",
		},
		{
			name: "a full stop inside a word is no boundary",
			adds: []string{"Pi is 3.14 or so"},
			want: [][]string{nil},
			rest: "Pi is 3.14 or so",
		},
		{
			name: "merging happens only within one call",
			adds: []string{"One. T", "wo. Three. F"},
			want: [][]string{{"One."}, {"Two. Three."}},
			rest: "F",
		},
		{
			name: "whitespace between segments is dropped",
			adds: []string{"One. ", "  Two.", "\n"},
			want: [][]string{{"One."}, nil, {"Two."}},
			rest: "",
		},
		{
			name: "a merged segment counts runes",
			adds: []string{accented + ". " + accented + ". x"},
			want: [][]string{{accented + ". " + accented + "."}},
			rest: "x",
		},
		{
			name: "a merged segment of exactly maxChars runes is one segment",
			adds: []string{accented19 + " " + accented20 + " x"},
			want: [][]string{{accented19 + " " + accented20}},
			rest: "x",
		},
		{
			name: "a merged segment one rune over maxChars is two segments",
			adds: []string{accented20 + " " + accented20 + " x"},
			want: [][]string{{accented20, accented20}},
			rest: "x",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newSplitter(testMaxChars)

			got := make([][]string, 0, len(tc.adds))
			for _, text := range tc.adds {
				got = append(got, s.Add(text))
			}

			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.rest, s.Rest())
		})
	}
}

// FuzzSplitter feeds text to the splitter in arbitrary pieces. Every released
// segment and the rest, in order, must be the input with only the whitespace
// between them removed, and the rest must hold no complete sentence.
func FuzzSplitter(f *testing.F) {
	seeds := []struct {
		text string
		cuts []byte
	}{
		{"Hello there. How are you?", []byte{8, 10}},
		{"One. Two. Three. Fo", nil},
		{"One. Two. Three. Four.\n\nFive!? Six… seven", []byte{1, 2, 3}},
		{"　Wide　space.　Next", []byte{1, 1, 1}},
		{"a.… b\n", []byte{2}},
	}
	for _, seed := range seeds {
		f.Add(seed.text, seed.cuts, uint8(testMaxChars))
	}

	f.Fuzz(func(t *testing.T, text string, cuts []byte, limit uint8) {
		s := newSplitter(int(limit%64) + 1)

		var pieces []string
		for _, chunk := range cutText(text, cuts) {
			for _, segment := range s.Add(chunk) {
				require.NotEmpty(t, segment)
				require.Equal(t, strings.TrimFunc(segment, unicode.IsSpace), segment, "a segment keeps outer whitespace")
				pieces = append(pieces, segment)
			}
		}
		pieces = append(pieces, s.Rest())

		remaining := text
		for _, piece := range pieces {
			remaining = strings.TrimLeftFunc(remaining, unicode.IsSpace)
			require.True(t, strings.HasPrefix(remaining, piece), "piece %q is not next in %q", piece, remaining)
			remaining = remaining[len(piece):]
		}
		require.Empty(t, strings.TrimLeftFunc(remaining, unicode.IsSpace), "text was dropped")

		require.Empty(t, newSplitter(int(limit%64)+1).Add(s.Rest()), "the rest holds a complete sentence")
	})
}

// cutText splits text into pieces whose byte lengths come from cuts, which
// may split a rune.
func cutText(text string, cuts []byte) []string {
	var chunks []string
	for _, cut := range cuts {
		n := min(int(cut%8)+1, len(text))
		chunks = append(chunks, text[:n])
		text = text[n:]
	}

	return append(chunks, text)
}
