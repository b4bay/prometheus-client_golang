// Copyright 2015 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package prometheus

import (
	"math"
	"math/rand"
	"reflect"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"testing/quick"
	"time"

	//nolint:staticcheck // Ignore SA1019. Need to keep deprecated package for compatibility.
	"github.com/golang/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	dto "github.com/prometheus/client_model/go"
)

func benchmarkHistogramObserve(w int, b *testing.B) {
	b.StopTimer()

	wg := new(sync.WaitGroup)
	wg.Add(w)

	g := new(sync.WaitGroup)
	g.Add(1)

	s := NewHistogram(HistogramOpts{})

	for i := 0; i < w; i++ {
		go func() {
			g.Wait()

			for i := 0; i < b.N; i++ {
				s.Observe(float64(i))
			}

			wg.Done()
		}()
	}

	b.StartTimer()
	g.Done()
	wg.Wait()
}

func BenchmarkHistogramObserve1(b *testing.B) {
	benchmarkHistogramObserve(1, b)
}

func BenchmarkHistogramObserve2(b *testing.B) {
	benchmarkHistogramObserve(2, b)
}

func BenchmarkHistogramObserve4(b *testing.B) {
	benchmarkHistogramObserve(4, b)
}

func BenchmarkHistogramObserve8(b *testing.B) {
	benchmarkHistogramObserve(8, b)
}

func benchmarkHistogramWrite(w int, b *testing.B) {
	b.StopTimer()

	wg := new(sync.WaitGroup)
	wg.Add(w)

	g := new(sync.WaitGroup)
	g.Add(1)

	s := NewHistogram(HistogramOpts{})

	for i := 0; i < 1000000; i++ {
		s.Observe(float64(i))
	}

	for j := 0; j < w; j++ {
		outs := make([]dto.Metric, b.N)

		go func(o []dto.Metric) {
			g.Wait()

			for i := 0; i < b.N; i++ {
				s.Write(&o[i])
			}

			wg.Done()
		}(outs)
	}

	b.StartTimer()
	g.Done()
	wg.Wait()
}

func BenchmarkHistogramWrite1(b *testing.B) {
	benchmarkHistogramWrite(1, b)
}

func BenchmarkHistogramWrite2(b *testing.B) {
	benchmarkHistogramWrite(2, b)
}

func BenchmarkHistogramWrite4(b *testing.B) {
	benchmarkHistogramWrite(4, b)
}

func BenchmarkHistogramWrite8(b *testing.B) {
	benchmarkHistogramWrite(8, b)
}

func TestHistogramNonMonotonicBuckets(t *testing.T) {
	testCases := map[string][]float64{
		"not strictly monotonic":  {1, 2, 2, 3},
		"not monotonic at all":    {1, 2, 4, 3, 5},
		"have +Inf in the middle": {1, 2, math.Inf(+1), 3},
	}
	for name, buckets := range testCases {
		func() {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("Buckets %v are %s but NewHistogram did not panic.", buckets, name)
				}
			}()
			_ = NewHistogram(HistogramOpts{
				Name:    "test_histogram",
				Help:    "helpless",
				Buckets: buckets,
			})
		}()
	}
}

// Intentionally adding +Inf here to test if that case is handled correctly.
// Also, getCumulativeCounts depends on it.
var testBuckets = []float64{-2, -1, -0.5, 0, 0.5, 1, 2, math.Inf(+1)}

