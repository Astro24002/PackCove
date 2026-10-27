package packcove

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

var durationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 300}

type histogram struct {
	count   uint64
	sum     float64
	buckets []uint64
}

type Metrics struct {
	mu               sync.Mutex
	artifactRequests map[string]map[string]uint64
	requestErrors    map[string]map[string]uint64
	upstreamRequests map[string]map[string]uint64
	bytesServed      map[string]map[string]uint64
	requestDuration  map[string]*histogram
	upstreamDuration map[string]*histogram
}

func NewMetrics() *Metrics {
	return &Metrics{
		artifactRequests: make(map[string]map[string]uint64),
		requestErrors:    make(map[string]map[string]uint64),
		upstreamRequests: make(map[string]map[string]uint64),
		bytesServed:      make(map[string]map[string]uint64),
		requestDuration:  make(map[string]*histogram),
		upstreamDuration: make(map[string]*histogram),
	}
}

func (m *Metrics) inc(group map[string]map[string]uint64, repository, result string) {
	if group[repository] == nil {
		group[repository] = make(map[string]uint64)
	}
	group[repository][result]++
}

func (m *Metrics) Artifact(repository, result string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inc(m.artifactRequests, repository, result)
}

func (m *Metrics) Error(repository, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inc(m.requestErrors, repository, reason)
}

func (m *Metrics) Upstream(repository, result string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inc(m.upstreamRequests, repository, result)
}

func (m *Metrics) Bytes(repository, source string, count int64) {
	if count <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.bytesServed[repository] == nil {
		m.bytesServed[repository] = make(map[string]uint64)
	}
	m.bytesServed[repository][source] += uint64(count)
}

func (m *Metrics) ObserveRequest(repository string, seconds float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	observe(m.requestDuration, repository, seconds)
}

func (m *Metrics) ObserveUpstream(repository string, seconds float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	observe(m.upstreamDuration, repository, seconds)
}

func observe(group map[string]*histogram, repository string, seconds float64) {
	h := group[repository]
	if h == nil {
		h = &histogram{buckets: make([]uint64, len(durationBuckets))}
		group[repository] = h
	}
	h.count++
	h.sum += seconds
	for i, upper := range durationBuckets {
		if seconds <= upper {
			h.buckets[i]++
		}
	}
}

type repositoryCounters struct {
	Requests      map[string]uint64 `json:"requests"`
	Errors        map[string]uint64 `json:"errors"`
	Upstream      map[string]uint64 `json:"upstream_requests"`
	Bytes         map[string]uint64 `json:"bytes_served"`
	CacheHitRatio *float64          `json:"cache_hit_ratio"`
}

func (m *Metrics) Snapshot(repositories []string) map[string]repositoryCounters {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make(map[string]repositoryCounters, len(repositories))
	for _, repository := range repositories {
		requests := cloneCounts(m.artifactRequests[repository])
		var hitRatio *float64
		total := requests["hit"] + requests["miss"] + requests["revalidated"]
		if total > 0 {
			ratio := float64(requests["hit"]) / float64(total)
			hitRatio = &ratio
		}
		result[repository] = repositoryCounters{
			Requests:      requests,
			Errors:        cloneCounts(m.requestErrors[repository]),
			Upstream:      cloneCounts(m.upstreamRequests[repository]),
			Bytes:         cloneCounts(m.bytesServed[repository]),
			CacheHitRatio: hitRatio,
		}
	}
	return result
}

func cloneCounts(src map[string]uint64) map[string]uint64 {
	result := make(map[string]uint64, len(src))
	for key, value := range src {
		result[key] = value
	}
	return result
}

func (m *Metrics) WritePrometheus(cacheBytes, storageFreeBytes int64, repositories []string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out strings.Builder
	writeHeader := func(name, help, kind string) {
		fmt.Fprintf(&out, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
	}
	writeHeader("packcove_artifact_requests_total", "Successful package file GET requests by cache result.", "counter")
	for _, repository := range repositories {
		for _, result := range []string{"hit", "miss", "revalidated"} {
			fmt.Fprintf(&out, "packcove_artifact_requests_total{repository=%q,result=%q} %d\n", repository, result, m.artifactRequests[repository][result])
		}
	}
	writeHeader("packcove_request_errors_total", "Proxy request errors by finite reason.", "counter")
	for _, repository := range repositories {
		keys := sortedKeys(m.requestErrors[repository])
		for _, reason := range keys {
			fmt.Fprintf(&out, "packcove_request_errors_total{repository=%q,reason=%q} %d\n", repository, reason, m.requestErrors[repository][reason])
		}
	}
	writeHeader("packcove_upstream_requests_total", "Requests sent to configured upstreams by outcome.", "counter")
	for _, repository := range repositories {
		for _, result := range []string{"success", "not_found", "error", "revalidated"} {
			fmt.Fprintf(&out, "packcove_upstream_requests_total{repository=%q,result=%q} %d\n", repository, result, m.upstreamRequests[repository][result])
		}
	}
	writeHeader("packcove_bytes_served_total", "Bytes written to clients by cache or upstream source.", "counter")
	for _, repository := range repositories {
		for _, source := range []string{"cache", "upstream"} {
			fmt.Fprintf(&out, "packcove_bytes_served_total{repository=%q,source=%q} %d\n", repository, source, m.bytesServed[repository][source])
		}
	}
	writeHeader("packcove_cache_bytes", "Bytes referenced by cache entries, deduplicated by content hash.", "gauge")
	fmt.Fprintf(&out, "packcove_cache_bytes %d\n", cacheBytes)
	writeHeader("packcove_storage_free_bytes", "Available bytes on the filesystem that stores the cache.", "gauge")
	fmt.Fprintf(&out, "packcove_storage_free_bytes %d\n", storageFreeBytes)
	writeHistogram := func(name, help string, group map[string]*histogram) {
		writeHeader(name, help, "histogram")
		for _, repository := range repositories {
			h := group[repository]
			if h == nil {
				continue
			}
			for i, upper := range durationBuckets {
				fmt.Fprintf(&out, "%s_bucket{repository=%q,le=%q} %d\n", name, repository, fmt.Sprintf("%g", upper), h.buckets[i])
			}
			fmt.Fprintf(&out, "%s_bucket{repository=%q,le=\"+Inf\"} %d\n", name, repository, h.count)
			fmt.Fprintf(&out, "%s_sum{repository=%q} %g\n%s_count{repository=%q} %d\n", name, repository, h.sum, name, repository, h.count)
		}
	}
	writeHistogram("packcove_request_duration_seconds", "Proxy request duration in seconds.", m.requestDuration)
	writeHistogram("packcove_upstream_duration_seconds", "Upstream request duration in seconds.", m.upstreamDuration)
	return out.String()
}

func sortedKeys(values map[string]uint64) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
