package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// =============================================================================
// Jaeger Expvar Extension 시뮬레이션
// =============================================================================
//
// Jaeger는 Go의 expvar 패키지를 확장하여 런타임 메트릭스를 HTTP 엔드포인트로 노출한다.
// OTel Collector 기반 Jaeger에서는 extension으로 구현된다.
//
// 핵심 개념:
//   - expvar: Go 표준 라이브러리의 변수 공개 메커니즘
//   - /debug/vars 엔드포인트: JSON 형식 메트릭스 노출
//   - 런타임 메트릭스: goroutine, memstats, GC 정보
//   - 커스텀 메트릭스: 카운터, 게이지, 히스토그램
//
// 실제 코드 참조:
//   - cmd/jaeger/internal/extension/expvar/: expvar 확장
// =============================================================================

// --- Expvar 타입 시뮬레이션 ---

type Var interface {
	String() string
}

type Int struct {
	val int64
}

func (v *Int) Add(delta int64) { atomic.AddInt64(&v.val, delta) }
func (v *Int) Set(val int64)   { atomic.StoreInt64(&v.val, val) }
func (v *Int) Value() int64    { return atomic.LoadInt64(&v.val) }
func (v *Int) String() string  { return fmt.Sprintf("%d", v.Value()) }

type Float struct {
	mu  sync.Mutex
	val float64
}

func (v *Float) Set(val float64) { v.mu.Lock(); v.val = val; v.mu.Unlock() }
func (v *Float) Value() float64  { v.mu.Lock(); defer v.mu.Unlock(); return v.val }
func (v *Float) String() string  { return fmt.Sprintf("%.4f", v.Value()) }

type StringVar struct {
	mu  sync.Mutex
	val string
}

func (v *StringVar) Set(val string) { v.mu.Lock(); v.val = val; v.mu.Unlock() }
func (v *StringVar) String() string { v.mu.Lock(); defer v.mu.Unlock(); return fmt.Sprintf("%q", v.val) }

type Map struct {
	mu   sync.RWMutex
	vars map[string]Var
	keys []string // 순서 유지
}

func NewMap() *Map {
	return &Map{vars: make(map[string]Var)}
}

func (m *Map) Set(key string, val Var) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.vars[key]; !ok {
		m.keys = append(m.keys, key)
	}
	m.vars[key] = val
}

func (m *Map) Get(key string) Var {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.vars[key]
}

func (m *Map) AddInt(key string, delta int64) {
	m.mu.Lock()
	v, ok := m.vars[key]
	if !ok {
		iv := &Int{}
		m.vars[key] = iv
		m.keys = append(m.keys, key)
		v = iv
	}
	m.mu.Unlock()
	if iv, ok := v.(*Int); ok {
		iv.Add(delta)
	}
}

func (m *Map) String() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	parts := make([]string, 0, len(m.keys))
	for _, key := range m.keys {
		parts = append(parts, fmt.Sprintf("%q: %s", key, m.vars[key].String()))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// --- Expvar Registry ---

type Registry struct {
	mu   sync.RWMutex
	vars map[string]Var
	keys []string
}

func NewRegistry() *Registry {
	return &Registry{vars: make(map[string]Var)}
}

func (r *Registry) Publish(name string, v Var) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.vars[name] = v
	r.keys = append(r.keys, name)
}

// ServeJSON은 /debug/vars 엔드포인트를 시뮬레이션한다.
func (r *Registry) ServeJSON() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	parts := make([]string, 0, len(r.keys))
	for _, key := range r.keys {
		parts = append(parts, fmt.Sprintf("  %q: %s", key, r.vars[key].String()))
	}
	return "{\n" + strings.Join(parts, ",\n") + "\n}"
}

// --- 런타임 메트릭스 수집 ---

type RuntimeMetrics struct {
	Goroutines   int
	HeapAlloc    uint64
	HeapSys      uint64
	HeapObjects  uint64
	GCRuns       uint32
	GCPauseTotal time.Duration
	NumCPU       int
}

func collectRuntimeMetrics() RuntimeMetrics {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return RuntimeMetrics{
		Goroutines:   runtime.NumGoroutine(),
		HeapAlloc:    m.HeapAlloc,
		HeapSys:      m.HeapSys,
		HeapObjects:  m.HeapObjects,
		GCRuns:       m.NumGC,
		GCPauseTotal: time.Duration(m.PauseTotalNs),
		NumCPU:       runtime.NumCPU(),
	}
}

func formatBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