func TestHistogramConcurrency(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping test in short mode.")
	}

	rand.Seed(42)

	it := func(n uint32) bool {
		mutations := int(n%1e4 + 1e4)
		concLevel := int(n%5 + 1)
		total := mutations * concLevel

		var start, end sync.WaitGroup
		start.Add(1)
		end.Add(concLevel)

		his := NewHistogram(HistogramOpts{
			Name:    "test_histogram",
			Help:    "helpless",
			Buckets: testBuckets,
		})

		allVars := make([]float64, total)
		var sampleSum float64
		for i := 0; i < concLevel; i++ {
			vals := make([]float64, mutations)
			for j := 0; j < mutations; j++ {
				v := rand.NormFloat64()
				vals[j] = v
				allVars[i*mutations+j] = v
				sampleSum += v
			}

			go func(vals []float64) {
				start.Wait()
				for _, v := range vals {
					if n%2 == 0 {
						his.Observe(v)
					} else {
						his.(ExemplarObserver).ObserveWithExemplar(v, Labels{"foo": "bar"})
					}
				}
				end.Done()
			}(vals)
		}
		sort.Float64s(allVars)
		start.Done()
		end.Wait()

		m := &dto.Metric{}
		his.Write(m)
		if got, want := int(*m.Histogram.SampleCount), total; got != want {
			t.Errorf("got sample count %d, want %d", got, want)
		}
		if got, want := *m.Histogram.SampleSum, sampleSum; math.Abs((got-want)/want) > 0.001 {
			t.Errorf("got sample sum %f, want %f", got, want)
		}

		wantCounts := getCumulativeCounts(allVars)
		wantBuckets := len(testBuckets)
		if !math.IsInf(m.Histogram.Bucket[len(m.Histogram.Bucket)-1].GetUpperBound(), +1) {
			wantBuckets--
		}

		if got := len(m.Histogram.Bucket); got != wantBuckets {
			t.Errorf("got %d buckets in protobuf, want %d", got, wantBuckets)
		}
		for i, wantBound := range testBuckets {
			if i == len(testBuckets)-1 {
				break // No +Inf bucket in protobuf.
			}
			if gotBound := *m.Histogram.Bucket[i].UpperBound; gotBound != wantBound {
				t.Errorf("got bound %f, want %f", gotBound, wantBound)
			}
			if gotCount, wantCount := *m.Histogram.Bucket[i].CumulativeCount, wantCounts[i]; gotCount != wantCount {
				t.Errorf("got count %d, want %d", gotCount, wantCount)
			}
		}
		return true
	}

	if err := quick.Check(it, nil); err != nil {
		t.Error(err)
	}
}

func TestHistogramVecConcurrency(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping test in short mode.")
	}

	rand.Seed(42)

	it := func(n uint32) bool {
		mutations := int(n%1e4 + 1e4)
		concLevel := int(n%7 + 1)
		vecLength := int(n%3 + 1)

		var start, end sync.WaitGroup
		start.Add(1)
		end.Add(concLevel)

		his := NewHistogramVec(
			HistogramOpts{
				Name:    "test_histogram",
				Help:    "helpless",
				Buckets: []float64{-2, -1, -0.5, 0, 0.5, 1, 2, math.Inf(+1)},
			},
			[]string{"label"},
		)

		allVars := make([][]float64, vecLength)
		sampleSums := make([]float64, vecLength)
		for i := 0; i < concLevel; i++ {
			vals := make([]float64, mutations)
			picks := make([]int, mutations)
			for j := 0; j < mutations; j++ {
				v := rand.NormFloat64()
				vals[j] = v
				pick := rand.Intn(vecLength)
				picks[j] = pick
				allVars[pick] = append(allVars[pick], v)
				sampleSums[pick] += v
			}

			go func(vals []float64) {
				start.Wait()
				for i, v := range vals {
					his.WithLabelValues(string('A' + rune(picks[i]))).Observe(v)
				}
				end.Done()
			}(vals)
		}
		for _, vars := range allVars {
			sort.Float64s(vars)
		}
		start.Done()
		end.Wait()

		for i := 0; i < vecLength; i++ {
			m := &dto.Metric{}
			s := his.WithLabelValues(string('A' + rune(i)))
			s.(Histogram).Write(m)

			if got, want := len(m.Histogram.Bucket), len(testBuckets)-1; got != want {
				t.Errorf("got %d buckets in protobuf, want %d", got, want)
			}
			if got, want := int(*m.Histogram.SampleCount), len(allVars[i]); got != want {
				t.Errorf("got sample count %d, want %d", got, want)
			}
			if got, want := *m.Histogram.SampleSum, sampleSums[i]; math.Abs((got-want)/want) > 0.001 {
				t.Errorf("got sample sum %f, want %f", got, want)
			}

			wantCounts := getCumulativeCounts(allVars[i])

			for j, wantBound := range testBuckets {
				if j == len(testBuckets)-1 {
					break // No +Inf bucket in protobuf.
				}
				if gotBound := *m.Histogram.Bucket[j].UpperBound; gotBound != wantBound {
					t.Errorf("got bound %f, want %f", gotBound, wantBound)
				}
				if gotCount, wantCount := *m.Histogram.Bucket[j].CumulativeCount, wantCounts[j]; gotCount != wantCount {
					t.Errorf("got count %d, want %d", gotCount, wantCount)
				}
			}
		}
		return true
	}

	if err := quick.Check(it, nil); err != nil {
		t.Error(err)
	}
}

