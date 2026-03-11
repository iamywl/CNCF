package main

import (
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// containerd Cgroups 메트릭스 수집 시뮬레이션
// =============================================================================
//
// containerd는 cgroups(v1/v2)를 통해 컨테이너의 CPU, 메모리, IO 등
// 리소스 사용량 메트릭스를 수집한다.
//
// 핵심 개념:
//   - cgroups v2 통합 파일 (/sys/fs/cgroup/.../cpu.stat, memory.stat 등)
//   - 메트릭스 수집 주기 (Prometheus 스크래핑 대응)
//   - 컨테이너별/Pod별 메트릭스 집계
//   - OOM 감지 및 이벤트 발행
//
// 실제 코드 참조:
//   - pkg/cri/server/container_stats.go: CRI 메트릭스 핸들러
//   - metrics/: Prometheus 메트릭스 노출
//   - cgroups v2: github.com/containerd/cgroups
// =============================================================================

// --- Cgroups 메트릭스 구조체 ---

type CPUStats struct {
	UsageUsec     uint64  // 총 CPU 사용 시간 (마이크로초)
	UserUsec      uint64  // 유저 모드 CPU 시간
	SystemUsec    uint64  // 시스템 모드 CPU 시간
	NrPeriods     uint64  // CFS 스케줄링 주기 수
	NrThrottled   uint64  // 스로틀링된 횟수
	ThrottledUsec uint64  // 스로틀링된 시간
	UsagePercent  float64 // CPU 사용률 (계산값)
}

type MemoryStats struct {
	Usage      uint64 // 현재 메모리 사용량 (바이트)
	Limit      uint64 // 메모리 제한
	Cache      uint64 // 페이지 캐시
	RSS        uint64 // RSS (실제 물리 메모리)
	Swap       uint64 // 스왑 사용량
	OOMKills   uint64 // OOM Kill 횟수
	WorkingSet uint64 // working set (RSS + cache - inactive file)
	UsagePercent float64
}

type IOStats struct {
	ReadBytes  uint64
	WriteBytes uint64
	ReadOps    uint64
	WriteOps   uint64
}

type NetworkStats struct {
	RxBytes   uint64
	TxBytes   uint64
	RxPackets uint64
	TxPackets uint64
	RxDropped uint64
	TxDropped uint64
}

type ContainerMetrics struct {
	ContainerID string
	Name        string
	Timestamp   time.Time
	CPU         CPUStats
	Memory      MemoryStats
	IO          IOStats
	Network     NetworkStats
	PID         uint64
}

// --- 메트릭스 수집기 ---

type MetricsCollector struct {
	mu         sync.RWMutex
	containers map[string]*ContainerState
	r          *rand.Rand
}

type ContainerState struct {
	ID          string
	Name        string
	CPULimit    uint64 // millicores
	MemLimit    uint64 // bytes
	prevCPU     uint64
	prevTime    time.Time
	oomCount    uint64
}

func NewMetricsCollector() *MetricsCollector {
	return &MetricsCollector{
		containers: make(map[string]*ContainerState),
		r:          rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (mc *MetricsCollector) RegisterContainer(id, name string, cpuLimit uint64, memLimit uint64) {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	mc.containers[id] = &ContainerState{
		ID:       id,
		Name:     name,
		CPULimit: cpuLimit,
		MemLimit: memLimit,
		prevTime: time.Now(),
	}
}

// CollectMetrics는 cgroups에서 메트릭스를 읽는 것을 시뮬레이션한다.
// 실제로는 /sys/fs/cgroup/<containerID>/cpu.stat 등의 파일을 읽는다.
func (mc *MetricsCollector) CollectMetrics(containerID string) (*ContainerMetrics, error) {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	state, ok := mc.containers[containerID]
	if !ok {
		return nil, fmt.Errorf("container %s not found", containerID)
	}

	now := time.Now()
	elapsed := now.Sub(state.prevTime).Seconds()
	if elapsed < 0.001 {
		elapsed = 1.0
	}

	// CPU 시뮬레이션 (0-100% 범위)
	cpuUsageDelta := uint64(mc.r.Intn(int(state.CPULimit))) * 1000 // microseconds
	state.prevCPU += cpuUsageDelta
	cpuPercent := float64(cpuUsageDelta) / (elapsed * 1000000) * 100

	throttled := uint64(0)
	throttledTime := uint64(0)
	if cpuPercent > 80 {
		throttled = uint64(mc.r.Intn(10))
		throttledTime = uint64(mc.r.Intn(50000))
	}

	// 메모리 시뮬레이션
	memUsage := uint64(float64(state.MemLimit) * (0.3 + mc.r.Float64()*0.5))
	rss := uint64(float64(memUsage) * 0.7)
	cache := memUsage - rss

	// OOM 감지
	oomKills := state.oomCount
	if memUsage > uint64(float64(state.MemLimit)*0.95) && mc.r.Intn(10) == 0 {
		oomKills++
		state.oomCount = oomKills
		fmt.Printf("    [OOM] Container %s: OOM Kill 감지! (usage=%dMB, limit=%dMB)\n",
			state.Name, memUsage/(1024*1024), state.MemLimit/(1024*1024))
	}

	memPercent := float64(memUsage) / float64(state.MemLimit) * 100

	// IO 시뮬레이션
	readBytes := uint64(mc.r.Intn(10000000))
	writeBytes := uint64(mc.r.Intn(5000000))

	// Network 시뮬레이션
	rxBytes := uint64(mc.r.Intn(50000000))
	txBytes := uint64(mc.r.Intn(30000000))

	state.prevTime = now

	return &ContainerMetrics{
		ContainerID: containerID,
		Name:        state.Name,
		Timestamp:   now,
		CPU: CPUStats{
			UsageUsec:     state.prevCPU,
			UserUsec:      state.prevCPU * 7 / 10,
			SystemUsec:    state.prevCPU * 3 / 10,
			NrThrottled:   throttled,
			ThrottledUsec: throttledTime,
			UsagePercent:  cpuPercent,
		},
		Memory: MemoryStats{
			Usage:        memUsage,
			Limit:        state.MemLimit,
			Cache:        cache,
			RSS:          rss,
			OOMKills:     oomKills,
			WorkingSet:   rss + cache/2,
			UsagePercent: memPercent,
		},
		IO: IOStats{
			ReadBytes:  readBytes,
			WriteBytes: writeBytes,
			ReadOps:    uint64(mc.r.Intn(10000)),
			WriteOps:   uint64(mc.r.Intn(5000)),
		},
		Network: NetworkStats{
			RxBytes:   rxBytes,
			TxBytes:   txBytes,
			RxPackets: rxBytes / 1500,
			TxPackets: txBytes / 1500,
			RxDropped: uint64(mc.r.Intn(10)),
			TxDropped: uint64(mc.r.Intn(5)),
		},
		PID: uint64(mc.r.Intn(50) + 1),
	}, nil
}

// --- Prometheus 메트릭스 포매터 ---

type PrometheusMetric struct {
	Name   string
	Labels map[string]string
	Value  float64
}

func (m PrometheusMetric) String() string {
	labels := make([]string, 0, len(m.Labels))
	for k, v := range m.Labels {
		labels = append(labels, fmt.Sprintf(`%s="%s"`, k, v))
	}
	return fmt.Sprintf("%s{%s} %g", m.Name, strings.Join(labels, ","), m.Value)
}

func MetricsToPrometheus(m *ContainerMetrics) []PrometheusMetric {
	labels := map[string]string{
		"container_id": m.ContainerID[:12],
		"name":         m.Name,
	}

	metrics := []PrometheusMetric{
		{"container_cpu_usage_seconds_total", labels, float64(m.CPU.UsageUsec) / 1e6},
		{"container_cpu_user_seconds_total", labels, float64(m.CPU.UserUsec) / 1e6},
		{"container_cpu_system_seconds_total", labels, float64(m.CPU.SystemUsec) / 1e6},
		{"container_cpu_cfs_throttled_periods_total", labels, float64(m.CPU.NrThrottled)},
		{"container_memory_usage_bytes", labels, float64(m.Memory.Usage)},
		{"container_memory_rss_bytes", labels, float64(m.Memory.RSS)},
		{"container_memory_cache_bytes", labels, float64(m.Memory.Cache)},
		{"container_memory_working_set_bytes", labels, float64(m.Memory.WorkingSet)},
		{"container_memory_oom_kills_total", labels, float64(m.Memory.OOMKills)},
		{"container_fs_reads_bytes_total", labels, float64(m.IO.ReadBytes)},
		{"container_fs_writes_bytes_total", labels, float64(m.IO.WriteBytes)},
		{"container_network_receive_bytes_total", labels, float64(m.Network.RxBytes)},
		{"container_network_transmit_bytes_total", labels, float64(m.Network.TxBytes)},
		{"container_processes", labels, float64(m.PID)},
	}
	return metrics
}

// --- Pod 메트릭스 집계 ---

type PodMetricsSummary struct {
	PodName    string
	Containers []ContainerMetrics
	TotalCPU   float64
	TotalMem   uint64
}

func AggregatePodMetrics(podName string, metrics []*ContainerMetrics) PodMetricsSummary {
	summary := PodMetricsSummary{PodName: podName}
	for _, m := range metrics {
		summary.Containers = append(summary.Containers, *m)
		summary.TotalCPU += m.CPU.UsagePercent
		summary.TotalMem += m.Memory.Usage
	}
	return summary
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
	fmt.Println("=== containerd Cgroups 메트릭스 수집 시뮬레이션 ===")
	fmt.Println()

	collector := NewMetricsCollector()

	// --- 컨테이너 등록 ---
	fmt.Println("[1] 컨테이너 등록")
	fmt.Println(strings.Repeat("-", 65))

	containers := []struct {
		id, name string
		cpu      uint64 // millicores
		mem      uint64 // bytes
	}{
		{"ctr-aabbccdd11223344", "nginx", 500, 256 * 1024 * 1024},
		{"ctr-eeff00112233aabb", "app-server", 2000, 1024 * 1024 * 1024},
		{"ctr-44556677889900aa", "redis-cache", 1000, 512 * 1024 * 1024},
		{"ctr-bbccddee11220033", "log-collector", 200, 128 * 1024 * 1024},
	}

	for _, c := range containers {
		collector.RegisterContainer(c.id, c.name, c.cpu, c.mem)
		fmt.Printf("  %-15s CPU=%dm Memory=%s\n", c.name, c.cpu, formatBytes(c.mem))
	}
	fmt.Println()

	// --- 메트릭스 수집 ---
	fmt.Println("[2] 메트릭스 수집 (3회 반복)")
	fmt.Println(strings.Repeat("-", 65))

	for round := 1; round <= 3; round++ {
		fmt.Printf("\n  === 수집 #%d (t=%s) ===\n", round, time.Now().Format("15:04:05"))
		for _, c := range containers {
			m, err := collector.CollectMetrics(c.id)
			if err != nil {
				fmt.Printf("  Error: %v\n", err)
				continue
			}
			fmt.Printf("  %-15s CPU=%.1f%% Mem=%s/%s(%.1f%%) IO(R/W)=%s/%s PIDs=%d",
				m.Name,
				m.CPU.UsagePercent,
				formatBytes(m.Memory.Usage), formatBytes(m.Memory.Limit), m.Memory.UsagePercent,
				formatBytes(m.IO.ReadBytes), formatBytes(m.IO.WriteBytes),
				m.PID,
			)
			if m.CPU.NrThrottled > 0 {
				fmt.Printf(" [THROTTLED:%d]", m.CPU.NrThrottled)
			}
			if m.Memory.OOMKills > 0 {
				fmt.Printf(" [OOM:%d]", m.Memory.OOMKills)
			}
			fmt.Println()
		}
		time.Sleep(50 * time.Millisecond)
	}
	fmt.Println()

	// --- Prometheus 형식 출력 ---
	fmt.Println("[3] Prometheus 메트릭스 형식")
	fmt.Println(strings.Repeat("-", 65))

	for _, c := range containers[:2] { // 처음 2개만
		m, _ := collector.CollectMetrics(c.id)
		promMetrics := MetricsToPrometheus(m)
		fmt.Printf("\n  # Container: %s\n", m.Name)
		for _, pm := range promMetrics {
			fmt.Printf("  %s\n", pm)
		}
	}
	fmt.Println()

	// --- Pod 집계 ---
	fmt.Println("[4] Pod 메트릭스 집계")
	fmt.Println(strings.Repeat("-", 65))

	// Pod 1: nginx + app-server
	var pod1Metrics []*ContainerMetrics
	m1, _ := collector.CollectMetrics(containers[0].id)
	m2, _ := collector.CollectMetrics(containers[1].id)
	pod1Metrics = append(pod1Metrics, m1, m2)
	summary1 := AggregatePodMetrics("web-app-pod", pod1Metrics)

	fmt.Printf("\n  Pod: %s\n", summary1.PodName)
	fmt.Printf("    Total CPU: %.1f%%\n", summary1.TotalCPU)
	fmt.Printf("    Total Memory: %s\n", formatBytes(summary1.TotalMem))
	for _, cm := range summary1.Containers {
		fmt.Printf("    - %-15s CPU=%.1f%% Mem=%s\n", cm.Name, cm.CPU.UsagePercent, formatBytes(cm.Memory.Usage))
	}

	// Pod 2: redis + log-collector
	var pod2Metrics []*ContainerMetrics
	m3, _ := collector.CollectMetrics(containers[2].id)
	m4, _ := collector.CollectMetrics(containers[3].id)
	pod2Metrics = append(pod2Metrics, m3, m4)
	summary2 := AggregatePodMetrics("cache-pod", pod2Metrics)

	fmt.Printf("\n  Pod: %s\n", summary2.PodName)
	fmt.Printf("    Total CPU: %.1f%%\n", summary2.TotalCPU)
	fmt.Printf("    Total Memory: %s\n", formatBytes(summary2.TotalMem))
	for _, cm := range summary2.Containers {
		fmt.Printf("    - %-15s CPU=%.1f%% Mem=%s\n", cm.Name, cm.CPU.UsagePercent, formatBytes(cm.Memory.Usage))
	}
	fmt.Println()

	fmt.Println("=== 시뮬레이션 완료 ===")
}
