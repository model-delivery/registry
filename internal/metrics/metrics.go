// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

// Package metrics keeps counters, gauges and histograms and writes them in
// the Prometheus text exposition format (version 0.0.4), with the standard
// library alone.
//
// Three things about it are deliberate:
//
//   - The series of a metric are fixed when the metric is made. A label is
//     declared with every value it can take, and a value that was not
//     declared is not counted. Nothing a request carries (a repository, a
//     path, a token's name, an address) can therefore become a label value,
//     and the output cannot grow while the process runs.
//   - Counting is one atomic operation. Callers look a series up once, when
//     they start, and keep the handle; nothing is locked to count.
//   - A nil handle counts nothing. Code that counts does not have to care
//     whether metrics were set up, which keeps tests of that code free of
//     them.
package metrics

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// ContentType is the Content-Type of what Set.Write produces.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// Label is a label of a metric and every value it can take.
type Label struct {
	Name   string
	Values []string
}

// L declares a label and its values.
func L(name string, values ...string) Label { return Label{Name: name, Values: values} }

// The kinds of metric, as the TYPE line names them.
const (
	kindCounter   = "counter"
	kindGauge     = "gauge"
	kindHistogram = "histogram"
)

// Set is a set of metrics, written out in the order they were made.
type Set struct {
	mu       sync.Mutex
	families []*family
	names    map[string]bool
	// collect is run before every Write, to set the gauges that are read
	// from somewhere else when asked for.
	collect []func()
}

// New returns an empty set.
func New() *Set { return &Set{names: map[string]bool{}} }

// family is one metric: its name, its help text and its series.
type family struct {
	name, help, kind string
	labels           []Label
	// buckets is the upper bounds of a histogram, ascending, without +Inf.
	buckets []float64

	mu     sync.RWMutex
	series map[string]*series
}

// series is one set of label values of a metric and its value.
type series struct {
	values []string
	// bits is the value of a counter or a gauge: an unsigned count for a
	// counter, the bits of a float64 for a gauge.
	bits atomic.Uint64
	// For a histogram: how many observations fell in each bucket (the last
	// is +Inf), not cumulative, and the bits of their float64 sum.
	counts []atomic.Uint64
	sum    atomic.Uint64
}

