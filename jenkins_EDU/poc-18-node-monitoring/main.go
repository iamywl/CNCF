// Package main은 Jenkins 노드 모니터링 시스템의 핵심 개념을 시뮬레이션한다.
//
// Jenkins의 노드 모니터링은 다음 핵심 메커니즘으로 동작한다:
// 1. NodeMonitor: 확장 포인트 — 모니터 종류 정의
// 2. AbstractNodeMonitorDescriptor: 주기적 실행 + 결과 캐싱
// 3. AbstractAsyncNodeMonitorDescriptor: 비동기 병렬 모니터링
// 4. 임계값 기반 자동 오프라인/온라인 전환
//
// 이 PoC는 Go 표준 라이브러리만으로 이 메커니즘을 재현한다.
package main

import (
	"fmt"
	"math/rand"
	"sync"
	"time"
)

// =============================================================================
// 1. 데이터 모델
// =============================================================================

// Computer는 Jenkins 에이전트 노드를 나타낸다
type Computer struct {
	Name             string
	Online           bool
	TemporarilyOff   bool
	OfflineCause     string
	OfflineTrigger   string // 오프라인을 발생시킨 모니터 이름
	IdleStartMillis  int64
}

// DiskSpace는 디스크 공간 정보
// Jenkins 원본: hudson/node_monitors/DiskSpaceMonitorDescriptor.java의 DiskSpace
type DiskSpace struct {
	Path             string
	Size             int64 // 사용 가능 바이트
	TotalSize        int64
	Threshold        int64
	WarningThreshold int64
}

func (d *DiskSpace) String() string {
	return fmt.Sprintf("%s (여유: %s, 총: %s)",
		d.Path, humanReadableBytes(d.Size), humanReadableBytes(d.TotalSize))
}

func (d *DiskSpace) IsTriggered() bool {
	return d.Threshold > 0 && d.Size <= d.Threshold
}

func (d *DiskSpace) IsWarning() bool {
	return d.WarningThreshold > 0 && d.Size > d.Threshold && d.Size < d.WarningThreshold
}

// ClockDifference는 시계 차이 정보
type ClockDifference struct {
	DiffMillis int64 // 마스터와의 시간 차이 (밀리초)
}

func (c *ClockDifference) String() string {
	if c.DiffMillis > 0 {
		return fmt.Sprintf("+%dms", c.DiffMillis)
	}
	return fmt.Sprintf("%dms", c.DiffMillis)
}

// ResponseTimeData는 응답 시간 데이터
// Jenkins 원본: hudson/node_monitors/ResponseTimeMonitor.java의 Data
type ResponseTimeData struct {
	Past5 []int64 // 최근 5회 응답 시간 (-1 = 타임아웃)
}

func (d *ResponseTimeData) Average() int64 {
	if len(d.Past5) == 0 {
		return 0
	}
	var total int64
	for _, v := range d.Past5 {
		if v < 0 {
			total += 5000 // 타임아웃은 5초로 간주
		} else {
			total += v
		}
	}
	return total / int64(len(d.Past5))
}

func (d *ResponseTimeData) FailureCount() int {
	count := 0
	for i := len(d.Past5) - 1; i >= 0 && d.Past5[i] < 0; i-- {
		count++
	}
	return count
}

func (d *ResponseTimeData) HasTooManyTimeouts() bool {
	return d.FailureCount() >= 5
}

func (d *ResponseTimeData) String() string {
	fc := d.FailureCount()
	if fc > 0 {
		return fmt.Sprintf("Timeout(%d회 연속)", fc)
	}
	return fmt.Sprintf("%dms", d.Average())
}

func (d *ResponseTimeData) AddDataPoint(value int64) {
	d.Past5 = append(d.Past5, value)
	if len(d.Past5) > 5 {
		d.Past5 = d.Past5[len(d.Past5)-5:]
	}
}

// =============================================================================
// 2. NodeMonitor 인터페이스
// =============================================================================

// NodeMonitor는 노드 모니터의 인터페이스
// Jenkins 원본: hudson/node_monitors/NodeMonitor.java
type NodeMonitor interface {
	Name() string
	CanTakeOffline() bool
	IsIgnored() bool
	Monitor(c *Computer) (interface{}, error)
}

