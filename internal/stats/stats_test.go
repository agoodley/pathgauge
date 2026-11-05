package stats

import (
	"math"
	"testing"
)

func near(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

func TestSummarize(t *testing.T) {
	xs := []float64{5, 1, 4, 2, 3}
	s := Summarize(xs)
	if s.N != 5 || s.Min != 1 || s.Max != 5 || s.Mean != 3 || s.Median != 3 {
		t.Errorf("got %+v", s)
	}
	if xs[0] != 5 {
		t.Error("Summarize modified its input")
	}
	if !near(s.StdDev, math.Sqrt(2), 1e-9) {
		t.Errorf("stddev %v", s.StdDev)
	}
	if Summarize(nil).N != 0 {
		t.Error("empty input")
	}
}

func TestPercentile(t *testing.T) {
	s := []float64{10, 20, 30, 40}
	cases := map[float64]float64{0: 10, 50: 25, 100: 40, 95: 38.5}
	for p, want := range cases {
		if got := Percentile(s, p); !near(got, want, 1e-9) {
			t.Errorf("P%v = %v, want %v", p, got, want)
		}
	}
	if Percentile([]float64{7}, 99) != 7 {
		t.Error("single sample")
	}
}

func TestJitterConstantDelay(t *testing.T) {
	var j Jitter
	for i := 0; i < 100; i++ {
		j.Add(1000 + 50) // constant transit (offset is irrelevant)
	}
	if j.Value() != 0 {
		t.Errorf("constant delay jitter = %v", j.Value())
	}
}

func TestJitterAlternating(t *testing.T) {
	var j Jitter
	for i := 0; i < 2000; i++ {
		j.Add(float64(10 * (i % 2))) // alternating 0,10 → |D| = 10
	}
	if !near(j.Value(), 10, 0.01) {
		t.Errorf("jitter = %v, want ~10", j.Value())
	}
	if !near(IPDV([]float64{0, 10, 0, 10}), 10, 1e-9) {
		t.Error("IPDV")
	}
}

func TestSeqTracker(t *testing.T) {
	var tr SeqTracker
	for _, s := range []uint64{0, 1, 2, 5, 4, 4, 9} { // lost 3, 6, 7, 8; 4 reordered; 4 duplicated
		tr.Add(s)
	}
	ls := tr.Loss(10)
	if ls.Received != 6 || ls.Lost != 4 || ls.Duplicates != 1 || ls.Reordered != 1 {
		t.Errorf("got %+v", ls)
	}
	if ls.Bursts != 2 || ls.MaxBurst != 3 {
		t.Errorf("bursts %d max %d", ls.Bursts, ls.MaxBurst)
	}
	if !near(ls.LossPct, 40, 1e-9) {
		t.Errorf("loss %v", ls.LossPct)
	}
	// Trailing loss counts when the sender's count is known.
	var tail SeqTracker
	tail.Add(0)
	if l := tail.Loss(3); l.Lost != 2 || l.MaxBurst != 2 {
		t.Errorf("tail loss %+v", l)
	}
	// Large sequence numbers grow the bitset.
	var big SeqTracker
	big.Add(100000)
	if big.Received != 1 {
		t.Error("large seq")
	}
}

func TestMOS(t *testing.T) {
	good, r := MOS(20, 2, 0)
	if good < 4.3 || r < 90 {
		t.Errorf("clean path MOS %v R %v", good, r)
	}
	bad, _ := MOS(300, 40, 5)
	if bad > 3.0 { // R ≈ 53.7 → MOS ≈ 2.77 ("poor")
		t.Errorf("bad path MOS %v", bad)
	}
	if m, _ := MOS(10000, 1000, 100); m != 1 {
		t.Errorf("floor %v", m)
	}
}

func TestGrades(t *testing.T) {
	cases := map[float64]string{0: "A+", 4.9: "A+", 5: "A", 29: "A", 45: "B", 150: "C", 399: "D", 400: "F", 2000: "F"}
	for ms, want := range cases {
		if got := Grade(ms); got != want {
			t.Errorf("Grade(%v) = %s, want %s", ms, got, want)
		}
	}
	if WorstGrade("A", "C") != "C" || WorstGrade("F", "A+") != "F" || WorstGrade("", "B") != "B" || WorstGrade("B", "") != "B" {
		t.Error("WorstGrade")
	}
}

func TestRPMAndMathis(t *testing.T) {
	if RPM(60) != 1000 || RPM(0) != 0 {
		t.Error("RPM")
	}
	// 1460 B MSS, 50 ms, 0.01% loss → ~28.5 Mbit/s
	if got := Mathis(1460, 0.05, 0.0001); !near(got/1e6, 28.5, 0.1) {
		t.Errorf("Mathis = %v Mbit/s", got/1e6)
	}
	if !math.IsInf(Mathis(1460, 0.05, 0), 1) {
		t.Error("no loss should be unbounded")
	}
	if Round(1.23456, 2) != 1.23 || Round(math.NaN(), 2) != 0 {
		t.Error("Round")
	}
}
