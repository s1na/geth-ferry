package progress

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTrackerEmitsLines(t *testing.T) {
	var buf safeBuf
	tr := (&Tracker{
		Label:    "test",
		Out:      &buf,
		Interval: 25 * time.Millisecond,
	}).Start()

	w := tr.Writer()
	for i := 0; i < 4; i++ {
		w.Write(make([]byte, 1024))
		time.Sleep(40 * time.Millisecond)
	}
	tr.Stop()

	got := buf.String()
	if !strings.Contains(got, "[test]") {
		t.Errorf("missing label in output: %q", got)
	}
	if !strings.Contains(got, "done") {
		t.Errorf("missing final summary in output: %q", got)
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		0:                "0 B",
		512:              "512 B",
		1024:             "1.00 KiB",
		1024 * 1024:      "1.00 MiB",
		2 * 1024 * 1024:  "2.00 MiB",
		1 << 30:          "1.00 GiB",
		3 * (1 << 40):    "3.00 TiB",
		1024*1024 + 512:  "1.00 MiB", // truncates fine; just rendering shape
		1024*1024*3 + 12: "3.00 MiB",
	}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

// safeBuf is a tiny goroutine-safe bytes.Buffer for tests.
type safeBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