// =============================================================================
// 3. 구체적 모니터 구현
// =============================================================================

// DiskSpaceMonitor는 디스크 공간 모니터
// Jenkins 원본: hudson/node_monitors/DiskSpaceMonitor.java
type DiskSpaceMonitor struct {
	Ignored              bool
	FreeSpaceThreshold   int64
	WarningThreshold     int64
}

func (m *DiskSpaceMonitor) Name() string          { return "DiskSpace" }
func (m *DiskSpaceMonitor) CanTakeOffline() bool   { return true }
func (m *DiskSpaceMonitor) IsIgnored() bool        { return m.Ignored }

func (m *DiskSpaceMonitor) Monitor(c *Computer) (interface{}, error) {
	// 에이전트에서 원격 실행되는 GetUsableSpace Callable 시뮬레이션
	freeSpace := int64(rand.Intn(50)) * 1024 * 1024 * 1024 // 0~50 GiB
	totalSpace := int64(100) * 1024 * 1024 * 1024           // 100 GiB
	return &DiskSpace{
		Path:             "/home/jenkins",
		Size:             freeSpace,
		TotalSize:        totalSpace,
		Threshold:        m.FreeSpaceThreshold,
		WarningThreshold: m.WarningThreshold,
	}, nil
}

// ClockMonitor는 시계 동기화 모니터
// Jenkins 원본: hudson/node_monitors/ClockMonitor.java
type ClockMonitor struct {
	Ignored bool
}

func (m *ClockMonitor) Name() string          { return "Clock" }
func (m *ClockMonitor) CanTakeOffline() bool   { return false } // 정보 제공만
func (m *ClockMonitor) IsIgnored() bool        { return m.Ignored }

func (m *ClockMonitor) Monitor(c *Computer) (interface{}, error) {
	// 에이전트에서 시간을 가져와 마스터와 비교하는 시뮬레이션
	diff := int64(rand.Intn(2000) - 1000) // -1000ms ~ +1000ms
	return &ClockDifference{DiffMillis: diff}, nil
}

// ResponseTimeMonitor는 응답 시간 모니터
// Jenkins 원본: hudson/node_monitors/ResponseTimeMonitor.java
type ResponseTimeMonitor struct {
	Ignored    bool
	prevData   map[string]*ResponseTimeData
}

func (m *ResponseTimeMonitor) Name() string          { return "ResponseTime" }
func (m *ResponseTimeMonitor) CanTakeOffline() bool   { return true }
func (m *ResponseTimeMonitor) IsIgnored() bool        { return m.Ignored }

func (m *ResponseTimeMonitor) Monitor(c *Computer) (interface{}, error) {
	if m.prevData == nil {
		m.prevData = make(map[string]*ResponseTimeData)
	}

	data, exists := m.prevData[c.Name]
	if !exists {
		data = &ResponseTimeData{}
	}

	// RTT 측정 시뮬레이션 (Step1 → Step2 → Step3 직렬화 트릭)
	if c.Online {
		rtt := int64(rand.Intn(100)) // 0~100ms
		data.AddDataPoint(rtt)
	} else {
		data.AddDataPoint(-1) // 타임아웃
	}

	m.prevData[c.Name] = data
	return data, nil
}

// =============================================================================
// 4. AbstractAsyncNodeMonitorDescriptor 시뮬레이션
// =============================================================================

// MonitorRecord는 모니터링 결과를 저장하는 스레드(고루틴)
// Jenkins 원본: AbstractNodeMonitorDescriptor.Record
type MonitorRecord struct {
	Data      map[string]interface{} // Computer.Name → 모니터링 결과
	Timestamp time.Time
}

// MonitoringEngine은 비동기 병렬 모니터링 엔진
// Jenkins 원본: AbstractAsyncNodeMonitorDescriptor
type MonitoringEngine struct {
	monitors   []NodeMonitor
	computers  []*Computer
	records    map[string]*MonitorRecord // 모니터 이름 → 최신 결과
	mu         sync.RWMutex
	inProgress map[string]bool
	period     time.Duration
}

func NewMonitoringEngine(monitors []NodeMonitor, computers []*Computer, period time.Duration) *MonitoringEngine {
	return &MonitoringEngine{
		monitors:   monitors,
		computers:  computers,
		records:    make(map[string]*MonitorRecord),
		inProgress: make(map[string]bool),
		period:     period,
	}
}

