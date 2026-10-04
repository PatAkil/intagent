//go:build unix

package server

import (
	"math/rand/v2"
	"syscall"
	"testing"
	"time"
)

func cpuTime() time.Duration {
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// BenchmarkStreamDelivery measures the server's CPU per activity delivered
// to a stream, with 300 streams on the busy repository and activities paced
// at 1000 a second, as in a burst. It counts the whole process: the hooks,
// the hand-over and the streams' handlers.
//
//	go test ./internal/server -run '^$' -bench StreamDelivery -benchtime 2000x
func BenchmarkStreamDelivery(b *testing.B) {
	const streams, perSecond = 300, 1000
	f := newStreamLoad(b, 30, 10, 20)
	writers, stop := f.openStreams(b, streams)
	defer stop()
	r := rand.New(rand.NewPCG(1, 1))
	b.ResetTimer()
	cpu0, start := cpuTime(), time.Now()
	for i := range b.N {
		if wait := time.Until(start.Add(time.Duration(i) * time.Second / perSecond)); wait > 0 {
			time.Sleep(wait)
		}
		f.hook(r, 1)
	}
	time.Sleep(200 * time.Millisecond) // let the streams write what they hold
	cpu := cpuTime() - cpu0
	b.StopTimer()
	var bytes int64
	for _, w := range writers {
		bytes += w.bytes.Load()
	}
	b.ReportMetric(float64(cpu.Nanoseconds())/float64(b.N*streams), "cpu-ns/delivery")
	b.ReportMetric(float64(bytes)/float64(b.N*streams), "B/delivery")
}