func main() {
	fmt.Println("=== Jaeger Expvar Extension 시뮬레이션 ===")
	fmt.Println()

	registry := NewRegistry()
	r := rand.New(rand.NewSource(time.Now().UnixNano()))

	// --- 런타임 메트릭스 등록 ---
	fmt.Println("[1] 런타임 메트릭스 등록")
	fmt.Println(strings.Repeat("-", 60))

	goroutines := &Int{}
	heapAlloc := &Int{}
	heapSys := &Int{}
	gcRuns := &Int{}

	registry.Publish("runtime.goroutines", goroutines)
	registry.Publish("runtime.heap_alloc_bytes", heapAlloc)
	registry.Publish("runtime.heap_sys_bytes", heapSys)
	registry.Publish("runtime.gc_runs", gcRuns)
	registry.Publish("runtime.num_cpu", &Int{val: int64(runtime.NumCPU())})

	rm := collectRuntimeMetrics()
	goroutines.Set(int64(rm.Goroutines))
	heapAlloc.Set(int64(rm.HeapAlloc))
	heapSys.Set(int64(rm.HeapSys))
	gcRuns.Set(int64(rm.GCRuns))

	fmt.Printf("  Goroutines: %d\n", rm.Goroutines)
	fmt.Printf("  Heap Alloc: %s\n", formatBytes(rm.HeapAlloc))
	fmt.Printf("  Heap Sys:   %s\n", formatBytes(rm.HeapSys))
	fmt.Printf("  GC Runs:    %d\n", rm.GCRuns)
	fmt.Println()

	// --- Jaeger 커스텀 메트릭스 ---
	fmt.Println("[2] Jaeger 커스텀 메트릭스 등록")
	fmt.Println(strings.Repeat("-", 60))

	// Collector 메트릭스
	collectorMap := NewMap()
	registry.Publish("jaeger.collector", collectorMap)

	collectorMap.Set("spans_received", &Int{})
	collectorMap.Set("spans_dropped", &Int{})
	collectorMap.Set("batches_received", &Int{})
	collectorMap.Set("queue_length", &Int{})

	// Query 메트릭스
	queryMap := NewMap()
	registry.Publish("jaeger.query", queryMap)

	queryMap.Set("requests_total", &Int{})
	queryMap.Set("request_duration_avg_ms", &Float{})
	queryMap.Set("traces_found", &Int{})

	// Storage 메트릭스
	storageMap := NewMap()
	registry.Publish("jaeger.storage", storageMap)

	storageMap.Set("write_latency_avg_ms", &Float{})
	storageMap.Set("read_latency_avg_ms", &Float{})
	storageMap.Set("errors_total", &Int{})

	// 빌드 정보
	buildInfo := &StringVar{}
	buildInfo.Set("v1.55.0 (go1.22, linux/amd64)")
	registry.Publish("jaeger.build_info", buildInfo)

	fmt.Println("  + jaeger.collector (spans_received, spans_dropped, ...)")
	fmt.Println("  + jaeger.query (requests_total, request_duration_avg_ms, ...)")
	fmt.Println("  + jaeger.storage (write_latency_avg_ms, read_latency_avg_ms, ...)")
	fmt.Println("  + jaeger.build_info")
	fmt.Println()

	// --- 메트릭스 업데이트 시뮬레이션 ---
	fmt.Println("[3] 메트릭스 업데이트 시뮬레이션 (5라운드)")
	fmt.Println(strings.Repeat("-", 60))

	for round := 1; round <= 5; round++ {
		// Collector 활동
		batchSize := int64(10 + r.Intn(100))
		collectorMap.AddInt("spans_received", batchSize)
		collectorMap.AddInt("batches_received", 1)
		dropped := int64(0)
		if r.Intn(10) == 0 {
			dropped = int64(r.Intn(5))
			collectorMap.AddInt("spans_dropped", dropped)
		}
		if qv, ok := collectorMap.Get("queue_length").(*Int); ok {
			qv.Set(int64(r.Intn(500)))
		}

		// Query 활동
		queryMap.AddInt("requests_total", int64(r.Intn(20)))
		queryMap.AddInt("traces_found", int64(r.Intn(50)))
		if fv, ok := queryMap.Get("request_duration_avg_ms").(*Float); ok {
			fv.Set(float64(10 + r.Intn(200)))
		}

		// Storage 활동
		if wv, ok := storageMap.Get("write_latency_avg_ms").(*Float); ok {
			wv.Set(float64(1 + r.Intn(50)))
		}
		if rv, ok := storageMap.Get("read_latency_avg_ms").(*Float); ok {
			rv.Set(float64(5 + r.Intn(100)))
		}
		if r.Intn(5) == 0 {
			storageMap.AddInt("errors_total", 1)
		}

		// 런타임 갱신
		rm = collectRuntimeMetrics()
		goroutines.Set(int64(rm.Goroutines))
		heapAlloc.Set(int64(rm.HeapAlloc))

		fmt.Printf("  Round %d: spans_rcvd=%s dropped=%d queue=%s\n",
			round,
			collectorMap.Get("spans_received").String(),
			dropped,
			collectorMap.Get("queue_length").String(),
		)
	}
	fmt.Println()

	// --- /debug/vars 출력 ---
	fmt.Println("[4] /debug/vars 엔드포인트 (JSON)")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println(registry.ServeJSON())
	fmt.Println()

	// --- 구조화된 메트릭스 ---
	fmt.Println("[5] 구조화된 메트릭스 출력")
	fmt.Println(strings.Repeat("-", 60))

	type MetricSnapshot struct {
		Runtime   RuntimeMetrics     `json:"runtime"`
		Collector map[string]string  `json:"collector"`
		Query     map[string]string  `json:"query"`
		Storage   map[string]string  `json:"storage"`
		BuildInfo string             `json:"build_info"`
	}

	snapshot := MetricSnapshot{
		Runtime:   collectRuntimeMetrics(),
		Collector: make(map[string]string),
		Query:     make(map[string]string),
		Storage:   make(map[string]string),
		BuildInfo: "v1.55.0",
	}

	for _, key := range collectorMap.keys {
		snapshot.Collector[key] = collectorMap.Get(key).String()
	}
	for _, key := range queryMap.keys {
		snapshot.Query[key] = queryMap.Get(key).String()
	}
	for _, key := range storageMap.keys {
		snapshot.Storage[key] = storageMap.Get(key).String()
	}

	jsonData, _ := json.MarshalIndent(snapshot, "  ", "  ")
	fmt.Printf("  %s\n", jsonData)
	fmt.Println()

	fmt.Println("=== 시뮬레이션 완료 ===")
}