// TriggerUpdate는 모니터링을 비동기로 시작한다
// Jenkins 원본: AbstractNodeMonitorDescriptor.triggerUpdate()
func (e *MonitoringEngine) TriggerUpdate(monitor NodeMonitor) {
	e.mu.Lock()
	if e.inProgress[monitor.Name()] {
		e.mu.Unlock()
		fmt.Printf("  [%s] 이미 진행 중, 건너뜀\n", monitor.Name())
		return
	}
	e.inProgress[monitor.Name()] = true
	e.mu.Unlock()

	go func() {
		defer func() {
			e.mu.Lock()
			e.inProgress[monitor.Name()] = false
			e.mu.Unlock()
		}()

		startTime := time.Now()
		record := &MonitorRecord{
			Data: make(map[string]interface{}),
		}

		// 모든 컴퓨터에 대해 병렬 모니터링 (AbstractAsyncNodeMonitorDescriptor)
		var wg sync.WaitGroup
		var mu sync.Mutex

		for _, c := range e.computers {
			wg.Add(1)
			go func(comp *Computer) {
				defer wg.Done()

				result, err := monitor.Monitor(comp)
				mu.Lock()
				defer mu.Unlock()

				if err != nil {
					fmt.Printf("  [%s] %s 모니터링 실패: %s\n",
						monitor.Name(), comp.Name, err)
					return
				}
				record.Data[comp.Name] = result
			}(c)
		}

		wg.Wait()
		record.Timestamp = time.Now()

		e.mu.Lock()
		e.records[monitor.Name()] = record
		e.mu.Unlock()

		elapsed := time.Since(startTime)
		fmt.Printf("  [%s] 모니터링 완료: %d개 노드, %v 소요\n",
			monitor.Name(), len(record.Data), elapsed.Round(time.Millisecond))

		// 자동 오프라인/온라인 처리
		if monitor.CanTakeOffline() && !monitor.IsIgnored() {
			e.processAutoOffline(monitor, record)
		}
	}()
}

// processAutoOffline는 임계값에 따라 노드를 자동으로 오프라인/온라인 전환
// Jenkins 원본: DiskSpaceMonitorDescriptor.markNodeOfflineOrOnline()
func (e *MonitoringEngine) processAutoOffline(monitor NodeMonitor, record *MonitorRecord) {
	for _, c := range e.computers {
		data, ok := record.Data[c.Name]
		if !ok {
			continue
		}

		switch v := data.(type) {
		case *DiskSpace:
			if v.IsTriggered() {
				if !c.TemporarilyOff {
					c.TemporarilyOff = true
					c.OfflineCause = fmt.Sprintf("디스크 부족: %s < %s",
						humanReadableBytes(v.Size), humanReadableBytes(v.Threshold))
					c.OfflineTrigger = monitor.Name()
					fmt.Printf("  *** %s → 자동 오프라인: %s\n", c.Name, c.OfflineCause)
				}
			} else if c.TemporarilyOff && c.OfflineTrigger == monitor.Name() {
				c.TemporarilyOff = false
				c.OfflineCause = ""
				c.OfflineTrigger = ""
				fmt.Printf("  *** %s → 자동 온라인 복구\n", c.Name)
			}

		case *ResponseTimeData:
			if v.HasTooManyTimeouts() {
				if c.Online {
					c.Online = false
					c.OfflineCause = "연속 5회 타임아웃으로 연결 해제"
					fmt.Printf("  *** %s → 연결 해제: %s\n", c.Name, c.OfflineCause)
				}
			}
		}
	}
}

// RunAll은 모든 모니터를 실행한다
func (e *MonitoringEngine) RunAll() {
	for _, m := range e.monitors {
		e.TriggerUpdate(m)
	}
}

