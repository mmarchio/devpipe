package service

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"syscall"
	"time"

	"overseer/internal/health"
)

// Disk levels.
const (
	DiskOK       = "OK"
	DiskWarning  = "WARNING"
	DiskCritical = "CRITICAL"
)

// DiskUsage is one monitored location.
type DiskUsage struct {
	Path        string  `json:"path"`
	UsedPercent float64 `json:"used_percent"`
	TotalBytes  uint64  `json:"total_bytes"`
	FreeBytes   uint64  `json:"free_bytes"`
	Level       string  `json:"level"`
	Error       string  `json:"error,omitempty"`
}

// StatFS returns a filesystem's total and available bytes.
type StatFS func(path string) (total, free uint64, err error)

// SysStatFS is the real filesystem probe.
func SysStatFS(path string) (uint64, uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	return st.Blocks * uint64(st.Bsize), st.Bavail * uint64(st.Bsize), nil
}

// DiskMonitor watches the filesystems the overseer writes to — project
// working directories / allowed roots, the backup directory, the
// temporary directory (WBS 18.97, decision U7): at the warning threshold
// it raises DISK_THRESHOLD; at the critical threshold it holds new worker
// dispatches, validation runs and checkpoints (executions WAIT) and lets
// them resume by themselves once usage falls below it.
type DiskMonitor struct {
	paths    func(ctx context.Context) []string
	gate     *WorkGate
	log      *slog.Logger
	warn     float64
	critical float64
	interval time.Duration

	mu    sync.Mutex
	stat  StatFS
	last  []DiskUsage
	level map[string]string
	stop  chan struct{}
	done  chan struct{}
}

// NewDiskMonitor builds the monitor.
func NewDiskMonitor(paths func(ctx context.Context) []string, gate *WorkGate, warn, critical float64, interval time.Duration, log *slog.Logger) *DiskMonitor {
	if log == nil {
		log = slog.Default()
	}
	return &DiskMonitor{paths: paths, gate: gate, log: log, warn: warn, critical: critical, interval: interval, stat: SysStatFS,
		level: map[string]string{}}
}

// SetStatFS replaces the filesystem probe (tests).
func (m *DiskMonitor) SetStatFS(f StatFS) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stat = f
}

// Check measures every location, logs level changes, and holds or
// releases side effects.
func (m *DiskMonitor) Check(ctx context.Context) []DiskUsage {
	seen := map[string]bool{}
	var paths []string
	for _, p := range m.paths(ctx) {
		if p != "" && !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	m.mu.Lock()
	stat := m.stat
	m.mu.Unlock()
	out := make([]DiskUsage, 0, len(paths))
	var critical []string
	for _, p := range paths {
		u := DiskUsage{Path: p, Level: DiskOK}
		total, free, err := stat(p)
		switch {
		case err != nil:
			u.Error = err.Error()
		case total > 0:
			u.TotalBytes, u.FreeBytes = total, free
			u.UsedPercent = float64(total-free) * 100 / float64(total)
			switch {
			case u.UsedPercent >= m.critical:
				u.Level = DiskCritical
				critical = append(critical, fmt.Sprintf("%s %.1f%%", p, u.UsedPercent))
			case u.UsedPercent >= m.warn:
				u.Level = DiskWarning
			}
		}
		out = append(out, u)
	}
	m.mu.Lock()
	for _, u := range out {
		if prev := m.level[u.Path]; prev != u.Level && (prev != "" || u.Level != DiskOK) {
			lvl := slog.LevelWarn
			if u.Level == DiskOK {
				lvl = slog.LevelInfo
			}
			m.log.Log(ctx, lvl, "disk usage level changed", "path", u.Path, "level", u.Level, "used_percent", u.UsedPercent)
		}
		m.level[u.Path] = u.Level
	}
	m.last = out
	m.mu.Unlock()
	if len(critical) > 0 {
		m.gate.BlockSideEffects(GateDisk, fmt.Sprintf("disk usage at or above %.0f%%: %v; free space to resume", m.critical, critical))
	} else {
		m.gate.Clear(GateDisk)
	}
	return out
}

// Last returns the latest measurement.
func (m *DiskMonitor) Last() []DiskUsage {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]DiskUsage(nil), m.last...)
}

// Contribute feeds DISK_THRESHOLD (warning or critical locations).
func (m *DiskMonitor) Contribute(_ context.Context, f *health.AlertFacts) {
	for _, u := range m.Last() {
		if u.Level != DiskOK {
			f.DiskOverThreshold = append(f.DiskOverThreshold, fmt.Sprintf("%s (%.1f%%)", u.Path, u.UsedPercent))
		}
	}
}

// HealthCheck reports the "disk" subsystem from a fresh measurement.
func (m *DiskMonitor) HealthCheck(ctx context.Context) health.Subsystem {
	s := health.Subsystem{Name: "disk", Status: health.Healthy}
	for _, u := range m.Check(ctx) {
		switch u.Level {
		case DiskCritical:
			s.Status, s.Detail = health.Unavailable, fmt.Sprintf("%s at %.1f%% (new work held)", u.Path, u.UsedPercent)
			return s
		case DiskWarning:
			s.Status, s.Detail = health.Degraded, fmt.Sprintf("%s at %.1f%%", u.Path, u.UsedPercent)
		}
	}
	return s
}

// Start measures every interval until Stop.
func (m *DiskMonitor) Start() {
	m.mu.Lock()
	if m.stop != nil {
		m.mu.Unlock()
		return
	}
	m.stop, m.done = make(chan struct{}), make(chan struct{})
	stop, done := m.stop, m.done
	m.mu.Unlock()
	go func() {
		defer close(done)
		t := time.NewTicker(m.interval)
		defer t.Stop()
		for {
			ctx, cancel := context.WithTimeout(context.Background(), m.interval)
			func() {
				defer containPanic(m.log, "disk check")
				m.Check(ctx)
			}()
			cancel()
			select {
			case <-stop:
				return
			case <-t.C:
			}
		}
	}()
}

// Stop ends the periodic measurement.
func (m *DiskMonitor) Stop() {
	m.mu.Lock()
	stop, done := m.stop, m.done
	m.stop, m.done = nil, nil
	m.mu.Unlock()
	if stop != nil {
		close(stop)
		<-done
	}
}