func getCumulativeCounts(vars []float64) []uint64 {
	counts := make([]uint64, len(testBuckets))
	for _, v := range vars {
		for i := len(testBuckets) - 1; i >= 0; i-- {
			if v > testBuckets[i] {
				break
			}
			counts[i]++
		}
	}
	return counts
}

func TestBuckets(t *testing.T) {
	got := LinearBuckets(-15, 5, 6)
	want := []float64{-15, -10, -5, 0, 5, 10}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("linear buckets: got %v, want %v", got, want)
	}

	got = ExponentialBuckets(100, 1.2, 3)
	want = []float64{100, 120, 144}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("exponential buckets: got %v, want %v", got, want)
	}

	got = ExponentialBucketsRange(1, 100, 10)
	want = []float64{
		1.0, 1.6681005372000588, 2.782559402207125,
		4.641588833612779, 7.742636826811273, 12.915496650148842,
		21.544346900318846, 35.93813663804629, 59.94842503189414,
		100.00000000000007,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("exponential buckets range: got %v, want %v", got, want)
	}
}

func TestHistogramAtomicObserve(t *testing.T) {
	var (
		quit = make(chan struct{})
		his  = NewHistogram(HistogramOpts{
			Buckets: []float64{0.5, 10, 20},
		})
	)

	defer func() { close(quit) }()

	observe := func() {
		for {
			select {
			case <-quit:
				return
			default:
				his.Observe(1)
			}
		}
	}

	go observe()
	go observe()
	go observe()

	for i := 0; i < 100; i++ {
		m := &dto.Metric{}
		if err := his.Write(m); err != nil {
			t.Fatal("unexpected error writing histogram:", err)
		}
		h := m.GetHistogram()
		if h.GetSampleCount() != uint64(h.GetSampleSum()) ||
			h.GetSampleCount() != h.GetBucket()[1].GetCumulativeCount() ||
			h.GetSampleCount() != h.GetBucket()[2].GetCumulativeCount() {
			t.Fatalf(
				"inconsistent counts in histogram: count=%d sum=%f buckets=[%d, %d]",
				h.GetSampleCount(), h.GetSampleSum(),
				h.GetBucket()[1].GetCumulativeCount(), h.GetBucket()[2].GetCumulativeCount(),
			)
		}
		runtime.Gosched()
	}
}

func TestHistogramExemplar(t *testing.T) {
	now := time.Now()

	histogram := NewHistogram(HistogramOpts{
		Name:    "test",
		Help:    "test help",
		Buckets: []float64{1, 2, 3, 4},
	}).(*histogram)
	histogram.now = func() time.Time { return now }

	ts := timestamppb.New(now)
	if err := ts.CheckValid(); err != nil {
		t.Fatal(err)
	}
	expectedExemplars := []*dto.Exemplar{
		nil,
		{
			Label: []*dto.LabelPair{
				{Name: proto.String("id"), Value: proto.String("2")},
			},
			Value:     proto.Float64(1.6),
			Timestamp: ts,
		},
		nil,
		{
			Label: []*dto.LabelPair{
				{Name: proto.String("id"), Value: proto.String("3")},
			},
			Value:     proto.Float64(4),
			Timestamp: ts,
		},
		{
			Label: []*dto.LabelPair{
				{Name: proto.String("id"), Value: proto.String("4")},
			},
			Value:     proto.Float64(4.5),
			Timestamp: ts,
		},
	}

	histogram.ObserveWithExemplar(1.5, Labels{"id": "1"})
	histogram.ObserveWithExemplar(1.6, Labels{"id": "2"}) // To replace exemplar in bucket 0.
	histogram.ObserveWithExemplar(4, Labels{"id": "3"})
	histogram.ObserveWithExemplar(4.5, Labels{"id": "4"}) // Should go to +Inf bucket.

	for i, ex := range histogram.exemplars {
		var got, expected string
		if val := ex.Load(); val != nil {
			got = val.(*dto.Exemplar).String()
		}
		if expectedExemplars[i] != nil {
			expected = expectedExemplars[i].String()
		}
		if got != expected {
			t.Errorf("expected exemplar %s, got %s.", expected, got)
		}
	}
}