// add registers a metric. A name that is not a metric name, a label that is
// not a label name, or a name used twice is a mistake in the program, found
// the first time it runs: it panics.
func (s *Set) add(name, help, kind string, buckets []float64, labels []Label) *family {
	if !validName(name, true) {
		panic(fmt.Sprintf("metrics: %q is not a metric name", name))
	}
	for i, l := range labels {
		if !validName(l.Name, false) || strings.HasPrefix(l.Name, "__") || l.Name == "le" {
			panic(fmt.Sprintf("metrics: %s: %q is not a label name", name, l.Name))
		}
		if len(l.Values) == 0 {
			panic(fmt.Sprintf("metrics: %s: label %q is declared with no values", name, l.Name))
		}
		for _, other := range labels[:i] {
			if other.Name == l.Name {
				panic(fmt.Sprintf("metrics: %s: label %q is declared twice", name, l.Name))
			}
		}
	}
	if !sort.Float64sAreSorted(buckets) {
		panic(fmt.Sprintf("metrics: %s: histogram buckets are not in ascending order", name))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// A histogram also writes name_bucket, name_sum and name_count.
	taken := []string{name}
	if kind == kindHistogram {
		taken = append(taken, name+"_bucket", name+"_sum", name+"_count")
	}
	for _, n := range taken {
		if s.names[n] {
			panic(fmt.Sprintf("metrics: %s is registered twice", n))
		}
		s.names[n] = true
	}
	f := &family{name: name, help: help, kind: kind, labels: labels, buckets: buckets, series: map[string]*series{}}
	s.families = append(s.families, f)
	return f
}

// validName reports whether s is a metric name, or a label name, as the
// exposition format defines them.
func validName(s string, metric bool) bool {
	if s == "" {
		return false
	}
	for i, c := range s {
		letter := c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (metric && c == ':')
		if !letter && (i == 0 || c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// at returns the series with the given label values, making it on first
// use, or nil when a value was not declared or the count of values is not
// the count of labels.
func (f *family) at(values []string) *series {
	if f == nil || len(values) != len(f.labels) {
		return nil
	}
	key := strings.Join(values, "\x00")
	f.mu.RLock()
	found := f.series[key]
	f.mu.RUnlock()
	if found != nil {
		return found
	}
	for i, v := range values {
		declared := false
		for _, allowed := range f.labels[i].Values {
			declared = declared || allowed == v
		}
		if !declared {
			return nil
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if found = f.series[key]; found != nil {
		return found
	}
	found = &series{values: append([]string(nil), values...)}
	if f.kind == kindHistogram {
		found.counts = make([]atomic.Uint64, len(f.buckets)+1)
	}
	f.series[key] = found
	return found
}

// all makes every series the labels allow, so that each is written, at
// zero, before anything is counted.
func (f *family) all() {
	values := make([]string, len(f.labels))
	var each func(i int)
	each = func(i int) {
		if i == len(f.labels) {
			f.at(values)
			return
		}
		for _, v := range f.labels[i].Values {
			values[i] = v
			each(i + 1)
		}
	}
	each(0)
}

// ---- counters ----

// CounterVec is a counter with labels.
type CounterVec struct{ f *family }

// Counter is one series of a counter: a count that only goes up.
type Counter struct{ s *series }

// Counter makes a counter. Its name ends in _total. Without labels it has
// one series, which With() returns. Every series is written from the start,
// at zero, so that a rate over it is zero and not absent.
func (s *Set) Counter(name, help string, labels ...Label) *CounterVec {
	v := s.SparseCounter(name, help, labels...)
	v.f.all()
	return v
}

// SparseCounter makes a counter whose series are written only once they
// have been asked for with With. It is for a counter with several labels,
// most of whose combinations never occur. The series it can have are still
// only those its labels declare.
func (s *Set) SparseCounter(name, help string, labels ...Label) *CounterVec {
	if !strings.HasSuffix(name, "_total") {
		panic(fmt.Sprintf("metrics: counter %s must be named ..._total", name))
	}
	return &CounterVec{s.add(name, help, kindCounter, nil, labels)}
}

// With returns the series with these label values, in the order the labels
// were declared. For a value that was not declared it returns nil, which
// counts nothing.
func (v *CounterVec) With(values ...string) *Counter {
	if v == nil {
		return nil
	}
	if s := v.f.at(values); s != nil {
		return &Counter{s}
	}
	return nil
}

// Inc adds one.
func (c *Counter) Inc() { c.Add(1) }

// Add adds n, which is not negative: a count of things or of bytes. A
// negative n is ignored.
func (c *Counter) Add(n int64) {
	if c == nil || n <= 0 {
		return
	}
	c.s.bits.Add(uint64(n))
}

// Follow brings the counter up to total, for a count that is kept somewhere
// else and only read here. A total below the counter's value is ignored:
// a counter does not go down.
func (c *Counter) Follow(total uint64) {
	if c == nil {
		return
	}
	for {
		old := c.s.bits.Load()
		if total <= old || c.s.bits.CompareAndSwap(old, total) {
			return
		}
	}
}

// Value is the count so far.
func (c *Counter) Value() uint64 {
	if c == nil {
		return 0
	}
	return c.s.bits.Load()
}

// ---- gauges ----

// GaugeVec is a gauge with labels.
type GaugeVec struct{ f *family }

// Gauge is one series of a gauge: a value that goes up and down.
type Gauge struct{ s *series }

// Gauge makes a gauge. Every series is written from the start, at zero.
func (s *Set) Gauge(name, help string, labels ...Label) *GaugeVec {
	f := s.add(name, help, kindGauge, nil, labels)
	f.all()
	return &GaugeVec{f}
}

// With returns the series with these label values; see CounterVec.With.
func (v *GaugeVec) With(values ...string) *Gauge {
	if v == nil {
		return nil
	}
	if s := v.f.at(values); s != nil {
		return &Gauge{s}
	}
	return nil
}

// Set sets the gauge.
func (g *Gauge) Set(v float64) {
	if g == nil {
		return
	}
	g.s.bits.Store(math.Float64bits(v))
}

// Add adds d, which may be negative.
func (g *Gauge) Add(d float64) {
	if g == nil {
		return
	}
	addFloat(&g.s.bits, d)
}

// Value is the gauge's value.
func (g *Gauge) Value() float64 {
	if g == nil {
		return 0
	}
	return math.Float64frombits(g.s.bits.Load())
}

// addFloat adds d to the float64 whose bits are in a.
func addFloat(a *atomic.Uint64, d float64) {
	for {
		old := a.Load()
		if a.CompareAndSwap(old, math.Float64bits(math.Float64frombits(old)+d)) {
			return
		}
	}
}

// ---- histograms ----

// HistogramVec is a histogram with labels.
type HistogramVec struct{ f *family }

// Histogram is one series of a histogram: observations counted in buckets.
type Histogram struct {
	s       *series
	buckets []float64
}

// Histogram makes a histogram with the given upper bounds, ascending; the
// +Inf bucket is added. Every series is written from the start.
func (s *Set) Histogram(name, help string, buckets []float64, labels ...Label) *HistogramVec {
	f := s.add(name, help, kindHistogram, append([]float64(nil), buckets...), labels)
	f.all()
	return &HistogramVec{f}
}

// With returns the series with these label values; see CounterVec.With.
func (v *HistogramVec) With(values ...string) *Histogram {
	if v == nil {
		return nil
	}
	if s := v.f.at(values); s != nil {
		return &Histogram{s: s, buckets: v.f.buckets}
	}
	return nil
}

// Observe counts one observation of v.
func (h *Histogram) Observe(v float64) {
	if h == nil {
		return
	}
	i := sort.SearchFloat64s(h.buckets, v) // the first bound that is >= v
	h.s.counts[i].Add(1)
	addFloat(&h.s.sum, v)
}

// Count is how many observations were made.
func (h *Histogram) Count() uint64 {
	if h == nil {
		return 0
	}
	var n uint64
	for i := range h.s.counts {
		n += h.s.counts[i].Load()
	}
	return n
}

// ---- writing ----

// OnWrite has fn run before every Write, in the order added. It is where
// gauges that mirror something kept elsewhere are set.
func (s *Set) OnWrite(fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.collect = append(s.collect, fn)
}

// Write writes every metric in the text exposition format: for each, its
// HELP and TYPE lines and then its series, ordered by their label values.
func (s *Set) Write(w io.Writer) error {
	s.mu.Lock()
	collect := append([]func(){}, s.collect...)
	families := append([]*family(nil), s.families...)
	s.mu.Unlock()
	for _, fn := range collect {
		fn()
	}
	var b bytes.Buffer
	for _, f := range families {
		f.write(&b)
	}
	_, err := w.Write(b.Bytes())
	return err
}

func (f *family) write(b *bytes.Buffer) {
	f.mu.RLock()
	all := make([]*series, 0, len(f.series))
	for _, s := range f.series {
		all = append(all, s)
	}
	f.mu.RUnlock()
	sort.Slice(all, func(i, j int) bool {
		for k := range all[i].values {
			if all[i].values[k] != all[j].values[k] {
				return all[i].values[k] < all[j].values[k]
			}
		}
		return false
	})

	b.WriteString("# HELP " + f.name + " " + escapeHelp(f.help) + "\n")
	b.WriteString("# TYPE " + f.name + " " + f.kind + "\n")
	for _, s := range all {
		switch f.kind {
		case kindCounter:
			writeSample(b, f.name, f.labels, s.values, "", strconv.FormatUint(s.bits.Load(), 10))
		case kindGauge:
			writeSample(b, f.name, f.labels, s.values, "", formatFloat(math.Float64frombits(s.bits.Load())))
		case kindHistogram:
			// Buckets are cumulative, and the count is the last of them, so
			// the two agree even while observations arrive.
			var cumulative uint64
			for i := range s.counts {
				cumulative += s.counts[i].Load()
				le := "+Inf"
				if i < len(f.buckets) {
					le = formatFloat(f.buckets[i])
				}
				writeSample(b, f.name+"_bucket", f.labels, s.values, le, strconv.FormatUint(cumulative, 10))
			}
			writeSample(b, f.name+"_sum", f.labels, s.values, "", formatFloat(math.Float64frombits(s.sum.Load())))
			writeSample(b, f.name+"_count", f.labels, s.values, "", strconv.FormatUint(cumulative, 10))
		}
	}
}

// writeSample writes one line: the name, its labels (and le, for a bucket)
// and the value.
func writeSample(b *bytes.Buffer, name string, labels []Label, values []string, le, value string) {
	b.WriteString(name)
	if len(labels) > 0 || le != "" {
		b.WriteByte('{')
		for i, l := range labels {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(l.Name + `="` + escapeValue(values[i]) + `"`)
		}
		if le != "" {
			if len(labels) > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`le="` + le + `"`)
		}
		b.WriteByte('}')
	}
	b.WriteByte(' ')
	b.WriteString(value)
	b.WriteByte('\n')
}

// formatFloat writes a value the way the format reads one. A whole number
// is written without an exponent, so a timestamp in seconds stays readable.
func formatFloat(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case v == math.Trunc(v) && math.Abs(v) < 1e15:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// escapeHelp escapes a help text: a backslash and a line feed.
func escapeHelp(s string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(s)
}

// escapeValue escapes a label value: a backslash, a double quote and a
// line feed.
func escapeValue(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}
