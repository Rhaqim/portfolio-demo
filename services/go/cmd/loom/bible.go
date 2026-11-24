package main

import (
	"bufio"
	"compress/gzip"
	"embed"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Bibles for the sermon captions demo. See bible/NOTICE.md for sources and
// licences: KJV (public domain), and Biblica's Yoruba and Igbo translations
// (CC BY-SA 4.0). Each file is "BOOK C:V<TAB>text", one verse per line.
//
//go:embed bible/*.tsv.gz
var bibleFiles embed.FS

// bibleLangs maps a caption language to the Bible its scripture is quoted
// from. Nigerian Pidgin has no redistributable Bible, so it quotes the KJV.
var bibleLangs = map[string]string{"en": "kjv", "yo": "yor", "ig": "ibo", "pcm": "kjv"}

type bible map[string]string // "JOH 3:16" -> text

func loadBibles() (map[string]bible, error) {
	out := map[string]bible{}
	for _, id := range []string{"kjv", "yor", "ibo"} {
		f, err := bibleFiles.Open("bible/" + id + ".tsv.gz")
		if err != nil {
			return nil, err
		}
		zr, err := gzip.NewReader(f)
		if err != nil {
			f.Close()
			return nil, err
		}
		b := bible{}
		sc := bufio.NewScanner(zr)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			if ref, text, ok := strings.Cut(sc.Text(), "\t"); ok {
				b[ref] = text
			}
		}
		zr.Close()
		f.Close()
		if err := sc.Err(); err != nil {
			return nil, fmt.Errorf("%s: %w", id, err)
		}
		out[id] = b
	}
	return out, nil
}

// bookNames are the 66 books with eBible's codes, in canon order.
var bookNames = [][2]string{
	{"GEN", "Genesis"}, {"EXO", "Exodus"}, {"LEV", "Leviticus"}, {"NUM", "Numbers"}, {"DEU", "Deuteronomy"},
	{"JOS", "Joshua"}, {"JDG", "Judges"}, {"RUT", "Ruth"}, {"1SA", "1 Samuel"}, {"2SA", "2 Samuel"},
	{"1KI", "1 Kings"}, {"2KI", "2 Kings"}, {"1CH", "1 Chronicles"}, {"2CH", "2 Chronicles"}, {"EZR", "Ezra"},
	{"NEH", "Nehemiah"}, {"EST", "Esther"}, {"JOB", "Job"}, {"PSA", "Psalms"}, {"PRO", "Proverbs"},
	{"ECC", "Ecclesiastes"}, {"SOL", "Song of Solomon"}, {"ISA", "Isaiah"}, {"JER", "Jeremiah"}, {"LAM", "Lamentations"},
	{"EZE", "Ezekiel"}, {"DAN", "Daniel"}, {"HOS", "Hosea"}, {"JOE", "Joel"}, {"AMO", "Amos"},
	{"OBA", "Obadiah"}, {"JON", "Jonah"}, {"MIC", "Micah"}, {"NAH", "Nahum"}, {"HAB", "Habakkuk"},
	{"ZEP", "Zephaniah"}, {"HAG", "Haggai"}, {"ZEC", "Zechariah"}, {"MAL", "Malachi"},
	{"MAT", "Matthew"}, {"MAR", "Mark"}, {"LUK", "Luke"}, {"JOH", "John"}, {"ACT", "Acts"},
	{"ROM", "Romans"}, {"1CO", "1 Corinthians"}, {"2CO", "2 Corinthians"}, {"GAL", "Galatians"}, {"EPH", "Ephesians"},
	{"PHI", "Philippians"}, {"COL", "Colossians"}, {"1TH", "1 Thessalonians"}, {"2TH", "2 Thessalonians"}, {"1TI", "1 Timothy"},
	{"2TI", "2 Timothy"}, {"TIT", "Titus"}, {"PHM", "Philemon"}, {"HEB", "Hebrews"}, {"JAM", "James"},
	{"1PE", "1 Peter"}, {"2PE", "2 Peter"}, {"1JO", "1 John"}, {"2JO", "2 John"}, {"3JO", "3 John"},
	{"JUD", "Jude"}, {"REV", "Revelation"},
}

var bookByCode = func() map[string]string {
	m := map[string]string{}
	for _, b := range bookNames {
		m[b[0]] = b[1]
	}
	return m
}()

// bookCodeList is the list the detector agent chooses codes from.
func bookCodeList() string {
	parts := make([]string, len(bookNames))
	for i, b := range bookNames {
		parts[i] = b[0] + "=" + b[1]
	}
	return strings.Join(parts, ", ")
}

// scriptureRef is one detected passage: up to three consecutive verses.
type scriptureRef struct {
	Book    string `json:"book"`
	Chapter int    `json:"chapter"`
	Verse   int    `json:"verse"`
	To      int    `json:"to,omitempty"`
	Mode    string `json:"mode"` // quoted | cited | paraphrase
}

func (r scriptureRef) label() string {
	s := fmt.Sprintf("%s %d:%d", bookByCode[r.Book], r.Chapter, r.Verse)
	if r.To > r.Verse {
		s += fmt.Sprintf("-%d", r.To)
	}
	return s
}

// passage returns the text of r in one Bible, or "" if any verse is missing.
func (r scriptureRef) passage(b bible) string {
	to := max(r.Verse, r.To)
	var parts []string
	for v := r.Verse; v <= to; v++ {
		t, ok := b[fmt.Sprintf("%s %d:%d", r.Book, r.Chapter, v)]
		if !ok {
			return ""
		}
		parts = append(parts, t)
	}
	return strings.Join(parts, " ")
}