func TestSparseHistogram(t *testing.T) {
	scenarios := []struct {
		name             string
		observations     []float64 // With simulated interval of 1m.
		factor           float64
		zeroThreshold    float64
		maxBuckets       uint32
		minResetDuration time.Duration
		maxZeroThreshold float64
		want             string // String representation of protobuf.
	}{
		{
			name:         "no sparse buckets",
			observations: []float64{1, 2, 3},
			factor:       1,
			want:         `sample_count:3 sample_sum:6 bucket:<cumulative_count:0 upper_bound:0.005 > bucket:<cumulative_count:0 upper_bound:0.01 > bucket:<cumulative_count:0 upper_bound:0.025 > bucket:<cumulative_count:0 upper_bound:0.05 > bucket:<cumulative_count:0 upper_bound:0.1 > bucket:<cumulative_count:0 upper_bound:0.25 > bucket:<cumulative_count:0 upper_bound:0.5 > bucket:<cumulative_count:1 upper_bound:1 > bucket:<cumulative_count:2 upper_bound:2.5 > bucket:<cumulative_count:3 upper_bound:5 > bucket:<cumulative_count:3 upper_bound:10 > `, // Has conventional buckets because there are no sparse buckets.
		},
		{
			name:         "factor 1.1 results in schema 3",
			observations: []float64{0, 1, 2, 3},
			factor:       1.1,
			want:         `sample_count:4 sample_sum:6 sb_schema:3 sb_zero_threshold:2.938735877055719e-39 sb_zero_count:1 sb_positive:<span:<offset:0 length:1 > span:<offset:7 length:1 > span:<offset:4 length:1 > delta:1 delta:0 delta:0 > `,
		},
		{
			name:         "factor 1.2 results in schema 2",
			observations: []float64{0, 1, 1.2, 1.4, 1.8, 2},
			factor:       1.2,
			want:         `sample_count:6 sample_sum:7.4 sb_schema:2 sb_zero_threshold:2.938735877055719e-39 sb_zero_count:1 sb_positive:<span:<offset:0 length:5 > delta:1 delta:-1 delta:2 delta:-2 delta:2 > `,
		},
		{
			name: "factor 4 results in schema -1",
			observations: []float64{
				0.5, 1, // Bucket 0: (0.25, 1]
				1.5, 2, 3, 3.5, // Bucket 1: (1, 4]
				5, 6, 7, // Bucket 2: (4, 16]
				33.33, // Bucket 3: (16, 64]
			},
			factor: 4,
			want:   `sample_count:10 sample_sum:62.83 sb_schema:-1 sb_zero_threshold:2.938735877055719e-39 sb_zero_count:0 sb_positive:<span:<offset:0 length:4 > delta:2 delta:2 delta:-1 delta:-2 > `,
		},
		{
			name: "factor 17 results in schema -2",
			observations: []float64{
				0.5, 1, // Bucket 0: (0.0625, 1]
				1.5, 2, 3, 3.5, 5, 6, 7, // Bucket 1: (1, 16]
				33.33, // Bucket 2: (16, 256]
			},
			factor: 17,
			want:   `sample_count:10 sample_sum:62.83 sb_schema:-2 sb_zero_threshold:2.938735877055719e-39 sb_zero_count:0 sb_positive:<span:<offset:0 length:3 > delta:2 delta:5 delta:-6 > `,
		},
		{
			name:         "negative buckets",
			observations: []float64{0, -1, -1.2, -1.4, -1.8, -2},
			factor:       1.2,
			want:         `sample_count:6 sample_sum:-7.4 sb_schema:2 sb_zero_threshold:2.938735877055719e-39 sb_zero_count:1 sb_negative:<span:<offset:0 length:5 > delta:1 delta:-1 delta:2 delta:-2 delta:2 > `,
		},
		{
			name:         "negative and positive buckets",
			observations: []float64{0, -1, -1.2, -1.4, -1.8, -2, 1, 1.2, 1.4, 1.8, 2},
			factor:       1.2,
			want:         `sample_count:11 sample_sum:0 sb_schema:2 sb_zero_threshold:2.938735877055719e-39 sb_zero_count:1 sb_negative:<span:<offset:0 length:5 > delta:1 delta:-1 delta:2 delta:-2 delta:2 > sb_positive:<span:<offset:0 length:5 > delta:1 delta:-1 delta:2 delta:-2 delta:2 > `,
		},
		{
			name:          "wide zero bucket",
			observations:  []float64{0, -1, -1.2, -1.4, -1.8, -2, 1, 1.2, 1.4, 1.8, 2},
			factor:        1.2,
			zeroThreshold: 1.4,
			want:          `sample_count:11 sample_sum:0 sb_schema:2 sb_zero_threshold:1.4 sb_zero_count:7 sb_negative:<span:<offset:4 length:1 > delta:2 > sb_positive:<span:<offset:4 length:1 > delta:2 > `,
		},
		{
			name:         "NaN observation",
			observations: []float64{0, 1, 1.2, 1.4, 1.8, 2, math.NaN()},
			factor:       1.2,
			want:         `sample_count:7 sample_sum:nan sb_schema:2 sb_zero_threshold:2.938735877055719e-39 sb_zero_count:1 sb_positive:<span:<offset:0 length:5 > delta:1 delta:-1 delta:2 delta:-2 delta:2 > `,
		},
		{
			name:         "+Inf observation",
			observations: []float64{0, 1, 1.2, 1.4, 1.8, 2, math.Inf(+1)},
			factor:       1.2,
			want:         `sample_count:7 sample_sum:inf sb_schema:2 sb_zero_threshold:2.938735877055719e-39 sb_zero_count:1 sb_positive:<span:<offset:0 length:5 > span:<offset:2147483642 length:1 > delta:1 delta:-1 delta:2 delta:-2 delta:2 delta:-1 > `,
		},
		{
			name:         "-Inf observation",
			observations: []float64{0, 1, 1.2, 1.4, 1.8, 2, math.Inf(-1)},
			factor:       1.2,
			want:         `sample_count:7 sample_sum:-inf sb_schema:2 sb_zero_threshold:2.938735877055719e-39 sb_zero_count:1 sb_negative:<span:<offset:2147483647 length:1 > delta:1 > sb_positive:<span:<offset:0 length:5 > delta:1 delta:-1 delta:2 delta:-2 delta:2 > `,
		},
		{
			name:         "limited buckets but nothing triggered",
			observations: []float64{0, 1, 1.2, 1.4, 1.8, 2},
			factor:       1.2,
			maxBuckets:   4,
			want:         `sample_count:6 sample_sum:7.4 sb_schema:2 sb_zero_threshold:2.938735877055719e-39 sb_zero_count:1 sb_positive:<span:<offset:0 length:5 > delta:1 delta:-1 delta:2 delta:-2 delta:2 > `,
		},
		{
			name:         "buckets limited by halving resolution",
			observations: []float64{0, 1, 1.1, 1.2, 1.4, 1.8, 2, 3},
			factor:       1.2,
			maxBuckets:   4,
			want:         `sample_count:8 sample_sum:11.5 sb_schema:1 sb_zero_threshold:2.938735877055719e-39 sb_zero_count:1 sb_positive:<span:<offset:0 length:5 > delta:1 delta:2 delta:-1 delta:-2 delta:1 > `,
		},
		{
			name:             "buckets limited by widening the zero bucket",
			observations:     []float64{0, 1, 1.1, 1.2, 1.4, 1.8, 2, 3},
			factor:           1.2,
			maxBuckets:       4,
			maxZeroThreshold: 1.2,
			want:             `sample_count:8 sample_sum:11.5 sb_schema:2 sb_zero_threshold:1 sb_zero_count:2 sb_positive:<span:<offset:1 length:7 > delta:1 delta:1 delta:-2 delta:2 delta:-2 delta:0 delta:1 > `,
		},
		{
			name:             "buckets limited by widening the zero bucket twice",
			observations:     []float64{0, 1, 1.1, 1.2, 1.4, 1.8, 2, 3, 4},
			factor:           1.2,
			maxBuckets:       4,
			maxZeroThreshold: 1.2,
			want:             `sample_count:9 sample_sum:15.5 sb_schema:2 sb_zero_threshold:1.189207115002721 sb_zero_count:3 sb_positive:<span:<offset:2 length:7 > delta:2 delta:-2 delta:2 delta:-2 delta:0 delta:1 delta:0 > `,
		},
		{
			name:             "buckets limited by reset",
			observations:     []float64{0, 1, 1.1, 1.2, 1.4, 1.8, 2, 3, 4},
			factor:           1.2,
			maxBuckets:       4,
			maxZeroThreshold: 1.2,
			minResetDuration: 5 * time.Minute,
			want:             `sample_count:2 sample_sum:7 sb_schema:2 sb_zero_threshold:2.938735877055719e-39 sb_zero_count:0 sb_positive:<span:<offset:7 length:2 > delta:1 delta:0 > `,
		},
		{
			name:         "limited buckets but nothing triggered, negative observations",
			observations: []float64{0, -1, -1.2, -1.4, -1.8, -2},
			factor:       1.2,
			maxBuckets:   4,
			want:         `sample_count:6 sample_sum:-7.4 sb_schema:2 sb_zero_threshold:2.938735877055719e-39 sb_zero_count:1 sb_negative:<span:<offset:0 length:5 > delta:1 delta:-1 delta:2 delta:-2 delta:2 > `,
		},
		{
			name:         "buckets limited by halving resolution, negative observations",
			observations: []float64{0, -1, -1.1, -1.2, -1.4, -1.8, -2, -3},
			factor:       1.2,
			maxBuckets:   4,
			want:         `sample_count:8 sample_sum:-11.5 sb_schema:1 sb_zero_threshold:2.938735877055719e-39 sb_zero_count:1 sb_negative:<span:<offset:0 length:5 > delta:1 delta:2 delta:-1 delta:-2 delta:1 > `,
		},
		{
			name:             "buckets limited by widening the zero bucket, negative observations",
			observations:     []float64{0, -1, -1.1, -1.2, -1.4, -1.8, -2, -3},
			factor:           1.2,
			maxBuckets:       4,
			maxZeroThreshold: 1.2,
			want:             `sample_count:8 sample_sum:-11.5 sb_schema:2 sb_zero_threshold:1 sb_zero_count:2 sb_negative:<span:<offset:1 length:7 > delta:1 delta:1 delta:-2 delta:2 delta:-2 delta:0 delta:1 > `,
		},
		{
			name:             "buckets limited by widening the zero bucket twice, negative observations",
			observations:     []float64{0, -1, -1.1, -1.2, -1.4, -1.8, -2, -3, -4},
			factor:           1.2,
			maxBuckets:       4,
			maxZeroThreshold: 1.2,
			want:             `sample_count:9 sample_sum:-15.5 sb_schema:2 sb_zero_threshold:1.189207115002721 sb_zero_count:3 sb_negative:<span:<offset:2 length:7 > delta:2 delta:-2 delta:2 delta:-2 delta:0 delta:1 delta:0 > `,
		},
		{
			name:             "buckets limited by reset, negative observations",
			observations:     []float64{0, -1, -1.1, -1.2, -1.4, -1.8, -2, -3, -4},
			factor:           1.2,
			maxBuckets:       4,
			maxZeroThreshold: 1.2,
			minResetDuration: 5 * time.Minute,
			want:             `sample_count:2 sample_sum:-7 sb_schema:2 sb_zero_threshold:2.938735877055719e-39 sb_zero_count:0 sb_negative:<span:<offset:7 length:2 > delta:1 delta:0 > `,
		},
		{
			name:             "buckets limited by halving resolution, then reset",
			observations:     []float64{0, 1, 1.1, 1.2, 1.4, 1.8, 2, 5, 5.1, 3, 4},
			factor:           1.2,
			maxBuckets:       4,
			minResetDuration: 9 * time.Minute,
			want:             `sample_count:2 sample_sum:7 sb_schema:2 sb_zero_threshold:2.938735877055719e-39 sb_zero_count:0 sb_positive:<span:<offset:7 length:2 > delta:1 delta:0 > `,
		},
		{
			name:             "buckets limited by widening the zero bucket, then reset",
			observations:     []float64{0, 1, 1.1, 1.2, 1.4, 1.8, 2, 5, 5.1, 3, 4},
			factor:           1.2,
			maxBuckets:       4,
			maxZeroThreshold: 1.2,
			minResetDuration: 9 * time.Minute,
			want:             `sample_count:2 sample_sum:7 sb_schema:2 sb_zero_threshold:2.938735877055719e-39 sb_zero_count:0 sb_positive:<span:<offset:7 length:2 > delta:1 delta:0 > `,
		},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			his := NewHistogram(HistogramOpts{
				Name:                          "name",
				Help:                          "help",
				SparseBucketsFactor:           s.factor,
				SparseBucketsZeroThreshold:    s.zeroThreshold,
				SparseBucketsMaxNumber:        s.maxBuckets,
				SparseBucketsMinResetDuration: s.minResetDuration,
				SparseBucketsMaxZeroThreshold: s.maxZeroThreshold,
			})
			ts := time.Now().Add(30 * time.Second)
			now := func() time.Time {
				return ts
			}
			his.(*histogram).now = now
			for _, o := range s.observations {
				his.Observe(o)
				ts = ts.Add(time.Minute)
			}
			m := &dto.Metric{}
			if err := his.Write(m); err != nil {
				t.Fatal("unexpected error writing metric", err)
			}
			got := m.Histogram.String()
			if s.want != got {
				t.Errorf("want histogram %q, got %q", s.want, got)
			}
		})
	}
}