// PrintReport는 ComputerSet 화면처럼 결과를 표시한다
func (e *MonitoringEngine) PrintReport() {
	e.mu.RLock()
	defer e.mu.RUnlock()

	fmt.Println("\n┌──────────────────┬──────────┬──────────────┬──────────┬──────────┐")
	fmt.Println("│ 노드 이름         │ 상태     │ 디스크 공간   │ 응답 시간 │ 시계 차이 │")
	fmt.Println("├──────────────────┼──────────┼──────────────┼──────────┼──────────┤")

	for _, c := range e.computers {
		status := "Online"
		if c.TemporarilyOff {
			status = "Offline!"
		} else if !c.Online {
			status = "Disconn"
		}

		disk := "N/A"
		if rec, ok := e.records["DiskSpace"]; ok {
			if d, ok := rec.Data[c.Name]; ok {
				ds := d.(*DiskSpace)
				disk = humanReadableBytes(ds.Size)
				if ds.IsTriggered() {
					disk += " (!)"
				} else if ds.IsWarning() {
					disk += " (?)"
				}
			}
		}

		rtt := "N/A"
		if rec, ok := e.records["ResponseTime"]; ok {
			if d, ok := rec.Data[c.Name]; ok {
				rtt = d.(*ResponseTimeData).String()
			}
		}

		clock := "N/A"
		if rec, ok := e.records["Clock"]; ok {
			if d, ok := rec.Data[c.Name]; ok {
				clock = d.(*ClockDifference).String()
			}
		}

		fmt.Printf("│ %-16s │ %-8s │ %-12s │ %-8s │ %-8s │\n",
			c.Name, status, disk, rtt, clock)
	}
	fmt.Println("└──────────────────┴──────────┴──────────────┴──────────┴──────────┘")
}

// =============================================================================
// 유틸리티
// =============================================================================

func humanReadableBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

// =============================================================================
// 메인 데모
// =============================================================================

func main() {
	fmt.Println("=== Jenkins 노드 모니터링 시스템 시뮬레이션 ===")
	fmt.Println()

	// 에이전트 노드 목록
	computers := []*Computer{
		{Name: "agent-linux-1", Online: true},
		{Name: "agent-linux-2", Online: true},
		{Name: "agent-win-1", Online: true},
		{Name: "agent-mac-1", Online: true},
		{Name: "agent-docker-1", Online: false}, // 오프라인 에이전트
	}

	// 모니터 목록
	monitors := []NodeMonitor{
		&DiskSpaceMonitor{
			FreeSpaceThreshold: 1 * 1024 * 1024 * 1024,  // 1 GiB
			WarningThreshold:   5 * 1024 * 1024 * 1024,  // 5 GiB
		},
		&ClockMonitor{},
		&ResponseTimeMonitor{},
	}

	// 모니터링 엔진 생성
	engine := NewMonitoringEngine(monitors, computers, 60*time.Second)

	// 1라운드: 초기 모니터링
	fmt.Println("--- 1라운드: 초기 모니터링 실행 ---")
	engine.RunAll()
	time.Sleep(200 * time.Millisecond) // 비동기 모니터링 완료 대기
	engine.PrintReport()

	// 2라운드: 반복 모니터링 (상태 변화 감지)
	fmt.Println("\n--- 2라운드: 반복 모니터링 (변화 감지) ---")
	engine.RunAll()
	time.Sleep(200 * time.Millisecond)
	engine.PrintReport()

	// NodeMonitorUpdater 시뮬레이션: 에이전트 온라인 시 트리거
	fmt.Println("\n--- NodeMonitorUpdater: agent-docker-1 연결됨 (5초 디바운스) ---")
	computers[4].Online = true
	fmt.Println("  에이전트 연결 이벤트 → 5초 후 모니터링 트리거 (디바운스)")

	// 디바운스 시뮬레이션 (실제로는 5초 대기, 여기서는 즉시 실행)
	engine.RunAll()
	time.Sleep(200 * time.Millisecond)
	engine.PrintReport()

	// 모니터 정보 출력
	fmt.Println("\n--- 등록된 모니터 정보 ---")
	for _, m := range monitors {
		canOffline := "Yes"
		if !m.CanTakeOffline() {
			canOffline = "No (정보 제공만)"
		}
		ignored := "No"
		if m.IsIgnored() {
			ignored = "Yes"
		}
		fmt.Printf("  %s: CanTakeOffline=%s, Ignored=%s\n",
			m.Name(), canOffline, ignored)
	}

	fmt.Println("\n=== 시뮬레이션 완료 ===")
}
