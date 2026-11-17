package webtools

import (
	"strings"
	"testing"
	"time"
)

func TestSliceWindowPlain(t *testing.T) {
	md := []rune(strings.Repeat("a", 1000))
	w := sliceWindow(md, 0, 500, nil)
	if w.Start != 0 || w.End != 500 || w.Found != nil {
		t.Fatalf("window = %+v", w)
	}
	w = sliceWindow(md, 900, 500, nil)
	if w.Start != 900 || w.End != 1000 {
		t.Fatalf("tail window = %+v", w)
	}
	w = sliceWindow(md, 5000, 500, nil)
	if w.Start != 1000 || w.End != 1000 {
		t.Fatalf("past the end = %+v", w)
	}
}

func TestSliceWindowBreaks(t *testing.T) {
	// A paragraph break at 450 is within the last 20 % of a 500 window.
	md := []rune(strings.Repeat("a", 450) + "\n\n" + strings.Repeat("b", 600))
	w := sliceWindow(md, 0, 500, nil)
	if w.End != 452 {
		t.Fatalf("end = %d, want 452 (after the paragraph break)", w.End)
	}
	// A break at 100 is too early: cut at the hard limit.
	md = []rune(strings.Repeat("a", 100) + "\n\n" + strings.Repeat("b", 900))
	if w := sliceWindow(md, 0, 500, nil); w.End != 500 {
		t.Fatalf("end = %d, want 500", w.End)
	}
	// A line break is used when there is no paragraph break.
	md = []rune(strings.Repeat("a", 480) + "\n" + strings.Repeat("b", 600))
	if w := sliceWindow(md, 0, 500, nil); w.End != 481 {
		t.Fatalf("end = %d, want 481", w.End)
	}
	// Code points, not bytes.
	md = []rune(strings.Repeat("ş", 600))
	if w := sliceWindow(md, 0, 500, nil); w.End != 500 {
		t.Fatalf("end = %d, want 500 runes", w.End)
	}
}

func TestSliceWindowFind(t *testing.T) {
	text := strings.Repeat("x", 1000) + "\nKabataş Erkek Lisesi 1908 yılında kuruldu.\n" + strings.Repeat("y", 2000)
	md := []rune(text)
	w := sliceWindow(md, 0, 500, []rune("LİSESİ"))
	// Turkish dotted capital İ lowercases rune-wise to i.
	if w.Found == nil || !*w.Found {
		t.Fatalf("find = %+v", w)
	}
	match := strings.Index(text, "Lisesi")
	if w.MatchAt != len([]rune(text[:match])) {
		t.Fatalf("matchAt = %d", w.MatchAt)
	}
	// Starts at the line start (the match is less than 300 into its line).
	if w.Start != 1001 {
		t.Fatalf("start = %d, want 1001 (line start)", w.Start)
	}
	if w.End <= w.MatchAt {
		t.Fatal("window must include the match")
	}

	// Accent-sensitive: "kabatas" does not match "Kabataş".
	if w := sliceWindow(md, 0, 500, []rune("kabatas ")); w.Found == nil || *w.Found {
		t.Fatalf("accent-insensitive match: %+v", w)
	}
	// Search starts at offset.
	if w := sliceWindow(md, 1100, 500, []rune("Kabataş")); w.Found == nil || *w.Found || w.End != w.Start {
		t.Fatalf("find before offset: %+v", w)
	}
	// Long line: at most 300 characters of context.
	long := []rune(strings.Repeat("z", 2000) + "needle" + strings.Repeat("z", 2000))
	w = sliceWindow(long, 0, 1000, []rune("NEEDLE"))
	if w.Start != 2000-findContext || w.MatchAt != 2000 {
		t.Fatalf("long line window = %+v", w)
	}
}

func TestPageCacheTTL(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	c := newPageCache(func() time.Time { return now })
	p := &page{URL: "https://example.com/final"}
	c.put("key-a", p, "https://example.com/start", p.URL)
	if c.get("key-a", "https://example.com/start") != p || c.get("key-a", p.URL) != p {
		t.Fatal("cache miss right after put")
	}
	if c.get("key-b", p.URL) != nil {
		t.Fatal("cache is per key")
	}
	now = now.Add(pageCacheTTL - time.Second)
	if c.get("key-a", p.URL) == nil {
		t.Fatal("expired too early")
	}
	now = now.Add(2 * time.Second)
	if c.get("key-a", p.URL) != nil {
		t.Fatal("entry outlived its TTL")
	}
}

func TestPageCacheEviction(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	c := newPageCache(func() time.Time { return now })
	for i := 0; i < pageCacheEntries+50; i++ {
		now = now.Add(time.Millisecond)
		c.put("k", &page{}, strings.Repeat("u", i+1))
	}
	if len(c.entries) > pageCacheEntries {
		t.Fatalf("cache holds %d entries", len(c.entries))
	}
}

func TestLimiterSlidingWindow(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	l := newLimiter(func() time.Time { return now })
	for i := 0; i < SearchPerMinute; i++ {
		release, terr := l.acquire("k", "search")
		if terr != nil {
			t.Fatalf("search %d refused: %v", i, terr)
		}
		release()
		now = now.Add(time.Second)
	}
	_, terr := l.acquire("k", "search")
	if terr == nil || terr.Code != CodeRateLimited || terr.HTTPStatus != 429 {
		t.Fatalf("21st search = %v", terr)
	}
	// The oldest call was 20 s ago, so it leaves the window in 40 s.
	if terr.RetryAfter != 40 {
		t.Fatalf("retry after = %d, want 40", terr.RetryAfter)
	}
	// Fetches have their own budget; other keys too.
	if release, terr := l.acquire("k", "fetch"); terr != nil {
		t.Fatalf("fetch refused: %v", terr)
	} else {
		release()
	}
	if release, terr := l.acquire("other", "search"); terr != nil {
		t.Fatalf("other key refused: %v", terr)
	} else {
		release()
	}
	now = now.Add(41 * time.Second)
	if release, terr := l.acquire("k", "search"); terr != nil {
		t.Fatalf("window did not slide: %v", terr)
	} else {
		release()
	}
}

func TestLimiterFetchBudgetAndConcurrency(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	l := newLimiter(func() time.Time { return now })
	for i := 0; i < FetchPerMinute; i++ {
		release, terr := l.acquire("k", "fetch")
		if terr != nil {
			t.Fatalf("fetch %d refused: %v", i, terr)
		}
		release()
	}
	if _, terr := l.acquire("k", "fetch"); terr == nil || terr.Code != CodeRateLimited {
		t.Fatalf("41st fetch = %v", terr)
	}

	var releases []func()
	for i := 0; i < MaxConcurrentKey; i++ {
		release, terr := l.acquire("c", "search")
		if terr != nil {
			t.Fatalf("concurrent %d refused: %v", i, terr)
		}
		releases = append(releases, release)
	}
	if _, terr := l.acquire("c", "fetch"); terr == nil || terr.Code != CodeRateLimited || terr.RetryAfter != 1 {
		t.Fatalf("5th concurrent call = %v", terr)
	}
	releases[0]()
	releases[0]() // releasing twice is harmless
	if release, terr := l.acquire("c", "fetch"); terr != nil {
		t.Fatalf("after release: %v", terr)
	} else {
		release()
	}
}
