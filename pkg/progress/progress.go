// Package progress provides a tiny throttled byte-rate reporter for upload
// and download streams. It is deliberately minimal: a label, a counter, and
// a periodic stderr line.
package progress

import (
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"time"
)

// Tracker counts bytes added via Writer and periodically reports progress
// on Out. Use Start to launch the ticker, Stop to halt it and emit a final
// summary line.
//
// When Total is non-zero, the periodic line additionally renders a
// percentage and an ETA based on the running average rate. Total should
// be the expected end value of bytes that will pass through this
// Tracker — for ferry that's the uncompressed source size of the part.
type Tracker struct {
	Label    string
	Out      io.Writer     // defaults to os.Stderr
	Interval time.Duration // defaults to 2s
	Total    int64         // optional — expected end value; enables % + ETA

	started time.Time
	n       atomic.Int64
	quit    chan struct{}
	done    chan struct{}
}

// Start kicks off the periodic reporter. Returns the Tracker for chaining.
func (t *Tracker) Start() *Tracker {
	if t.Out == nil {
		t.Out = os.Stderr
	}
	if t.Interval == 0 {
		t.Interval = 2 * time.Second
	}
	t.started = time.Now()
	t.quit = make(chan struct{})
	t.done = make(chan struct{})
	go t.run()
	return t
}

// Stop halts the ticker and emits a final summary line.
func (t *Tracker) Stop() {
	if t.quit == nil {
		return
	}
	close(t.quit)
	<-t.done
	t.report("done")
}

// Writer returns an io.Writer that adds every successful write byte count
// to this tracker. Compose via io.MultiWriter to attach to an existing
// pipeline without consuming bytes.
func (t *Tracker) Writer() io.Writer {
	return writerFunc(func(p []byte) (int, error) {
		t.n.Add(int64(len(p)))
		return len(p), nil
	})
}

func (t *Tracker) run() {
	defer close(t.done)
	ticker := time.NewTicker(t.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-t.quit:
			return
		case <-ticker.C:
			t.report("")
		}
	}
}

func (t *Tracker) report(suffix string) {
	n := t.n.Load()
	elapsed := time.Since(t.started)
	rate := float64(0)
	if elapsed > 0 {
		rate = float64(n) / elapsed.Seconds()
	}
	tag := ""
	if suffix != "" {
		tag = " — " + suffix
	}
	// Optional progress-towards-total block: "/ 2.18 TiB (5.7%) ETA 3h41m".
	var progressTag string
	if t.Total > 0 {
		pct := 100 * float64(n) / float64(t.Total)
		if pct > 100 {
			pct = 100
		}
		eta := "—"
		switch {
		case n >= t.Total:
			eta = "0s"
		case rate > 0:
			// Compute as float seconds, then to Duration in one cast so the
			// nanosecond-scale conversion uses the full float magnitude
			// instead of overflowing through int64(seconds) × time.Second.
			remaining := time.Duration(float64(t.Total-n) / rate * float64(time.Second))
			eta = remaining.Round(time.Second).String()
		}
		progressTag = fmt.Sprintf(" / %s (%.1f%%) ETA %s", HumanBytes(t.Total), pct, eta)
	}
	fmt.Fprintf(t.Out, "[%s] %s%s in %s (%s/s)%s\n",
		t.Label, HumanBytes(n), progressTag, elapsed.Round(time.Second), HumanBytes(int64(rate)), tag)
}

type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// HumanBytes renders a byte count in the largest unit at which it is ≥ 1
// (B, KiB, MiB, GiB, TiB), with two decimal places. Shared with the CLI's
// `list` and `upload --dry-run` formatters.
func HumanBytes(n int64) string {
	const (
		KiB = 1024
		MiB = KiB * 1024
		GiB = MiB * 1024
		TiB = GiB * 1024
	)
	switch {
	case n >= TiB:
		return fmt.Sprintf("%.2f TiB", float64(n)/float64(TiB))
	case n >= GiB:
		return fmt.Sprintf("%.2f GiB", float64(n)/float64(GiB))
	case n >= MiB:
		return fmt.Sprintf("%.2f MiB", float64(n)/float64(MiB))
	case n >= KiB:
		return fmt.Sprintf("%.2f KiB", float64(n)/float64(KiB))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
