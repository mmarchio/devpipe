package precompute

import (
	"sort"
	"time"
)

// UsageRecord joins a built candidate with the use of what it produced.
type UsageRecord struct {
	Depth    int
	Priority int
	Result   CandidateResult
	// Used: the precomputed artifact was later used by an actual
	// preparation (cache entry hit / template resolved / fragment used).
	Used bool
}

// LatencySample is one measured transition (WBS 11.53): worker result
// received → next worker prompt ready.
type LatencySample struct {
	Source   string
	Duration time.Duration
}

// Latency sources.
const (
	LatencyOnDemand    = "ON_DEMAND"
	LatencyCacheHit    = "CACHE_HIT"
	LatencyPrecomputed = "PRECOMPUTED"
	// LatencyPlaceholder: a placeholder template resolved (Decision 5).
	LatencyPlaceholder = "PRECOMPUTED_PLACEHOLDER"
)

// Ratio is a used/created utilization figure.
type Ratio struct {
	Created     int     `json:"created"`
	Used        int     `json:"used"`
	Utilization float64 `json:"utilization"`
	Waste       float64 `json:"waste"`
}

func ratio(created, used int) Ratio {
	r := Ratio{Created: created, Used: used}
	if created > 0 {
		r.Utilization = float64(used) / float64(created)
		r.Waste = float64(created-used) / float64(created)
	}
	return r
}

// LatencyStats summarises measured transition latency per source.
type LatencyStats struct {
	Source  string        `json:"source"`
	Count   int           `json:"count"`
	Average time.Duration `json:"average"`
	Max     time.Duration `json:"max"`
}

// Metrics is the precompute effectiveness summary (WBS 11.51/11.52/11.53/
// 11.77).
type Metrics struct {
	Jobs        int
	Candidates  int
	FullPrompts int
	Fragments   int
	Templates   int
	Hits        int
	Failed      int
	Skipped     int

	Overall    Ratio
	ByDepth    map[int]Ratio
	ByPriority map[int]Ratio

	Latency []LatencyStats
}

// Aggregate computes metrics from usage records and latency samples. Only
// produced artifacts (built prompts, templates, fragments) enter the
// utilization ratios; hits and skips are counted separately. Latencies are
// measured values only (T11.53.3) — nothing is estimated here.
func Aggregate(jobs int, usage []UsageRecord, lat []LatencySample) Metrics {
	m := Metrics{Jobs: jobs, Candidates: len(usage), ByDepth: map[int]Ratio{}, ByPriority: map[int]Ratio{}}
	type cu struct{ c, u int }
	depth, prio := map[int]*cu{}, map[int]*cu{}
	var total cu
	for _, r := range usage {
		produced := false
		switch r.Result {
		case ResultBuilt, ResultCanonical:
			m.FullPrompts++
			produced = true
		case ResultFragment:
			m.Fragments++
			produced = true
		case ResultTemplate:
			m.Templates++
			produced = true
		case ResultHit:
			m.Hits++
		case ResultFailed:
			m.Failed++
		case ResultSkipped:
			m.Skipped++
		}
		if !produced {
			continue
		}
		for _, mp := range []struct {
			m map[int]*cu
			k int
		}{{depth, r.Depth}, {prio, r.Priority}} {
			if mp.m[mp.k] == nil {
				mp.m[mp.k] = &cu{}
			}
			mp.m[mp.k].c++
			if r.Used {
				mp.m[mp.k].u++
			}
		}
		total.c++
		if r.Used {
			total.u++
		}
	}
	m.Overall = ratio(total.c, total.u)
	for k, v := range depth {
		m.ByDepth[k] = ratio(v.c, v.u)
	}
	for k, v := range prio {
		m.ByPriority[k] = ratio(v.c, v.u)
	}
	by := map[string]*LatencyStats{}
	for _, s := range lat {
		st := by[s.Source]
		if st == nil {
			st = &LatencyStats{Source: s.Source}
			by[s.Source] = st
		}
		st.Count++
		st.Average += s.Duration
		if s.Duration > st.Max {
			st.Max = s.Duration
		}
	}
	for _, st := range by {
		st.Average /= time.Duration(st.Count)
		m.Latency = append(m.Latency, *st)
	}
	sort.Slice(m.Latency, func(i, j int) bool { return m.Latency[i].Source < m.Latency[j].Source })
	return m
}
