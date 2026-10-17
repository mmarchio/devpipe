package readmodel

import (
	"fmt"
	"math"
)

// Progress is WBS progress: DONE work units over all units of the current
// plan (WBS 17.11). Only DONE contributes; the percentage is recomputed
// from counts every time, so a plan amendment that grows the denominator
// lowers it immediately (17.12).
type Progress struct {
	Done    int     `json:"done"`
	Total   int     `json:"total"`
	Percent float64 `json:"percent"`
	Label   string  `json:"label"`
}

// NewProgress builds Progress from counts (Percent rounded to 1 decimal).
func NewProgress(done, total int) Progress {
	if done < 0 {
		done = 0
	}
	if total < 0 {
		total = 0
	}
	if done > total {
		done = total
	}
	p := Progress{Done: done, Total: total}
	if total > 0 {
		p.Percent = math.Round(float64(done)*1000/float64(total)) / 10
	}
	p.Label = fmt.Sprintf("%d / %d tasks complete", done, total)
	return p
}

// ContextUsage is the worker's context consumption (WBS 17.13 / 17.14). It
// is a separate metric: there is deliberately no conversion to or from
// Progress.
type ContextUsage struct {
	Known  bool `json:"known"`
	Used   int  `json:"used"`
	Window int  `json:"window"`
	// Percent of Window used (1 decimal).
	Percent float64 `json:"percent"`
	// ResetThreshold is the configured reset threshold in percent, taken
	// from the execution configuration snapshot.
	ResetThreshold float64 `json:"reset_threshold"`
}

// NewContextUsage builds a usage value; threshold is a fraction (0.8) or a
// percentage (80) and is normalized to percent.
func NewContextUsage(used, window int, threshold float64) ContextUsage {
	c := ContextUsage{Known: window > 0 && used >= 0, Used: used, Window: window}
	if c.Known {
		c.Percent = math.Round(float64(used)*1000/float64(window)) / 10
	}
	if threshold > 0 && threshold <= 1 {
		threshold *= 100
	}
	c.ResetThreshold = math.Round(threshold*10) / 10
	return c
}