func TestSparseHistogramConcurrency(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping test in short mode.")
	}

	rand.Seed(42)

	it := func(n uint32) bool {
		mutations := int(n%1e4 + 1e4)
		concLevel := int(n%5 + 1)
		total := mutations * concLevel

		var start, end sync.WaitGroup
		start.Add(1)
		end.Add(concLevel)

		his := NewHistogram(HistogramOpts{
			Name:                          "test_sparse_histogram",
			Help:                          "This help is sparse.",
			SparseBucketsFactor:           1.05,
			SparseBucketsZeroThreshold:    0.0000001,
			SparseBucketsMaxNumber:        50,
			SparseBucketsMinResetDuration: time.Hour, // Comment out to test for totals below.
			SparseBucketsMaxZeroThreshold: 0.001,
		})

		ts := time.Now().Add(30 * time.Second).Unix()
		now := func() time.Time {
			return time.Unix(atomic.LoadInt64(&ts), 0)
		}
		his.(*histogram).now = now

		allVars := make([]float64, total)
		var sampleSum float64
		for i := 0; i < concLevel; i++ {
			vals := make([]float64, mutations)
			for j := 0; j < mutations; j++ {
				v := rand.NormFloat64()
				vals[j] = v
				allVars[i*mutations+j] = v
				sampleSum += v
			}

			go func(vals []float64) {
				start.Wait()
				for _, v := range vals {
					// An observation every 1 to 10 seconds.
					atomic.AddInt64(&ts, rand.Int63n(10)+1)
					his.Observe(v)
				}
				end.Done()
			}(vals)
		}
		sort.Float64s(allVars)
		start.Done()
		end.Wait()

		m := &dto.Metric{}
		his.Write(m)

		// Uncomment these tests for totals only if you have disabled histogram resets above.
		//
		// if got, want := int(*m.Histogram.SampleCount), total; got != want {
		// 	t.Errorf("got sample count %d, want %d", got, want)
		// }
		// if got, want := *m.Histogram.SampleSum, sampleSum; math.Abs((got-want)/want) > 0.001 {
		// 	t.Errorf("got sample sum %f, want %f", got, want)
		// }

		sumBuckets := int(m.Histogram.GetSbZeroCount())
		current := 0
		for _, delta := range m.Histogram.GetSbNegative().GetDelta() {
			current += int(delta)
			if current < 0 {
				t.Fatalf("negative bucket population negative: %d", current)
			}
			sumBuckets += current
		}
		current = 0
		for _, delta := range m.Histogram.GetSbPositive().GetDelta() {
			current += int(delta)
			if current < 0 {
				t.Fatalf("positive bucket population negative: %d", current)
			}
			sumBuckets += current
		}
		if got, want := sumBuckets, int(*m.Histogram.SampleCount); got != want {
			t.Errorf("got bucket population sum %d, want %d", got, want)
		}

		return true
	}

	if err := quick.Check(it, nil); err != nil {
		t.Error(err)
	}
}
