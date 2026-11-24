package main

import (
	"strings"
	"testing"
)

func testBibles(t *testing.T) map[string]bible {
	t.Helper()
	b, err := loadBibles()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestBiblesLoad(t *testing.T) {
	b := testBibles(t)
	for id, want := range map[string]string{
		"kjv": "For God so loved the world",
		"yor": "Nítorí Ọlọ́run fẹ́ aráyé",
		"ibo": "Chineke hụrụ ụwa nʼanya",
	} {
		if got := b[id]["JOH 3:16"]; !strings.Contains(got, want) {
			t.Errorf("%s JOH 3:16 = %q, want it to contain %q", id, got, want)
		}
	}
	if strings.ContainsAny(b["kjv"]["PSA 23:1"], "[]¶") {
		t.Errorf("KJV markup left in: %q", b["kjv"]["PSA 23:1"])
	}
}

func TestDetectRefs(t *testing.T) {
	kjv := testBibles(t)["kjv"]
	cases := []struct {
		line, want, mode string
	}{
		{sampleSermon[1], "Matthew 11:28", "quoted"},
		{sampleSermon[4], "Isaiah 40:31", "cited"},
		{sampleSermon[6], "Proverbs 3:5", "quoted"},
		{"Turn with me to Psalm twenty-three verse one.", "Psalms 23:1", "cited"},
		{"John 3 verse 16 is the whole gospel in one line.", "John 3:16", "cited"},
	}
	for _, c := range cases {
		refs := detectRefs(c.line, kjv)
		if len(refs) != 1 || refs[0].label() != c.want || refs[0].Mode != c.mode {
			t.Errorf("detectRefs(%q) = %+v, want %s (%s)", c.line, refs, c.want, c.mode)
		}
	}
	if refs := detectRefs(sampleSermon[3], kjv); len(refs) != 0 {
		t.Errorf("found scripture in a plain line: %+v", refs)
	}
}

func TestNormalizeRefsDropsInventions(t *testing.T) {
	kjv := testBibles(t)["kjv"]
	refs := normalizeRefs([]scriptureRef{
		{Book: "JOH", Chapter: 3, Verse: 16, Mode: "quoted"},
		{Book: "JOH", Chapter: 3, Verse: 16, Mode: "cited"}, // duplicate
		{Book: "JOH", Chapter: 99, Verse: 1},                // no such chapter
		{Book: "XYZ", Chapter: 1, Verse: 1},                 // no such book
		{Book: "rom", Chapter: 8, Verse: 28, To: 40},        // range capped at three verses
	}, kjv)
	if len(refs) != 2 || refs[1].label() != "Romans 8:28-30" || refs[1].Mode != "cited" {
		t.Fatalf("normalizeRefs = %+v", refs)
	}
}

func TestCoverage(t *testing.T) {
	b := testBibles(t)
	ig := b["ibo"]["PRO 3:5"]
	if c := coverage("Pastor kwuru, sị: "+ig, ig); c < 0.99 {
		t.Errorf("a caption quoting the verse covers %.2f", c)
	}
	if c := coverage("Tụkwasị Chineke obi, ọ ga-eme ya.", ig); c >= guardThreshold {
		t.Errorf("a loose paraphrase passes the guard at %.2f", c)
	}
}
