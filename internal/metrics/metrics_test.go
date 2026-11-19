// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"bufio"
	"bytes"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func written(t *testing.T, s *Set) string {
	t.Helper()
	var b bytes.Buffer
	if err := s.Write(&b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// The whole output of a small set, byte for byte: HELP and TYPE before each
// metric's series, metrics in the order they were made, series in the order
// of their label values, histograms with cumulative buckets.
func TestWriteFormat(t *testing.T) {
	t.Parallel()
	s := New()
	plain := s.Counter("app_things_total", "Things that happened.").With()
	byOutcome := s.Counter("app_jobs_total", "Jobs, by how they ended.", L("kind", "listing", "file"), L("outcome", "succeeded", "failed"))
	sparse := s.SparseCounter("app_requests_total", "Requests.", L("method", "GET", "POST"), L("code", "2xx", "5xx"))
	level := s.Gauge("app_queue_length", "Jobs waiting.").With()
	stamp := s.Gauge("app_last_success_timestamp_seconds", "When it last worked.").With()
	ratio := s.Gauge("app_ratio", "A value that is not whole.").With()
	took := s.Histogram("app_duration_seconds", "How long it took.", []float64{0.1, 0.5, 2}, L("route", "api", "ui"))

	plain.Add(41)
	plain.Inc()
	byOutcome.With("file", "failed").Add(3)
	byOutcome.With("listing", "succeeded").Inc()
	sparse.With("POST", "5xx").Inc()
	sparse.With("GET", "2xx").Add(7)
	level.Add(5)
	level.Add(-2)
	stamp.Set(1795000000)
	ratio.Set(0.25)
	for _, v := range []float64{0.05, 0.1, 0.3, 1.5, 30} {
		took.With("api").Observe(v)
	}

	const want = `# HELP app_things_total Things that happened.
# TYPE app_things_total counter
app_things_total 42
# HELP app_jobs_total Jobs, by how they ended.
# TYPE app_jobs_total counter
app_jobs_total{kind="file",outcome="failed"} 3
app_jobs_total{kind="file",outcome="succeeded"} 0
app_jobs_total{kind="listing",outcome="failed"} 0
app_jobs_total{kind="listing",outcome="succeeded"} 1
# HELP app_requests_total Requests.
# TYPE app_requests_total counter
app_requests_total{method="GET",code="2xx"} 7
app_requests_total{method="POST",code="5xx"} 1
# HELP app_queue_length Jobs waiting.
# TYPE app_queue_length gauge
app_queue_length 3
# HELP app_last_success_timestamp_seconds When it last worked.
# TYPE app_last_success_timestamp_seconds gauge
app_last_success_timestamp_seconds 1795000000
# HELP app_ratio A value that is not whole.
# TYPE app_ratio gauge
app_ratio 0.25
# HELP app_duration_seconds How long it took.
# TYPE app_duration_seconds histogram
app_duration_seconds_bucket{route="api",le="0.1"} 2
app_duration_seconds_bucket{route="api",le="0.5"} 3
app_duration_seconds_bucket{route="api",le="2"} 4
app_duration_seconds_bucket{route="api",le="+Inf"} 5
app_duration_seconds_sum{route="api"} 31.95
app_duration_seconds_count{route="api"} 5
app_duration_seconds_bucket{route="ui",le="0.1"} 0
app_duration_seconds_bucket{route="ui",le="0.5"} 0
app_duration_seconds_bucket{route="ui",le="2"} 0
app_duration_seconds_bucket{route="ui",le="+Inf"} 0
app_duration_seconds_sum{route="ui"} 0
app_duration_seconds_count{route="ui"} 0
`
	if got := written(t, s); got != want {
		t.Fatalf("output:\n%s\nwant:\n%s", got, want)
	}
	if plain.Value() != 42 || level.Value() != 3 || took.With("api").Count() != 5 {
		t.Fatalf("values read back: %d, %v, %d", plain.Value(), level.Value(), took.With("api").Count())
	}
}

// A histogram without labels, and values the format writes specially.
func TestWriteValues(t *testing.T) {
	t.Parallel()
	cases := []struct {
		value float64
		want  string
	}{
		{0, "0"}, {1, "1"}, {-3, "-3"}, {0.5, "0.5"}, {1795000000, "1795000000"}, {1795000000.25, "1.79500000025e+09"},
		{4398046511104, "4398046511104"}, {1e21, "1e+21"}, {math.Inf(1), "+Inf"}, {math.Inf(-1), "-Inf"}, {math.NaN(), "NaN"},
	}
	for _, tc := range cases {
		s := New()
		s.Gauge("app_value", "A value.").With().Set(tc.value)
		if got := written(t, s); !strings.HasSuffix(got, "\napp_value "+tc.want+"\n") {
			t.Errorf("%v is written as:\n%s\nwant app_value %s", tc.value, got, tc.want)
		}
	}

	s := New()
	h := s.Histogram("app_size_bytes", "Sizes.", []float64{10, 100}).With()
	h.Observe(10) // a bound is inclusive
	h.Observe(11)
	const want = `# HELP app_size_bytes Sizes.
# TYPE app_size_bytes histogram
app_size_bytes_bucket{le="10"} 1
app_size_bytes_bucket{le="100"} 2
app_size_bytes_bucket{le="+Inf"} 2
app_size_bytes_sum 21
app_size_bytes_count 2
`
	if got := written(t, s); got != want {
		t.Fatalf("output:\n%s\nwant:\n%s", got, want)
	}
}

// A label value and a help text are escaped as the format says: a
// backslash, a line feed and, in a value, a double quote. Anything else,
// including what would end a label set, passes as it is, inside the quotes.
func TestEscaping(t *testing.T) {
	t.Parallel()
	values := []string{`plain`, `back\slash`, `a "quoted" word`, "two\nlines", `C:\dir\"x"` + "\n", `brace} comma, equals= hash#`, `ünïcode ✓`, ``}
	s := New()
	v := s.Counter("app_odd_total", "Help with a back\\slash, a \"quote\"\nand a second line.", L("value", values...))
	for i, value := range values {
		v.With(value).Add(int64(i + 1))
	}
	got := written(t, s)
	for _, line := range []string{
		`# HELP app_odd_total Help with a back\\slash, a "quote"\nand a second line.`,
		`app_odd_total{value="plain"} 1`,
		`app_odd_total{value="back\\slash"} 2`,
		`app_odd_total{value="a \"quoted\" word"} 3`,
		`app_odd_total{value="two\nlines"} 4`,
		`app_odd_total{value="C:\\dir\\\"x\"\n"} 5`,
		`app_odd_total{value="brace} comma, equals= hash#"} 6`,
		`app_odd_total{value="ünïcode ✓"} 7`,
		`app_odd_total{value=""} 8`,
	} {
		if !strings.Contains(got, line+"\n") {
			t.Errorf("missing the line\n  %s\nin:\n%s", line, got)
		}
	}
	// Whatever the values, the output is one line for each sample.
	if n := strings.Count(got, "\n"); n != 2+len(values) {
		t.Fatalf("%d lines, want %d:\n%s", n, 2+len(values), got)
	}
	// And each reads back to the value it was made from.
	sample := regexp.MustCompile(`^app_odd_total\{value="((?:[^"\\]|\\.)*)"\} (\d+)$`)
	unescape := strings.NewReplacer(`\\`, `\`, `\"`, `"`, `\n`, "\n")
	sc := bufio.NewScanner(strings.NewReader(got))
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "#") {
			continue
		}
		m := sample.FindStringSubmatch(sc.Text())
		if m == nil {
			t.Fatalf("a line that does not parse: %s", sc.Text())
		}
		n, _ := strconv.Atoi(m[2])
		if unescape.Replace(m[1]) != values[n-1] {
			t.Errorf("%q read back as %q", values[n-1], unescape.Replace(m[1]))
		}
	}
}

// The series of a metric are the ones its labels declare. Anything else is
// not counted and never appears, so nothing taken from a request can
// become a label value.
func TestOnlyDeclaredLabelValuesAreCounted(t *testing.T) {
	t.Parallel()
	s := New()
	c := s.Counter("app_requests_total", "Requests.", L("route", "api", "other"))
	g := s.Gauge("app_level", "A level.", L("route", "api", "other"))
	h := s.Histogram("app_seconds", "Time.", []float64{1}, L("route", "api", "other"))
	sparse := s.SparseCounter("app_sparse_total", "Sparse.", L("route", "api", "other"))
	before := written(t, s)

	for _, undeclared := range [][]string{{"acme/secret-model"}, {""}, {"API"}, {"api", "extra"}, {}} {
		if c.With(undeclared...) != nil || g.With(undeclared...) != nil || h.With(undeclared...) != nil || sparse.With(undeclared...) != nil {
			t.Fatalf("With(%q) returned a series", undeclared)
		}
		// Counting on what came back does nothing, and does not panic.
		c.With(undeclared...).Inc()
		c.With(undeclared...).Add(5)
		g.With(undeclared...).Set(1)
		g.With(undeclared...).Add(1)
		h.With(undeclared...).Observe(1)
		sparse.With(undeclared...).Inc()
	}
	if after := written(t, s); after != before {
		t.Fatalf("the output changed:\n%s\nwas:\n%s", after, before)
	}
	if strings.Contains(before, "secret") {
		t.Fatal("an undeclared value was written")
	}

	// A handle that was never made counts nothing either.
	var (
		noCounterVec *CounterVec
		noCounter    *Counter
		noGaugeVec   *GaugeVec
		noGauge      *Gauge
		noHistVec    *HistogramVec
		noHist       *Histogram
	)
	noCounterVec.With("api").Inc()
	noCounter.Add(1)
	noGaugeVec.With("api").Set(1)
	noGauge.Add(1)
	noHistVec.With("api").Observe(1)
	noHist.Observe(1)
	if noCounter.Value() != 0 || noGauge.Value() != 0 || noHist.Count() != 0 {
		t.Fatal("a nil handle has a value")
	}

	// A counter only goes up.
	up := s.Counter("app_up_total", "Up.").With()
	up.Add(3)
	up.Add(-2)
	up.Add(0)
	if up.Value() != 3 {
		t.Fatalf("after adding 3 and then -2: %d", up.Value())
	}
}

// Follow brings a counter up to a count kept elsewhere, and never down.
func TestFollow(t *testing.T) {
	t.Parallel()
	s := New()
	c := s.Counter("app_backups_total", "Backups.").With()
	for _, step := range []struct{ total, want uint64 }{{0, 0}, {3, 3}, {3, 3}, {2, 3}, {10, 10}, {0, 10}} {
		c.Follow(step.total)
		if c.Value() != step.want {
			t.Fatalf("after following %d: %d, want %d", step.total, c.Value(), step.want)
		}
	}
	c.Inc()
	if got := written(t, s); !strings.Contains(got, "\napp_backups_total 11\n") {
		t.Fatalf("output:\n%s", got)
	}
	var none *Counter
	none.Follow(5)
}

// Mistakes in how a metric is declared are found when it is made.
func TestDeclarationMistakesPanic(t *testing.T) {
	t.Parallel()
	cases := map[string]func(s *Set){
		"a name with a dash":          func(s *Set) { s.Gauge("app-level", "x") },
		"a name that starts a digit":  func(s *Set) { s.Gauge("1app", "x") },
		"an empty name":               func(s *Set) { s.Gauge("", "x") },
		"a counter without _total":    func(s *Set) { s.Counter("app_requests", "x") },
		"a name used twice":           func(s *Set) { s.Gauge("app_level", "x"); s.Gauge("app_level", "x") },
		"a name a histogram writes":   func(s *Set) { s.Histogram("app_seconds", "x", []float64{1}); s.Gauge("app_seconds_count", "x") },
		"a label with a dash":         func(s *Set) { s.Gauge("app_level", "x", L("my-label", "a")) },
		"a reserved label":            func(s *Set) { s.Gauge("app_level", "x", L("__name__", "a")) },
		"le on a histogram":           func(s *Set) { s.Histogram("app_seconds", "x", []float64{1}, L("le", "a")) },
		"a label with no values":      func(s *Set) { s.Gauge("app_level", "x", L("route")) },
		"a label declared twice":      func(s *Set) { s.Gauge("app_level", "x", L("route", "a"), L("route", "b")) },
		"buckets out of order":        func(s *Set) { s.Histogram("app_seconds", "x", []float64{2, 1}) },
		"a colon in a label's name":   func(s *Set) { s.Gauge("app_level", "x", L("a:b", "a")) },
		"a sparse counter, no _total": func(s *Set) { s.SparseCounter("app_requests", "x") },
	}
	for name, declare := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if recover() == nil {
					t.Fatal("it was accepted")
				}
			}()
			declare(New())
		})
	}
}

// OnWrite runs before each Write, which is how a gauge shows something kept
// elsewhere.
func TestOnWrite(t *testing.T) {
	t.Parallel()
	s := New()
	g := s.Gauge("app_level", "A level.").With()
	level := 0.0
	s.OnWrite(func() { level++; g.Set(level) })
	for want := 1; want <= 3; want++ {
		if got := written(t, s); !strings.Contains(got, "\napp_level "+strconv.Itoa(want)+"\n") {
			t.Fatalf("write %d:\n%s", want, got)
		}
	}
}

// Counting from many goroutines while the set is written loses nothing.
// Run with -race.
func TestConcurrentCounting(t *testing.T) {
	t.Parallel()
	const workers, each = 16, 2000
	s := New()
	c := s.Counter("app_things_total", "Things.", L("kind", "a", "b"))
	sparse := s.SparseCounter("app_sparse_total", "Sparse.", L("kind", "a", "b"), L("code", "2xx", "5xx"))
	g := s.Gauge("app_in_flight", "In flight.").With()
	h := s.Histogram("app_seconds", "Time.", []float64{0.5, 1}).With()
	a := c.With("a")

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wrote := make(chan int)
	go func() { // a scraper, all the while
		n := 0
		for {
			select {
			case <-stop:
				wrote <- n
				return
			default:
				var b bytes.Buffer
				if err := s.Write(&b); err != nil {
					t.Error(err)
				}
				n++
			}
		}
	}()
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				a.Inc()
				c.With("b").Add(2)            // looked up every time
				sparse.With("a", "5xx").Inc() // made by whoever gets there first
				c.With("nobody declared this").Inc()
				g.Add(1)
				h.Observe(float64((w+i)%3) * 0.5)
				g.Add(-1)
			}
		}()
	}
	wg.Wait()
	close(stop)
	if n := <-wrote; n == 0 {
		t.Fatal("nothing was written while counting")
	}

	const total = workers * each
	if a.Value() != total || c.With("b").Value() != 2*total || sparse.With("a", "5xx").Value() != total {
		t.Fatalf("counted %d, %d and %d; want %d, %d and %d", a.Value(), c.With("b").Value(), sparse.With("a", "5xx").Value(), total, 2*total, total)
	}
	if g.Value() != 0 || h.Count() != total {
		t.Fatalf("gauge %v, want 0; histogram count %d, want %d", g.Value(), h.Count(), total)
	}
	got := written(t, s)
	for _, line := range []string{
		"app_things_total{kind=\"a\"} " + strconv.Itoa(total),
		"app_things_total{kind=\"b\"} " + strconv.Itoa(2*total),
		"app_sparse_total{kind=\"a\",code=\"5xx\"} " + strconv.Itoa(total),
		"app_in_flight 0",
		"app_seconds_bucket{le=\"+Inf\"} " + strconv.Itoa(total),
		"app_seconds_count " + strconv.Itoa(total),
	} {
		if !strings.Contains(got, line+"\n") {
			t.Errorf("missing %q in:\n%s", line, got)
		}
	}
}
