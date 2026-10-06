package main

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ResourceResult is what the sampler saw of the bridge process and its
// spill directory over the run.
type ResourceResult struct {
	Process       string  `json:"process"`
	PID           int     `json:"pid,omitempty"`
	Samples       int     `json:"samples"`
	PeakRSSBytes  int64   `json:"peak_rss_bytes"`
	MeanCPUPct    float64 `json:"mean_cpu_pct"`
	PeakCPUPct    float64 `json:"peak_cpu_pct"`
	SpillDir      string  `json:"spill_dir,omitempty"`
	PeakSpillByte int64   `json:"peak_spill_bytes"`
	EndSpillBytes int64   `json:"end_spill_bytes"`
	Note          string  `json:"note,omitempty"`
}

// clkTck is USER_HZ, 100 on every Linux the bench runs on.
const clkTck = 100

// sampler reads /proc/<pid>/{status,stat} of the named process (and the
// spill directory's size) once a second. Without /proc (macOS, another
// host) it records why and samples nothing — the run goes on.
type sampler struct {
	mu     sync.Mutex
	res    ResourceResult
	cpuSum float64
	cancel context.CancelFunc
	done   chan struct{}
}

func startSampler(ctx context.Context, proc, spillDir string, every time.Duration) *sampler {
	ctx, cancel := context.WithCancel(ctx)
	s := &sampler{res: ResourceResult{Process: proc, SpillDir: spillDir}, cancel: cancel, done: make(chan struct{})}
	go s.loop(ctx, every)
	return s
}

func (s *sampler) loop(ctx context.Context, every time.Duration) {
	defer close(s.done)
	var lastTicks int64 = -1
	var lastAt time.Time
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		now := time.Now()
		s.sampleSpill()
		if pid := s.pid(); pid > 0 {
			rss, rssOK := readRSS(pid)
			ticks, ticksOK := readCPUTicks(pid)
			s.mu.Lock()
			if rssOK {
				if rss > s.res.PeakRSSBytes {
					s.res.PeakRSSBytes = rss
				}
			}
			if ticksOK {
				if lastTicks >= 0 {
					pct := float64(ticks-lastTicks) / clkTck / now.Sub(lastAt).Seconds() * 100
					s.cpuSum += pct
					s.res.Samples++
					if pct > s.res.PeakCPUPct {
						s.res.PeakCPUPct = pct
					}
				}
				lastTicks, lastAt = ticks, now
			}
			s.mu.Unlock()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// pid finds the process once (by /proc/<pid>/comm) and keeps it.
func (s *sampler) pid() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.res.PID > 0 || s.res.Process == "" {
		return s.res.PID
	}
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		s.res.Note = "no /proc on this host: process sampling off"
		return 0
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		s.res.Note = "read /proc: " + err.Error()
		return 0
	}
	want := s.res.Process
	if len(want) > 15 { // comm is truncated to 15 bytes
		want = want[:15]
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/proc", e.Name(), "comm"))
		if err == nil && strings.TrimSpace(string(b)) == want {
			s.res.PID = pid
			s.res.Note = ""
			return pid
		}
	}
	s.res.Note = "process " + s.res.Process + " not found on this host"
	return 0
}

func (s *sampler) sampleSpill() {
	if s.res.SpillDir == "" {
		return
	}
	n := dirSize(s.res.SpillDir)
	s.mu.Lock()
	if n > s.res.PeakSpillByte {
		s.res.PeakSpillByte = n
	}
	s.res.EndSpillBytes = n
	s.mu.Unlock()
}

// stop ends sampling and returns the result.
func (s *sampler) stop() *ResourceResult {
	s.cancel()
	<-s.done
	s.sampleSpill()
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.res
	if r.Samples > 0 {
		r.MeanCPUPct = s.cpuSum / float64(r.Samples)
	}
	return &r
}

func readRSS(pid int) (int64, bool) {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			f := strings.Fields(line)
			if len(f) >= 2 {
				kb, err := strconv.ParseInt(f[1], 10, 64)
				return kb << 10, err == nil
			}
		}
	}
	return 0, false
}

// readCPUTicks is utime+stime from /proc/<pid>/stat (fields 14 and 15; the
// comm field may contain spaces, so count from the closing parenthesis).
func readCPUTicks(pid int) (int64, bool) {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, false
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, false
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 13 {
		return 0, false
	}
	u, err1 := strconv.ParseInt(f[11], 10, 64)
	st, err2 := strconv.ParseInt(f[12], 10, 64)
	return u + st, err1 == nil && err2 == nil
}

func dirSize(root string) int64 {
	var n int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}