// normalizeRefs keeps refs that exist in the KJV, caps ranges at three
// verses and drops duplicates. A detector can invent a reference; this is
// where an invented one stops.
func normalizeRefs(refs []scriptureRef, kjv bible) []scriptureRef {
	seen := map[string]bool{}
	var out []scriptureRef
	for _, r := range refs {
		r.Book = strings.ToUpper(strings.TrimSpace(r.Book))
		if _, ok := bookByCode[r.Book]; !ok || r.Chapter < 1 || r.Verse < 1 {
			continue
		}
		if r.To < r.Verse {
			r.To = 0
		}
		if r.To > r.Verse+2 {
			r.To = r.Verse + 2
		}
		switch r.Mode {
		case "quoted", "cited", "paraphrase":
		default:
			r.Mode = "cited"
		}
		if r.passage(kjv) == "" || seen[r.label()] {
			continue
		}
		seen[r.label()] = true
		out = append(out, r)
		if len(out) == 3 {
			break
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Rule-based detection, used by the dry run (no model) and as the page's
// "why" for exact citations. It finds "John 3:16", "John 3 verse 16" and
// spoken numbers ("John three sixteen", "Psalm twenty-three verse one").
// ---------------------------------------------------------------------------

var numberWords = map[string]int{
	"one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6, "seven": 7, "eight": 8, "nine": 9,
	"ten": 10, "eleven": 11, "twelve": 12, "thirteen": 13, "fourteen": 14, "fifteen": 15, "sixteen": 16,
	"seventeen": 17, "eighteen": 18, "nineteen": 19, "twenty": 20, "thirty": 30, "forty": 40, "fifty": 50,
	"sixty": 60, "seventy": 70, "eighty": 80, "ninety": 90,
}

// bookPattern matches English book names, longest first, with "Psalm" for "Psalms".
var bookPattern = func() *regexp.Regexp {
	var alts []string
	for i := len(bookNames) - 1; i >= 0; i-- {
		n := regexp.QuoteMeta(bookNames[i][1])
		if bookNames[i][0] == "PSA" {
			n = `Psalms?`
		}
		alts = append(alts, strings.ReplaceAll(n, `\ `, `\s+`))
	}
	return regexp.MustCompile(`(?i)\b(` + strings.Join(alts, "|") + `)\b`)
}()

func bookCode(name string) string {
	name = strings.Join(strings.Fields(strings.ToLower(name)), " ")
	if strings.HasPrefix(name, "psalm") {
		return "PSA"
	}
	for _, b := range bookNames {
		if strings.ToLower(b[1]) == name {
			return b[0]
		}
	}
	return ""
}

// readNumbers reads up to two numbers from the words after a book name:
// digits, "3:16", "chapter 3 verse 16", or number words ("twenty-three").
func readNumbers(rest string) []int {
	rest = strings.NewReplacer(":", " ", "-", " ", ",", " ", ".", " ").Replace(strings.ToLower(rest))
	var nums []int
	pending := -1
	flush := func() {
		if pending >= 0 {
			nums = append(nums, pending)
			pending = -1
		}
	}
	for i, w := range strings.Fields(rest) {
		if i > 8 || len(nums) == 2 {
			break
		}
		switch {
		case w == "chapter" || w == "verse" || w == "verses" || w == "and":
			flush()
			continue
		}
		if n, err := strconv.Atoi(w); err == nil {
			flush()
			nums = append(nums, n)
			continue
		}
		n, ok := numberWords[w]
		if !ok {
			break
		}
		switch {
		case pending >= 20 && pending%10 == 0 && n < 10:
			pending += n // twenty + three
		default:
			flush()
			pending = n
		}
	}
	flush()
	if len(nums) > 2 {
		nums = nums[:2]
	}
	return nums
}

func detectRefs(line string, kjv bible) []scriptureRef {
	var refs []scriptureRef
	for _, m := range bookPattern.FindAllStringSubmatchIndex(line, -1) {
		code := bookCode(line[m[2]:m[3]])
		nums := readNumbers(line[m[1]:])
		if code == "" || len(nums) < 2 {
			continue
		}
		r := scriptureRef{Book: code, Chapter: nums[0], Verse: nums[1], Mode: "cited"}
		if quotes(line, r.passage(kjv)) {
			r.Mode = "quoted"
		}
		refs = append(refs, r)
	}
	return normalizeRefs(refs, kjv)
}

// ---------------------------------------------------------------------------
// The scripture guard's measure: how much of a Bible passage appears, word
// for word, in a caption.
// ---------------------------------------------------------------------------

func words(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r) && !unicode.Is(unicode.Mn, r) && r != 'ʼ' && r != '\''
	})
}

// coverage is the share of the passage's words found in the caption, in
// order (a greedy subsequence match), so scattered common words don't count.
func coverage(caption, passage string) float64 {
	p := words(passage)
	if len(p) == 0 {
		return 1
	}
	c := words(caption)
	i, hit := 0, 0
	for _, w := range p {
		for j := i; j < len(c); j++ {
			if c[j] == w {
				hit++
				i = j + 1
				break
			}
		}
	}
	return float64(hit) / float64(len(p))
}

// quotes reports whether a spoken line reads the passage aloud (rather than
// only naming it): most of the passage's opening words appear in order.
func quotes(line, passage string) bool {
	p := words(passage)
	if len(p) > 12 {
		p = p[:12]
	}
	return len(p) > 0 && coverage(line, strings.Join(p, " ")) >= 0.75
}
