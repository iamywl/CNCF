// Alertmanager Notification Pipeline PoC
//
// Alertmanager의 Stage 기반 알림 파이프라인을 시뮬레이션한다.
// 실제 notify/notify.go의 Stage 체인 패턴을 재현한다.
//
// 핵심 개념:
//   - Stage 인터페이스 (Exec 메서드)
//   - MultiStage (순차 실행)
//   - FanoutStage (병렬 실행)
//   - MuteStage, DedupStage, RetryStage 등
//
// 실행: go run main.go

package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Alert는 알림 데이터이다.
type Alert struct {
	Name     string
	Severity string
	Status   string // "firing" or "resolved"
}

func (a *Alert) String() string {
	return fmt.Sprintf("{%s severity=%s status=%s}", a.Name, a.Severity, a.Status)
}

// Stage는 파이프라인의 단계를 나타내는 인터페이스이다.
type Stage interface {
	Exec(ctx context.Context, alerts []*Alert) ([]*Alert, error)
}

// MultiStage는 여러 Stage를 순차적으로 실행한다.
type MultiStage []Stage

func (ms MultiStage) Exec(ctx context.Context, alerts []*Alert) ([]*Alert, error) {
	var err error
	for _, s := range ms {
		if len(alerts) == 0 {
			return nil, nil
		}
		alerts, err = s.Exec(ctx, alerts)
		if err != nil {
			return nil, err
		}
	}
	return alerts, nil
}

// FanoutStage는 여러 Stage를 병렬로 실행한다.
type FanoutStage []Stage

func (fs FanoutStage) Exec(ctx context.Context, alerts []*Alert) ([]*Alert, error) {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var allResults []*Alert

	fmt.Printf("    [FanoutStage] %d개 Integration에 병렬 전송\n", len(fs))

	for i, s := range fs {
		wg.Add(1)
		go func(idx int, stage Stage) {
			defer wg.Done()
			result, err := stage.Exec(ctx, alerts)
			if err != nil {
				fmt.Printf("    [FanoutStage] Integration %d 오류: %v\n", idx, err)
				return
			}
			mu.Lock()
			allResults = append(allResults, result...)
			mu.Unlock()
		}(i, s)
	}

	wg.Wait()
	return allResults, nil
}

// MuteStage는 Silence/Inhibition으로 억제된 Alert를 필터링한다.
type MuteStage struct {
	silencedAlerts map[string]bool // 이름 기반 Silence
}

func NewMuteStage(silenced []string) *MuteStage {
	m := &MuteStage{silencedAlerts: make(map[string]bool)}
	for _, s := range silenced {
		m.silencedAlerts[s] = true
	}
	return m
}

func (ms *MuteStage) Exec(ctx context.Context, alerts []*Alert) ([]*Alert, error) {
	fmt.Printf("    [MuteStage] 입력: %d개 Alert\n", len(alerts))

	var filtered []*Alert
	for _, a := range alerts {
		if ms.silencedAlerts[a.Name] {
			fmt.Printf("    [MuteStage] %s → SILENCED (억제됨)\n", a)
		} else {
			filtered = append(filtered, a)
		}
	}

	fmt.Printf("    [MuteStage] 출력: %d개 Alert (억제: %d개)\n",
		len(filtered), len(alerts)-len(filtered))
	return filtered, nil
}

// WaitStage는 RepeatInterval을 확인하여 반복 알림을 제어한다.
type WaitStage struct {
	lastSent map[string]time.Time
	interval time.Duration
}

func NewWaitStage(interval time.Duration) *WaitStage {
	return &WaitStage{
		lastSent: make(map[string]time.Time),
		interval: interval,
	}
}

func (ws *WaitStage) Exec(ctx context.Context, alerts []*Alert) ([]*Alert, error) {
	fmt.Printf("    [WaitStage] RepeatInterval=%v 확인\n", ws.interval)

	var passed []*Alert
	now := time.Now()
	for _, a := range alerts {
		if last, ok := ws.lastSent[a.Name]; ok {
			if now.Sub(last) < ws.interval {
				fmt.Printf("    [WaitStage] %s → 반복 대기 중 (마지막 전송: %v 전)\n",
					a, now.Sub(last).Round(time.Millisecond))
				continue
			}
		}
		ws.lastSent[a.Name] = now
		passed = append(passed, a)
	}

	fmt.Printf("    [WaitStage] 출력: %d개 Alert\n", len(passed))
	return passed, nil
}

// DedupStage는 이미 전송된 Alert의 중복을 제거한다.
type DedupStage struct {
	sentLog map[string][]string // groupKey → 전송된 alert 이름들
}

func NewDedupStage() *DedupStage {
	return &DedupStage{sentLog: make(map[string][]string)}
}

func (ds *DedupStage) Exec(ctx context.Context, alerts []*Alert) ([]*Alert, error) {
	groupKey := ctx.Value("groupKey").(string)
	fmt.Printf("    [DedupStage] 그룹 %q 중복 확인\n", groupKey)

	prevSent := ds.sentLog[groupKey]
	prevSet := make(map[string]bool)
	for _, name := range prevSent {
		prevSet[name] = true
	}

	var newAlerts []*Alert
	var newNames []string
	for _, a := range alerts {
		if !prevSet[a.Name] || a.Status == "resolved" {
			newAlerts = append(newAlerts, a)
			newNames = append(newNames, a.Name)
		} else {
			fmt.Printf("    [DedupStage] %s → 이미 전송됨 (중복 제거)\n", a)
		}
	}

	// 전송 기록 업데이트
	ds.sentLog[groupKey] = append(prevSent, newNames...)

	fmt.Printf("    [DedupStage] 출력: %d개 Alert (중복 제거: %d개)\n",
		len(newAlerts), len(alerts)-len(newAlerts))
	return newAlerts, nil
}

// RetryStage는 알림 전송 실패 시 재시도한다.
type RetryStage struct {
	integration string
	failCount   int // 시뮬레이션용: 처음 N번 실패
	attempts    int
}

func NewRetryStage(name string, failCount int) *RetryStage {
	return &RetryStage{integration: name, failCount: failCount}
}

func (rs *RetryStage) Exec(ctx context.Context, alerts []*Alert) ([]*Alert, error) {
	if len(alerts) == 0 {
		return nil, nil
	}

	maxRetries := 3
	var names []string
	for _, a := range alerts {
		names = append(names, a.Name)
	}

	for attempt := 1; attempt <= maxRetries; attempt++ {
		rs.attempts++
		if rs.attempts <= rs.failCount {
			fmt.Printf("    [RetryStage] %s 전송 시도 %d/%d → 실패 (재시도)\n",
				rs.integration, attempt, maxRetries)
			time.Sleep(10 * time.Millisecond) // 짧은 backoff
			continue
		}

		fmt.Printf("    [RetryStage] %s 전송 성공! Alert: [%s]\n",
			rs.integration, strings.Join(names, ", "))
		return alerts, nil
	}

	return nil, fmt.Errorf("%s 전송 최종 실패", rs.integration)
}

func main() {
	fmt.Println("=== Alertmanager Notification Pipeline PoC ===")
	fmt.Println()

	// 테스트 Alert 생성
	alerts := []*Alert{
		{Name: "HighCPU", Severity: "critical", Status: "firing"},
		{Name: "HighMemory", Severity: "warning", Status: "firing"},
		{Name: "DiskFull", Severity: "critical", Status: "firing"},
		{Name: "MaintenanceAlert", Severity: "info", Status: "firing"},
	}

	fmt.Println("입력 Alert:")
	for _, a := range alerts {
		fmt.Printf("  %s\n", a)
	}
	fmt.Println()

	// Pipeline 구성:
	// MuteStage → WaitStage → DedupStage → FanoutStage[RetryStage(Slack), RetryStage(Email)]
	pipeline := MultiStage{
		NewMuteStage([]string{"MaintenanceAlert"}),           // Silence
		NewWaitStage(1 * time.Second),                         // RepeatInterval
		NewDedupStage(),                                       // 중복 제거
		FanoutStage{                                           // 병렬 전송
			NewRetryStage("Slack", 1),                         // Slack (1번 실패 후 성공)
			NewRetryStage("Email", 0),                         // Email (즉시 성공)
		},
	}

	// 첫 번째 실행
	fmt.Println("=== 1차 Pipeline 실행 ===")
	ctx := context.WithValue(context.Background(), "groupKey", "alertname=HighCPU")
	result, err := pipeline.Exec(ctx, alerts)
	if err != nil {
		fmt.Printf("파이프라인 오류: %v\n", err)
	} else {
		fmt.Printf("\n최종 전송된 Alert: %d개\n", len(result))
	}

	fmt.Println()
	fmt.Println("=== 2차 Pipeline 실행 (동일 Alert, 중복 제거 테스트) ===")
	// 같은 Alert를 다시 전송 → DedupStage에서 중복 제거
	alerts2 := []*Alert{
		{Name: "HighCPU", Severity: "critical", Status: "firing"},     // 중복
		{Name: "NewAlert", Severity: "warning", Status: "firing"},      // 새로운
		{Name: "HighMemory", Severity: "warning", Status: "resolved"},  // 해결됨 (전송)
	}

	result2, err := pipeline.Exec(ctx, alerts2)
	if err != nil {
		fmt.Printf("파이프라인 오류: %v\n", err)
	} else {
		fmt.Printf("\n최종 전송된 Alert: %d개\n", len(result2))
	}

	fmt.Println()
	fmt.Println("=== 파이프라인 동작 원리 ===")
	fmt.Println("1. MuteStage: Silence/Inhibition으로 억제된 Alert 필터링")
	fmt.Println("2. WaitStage: RepeatInterval 미경과 Alert 대기")
	fmt.Println("3. DedupStage: nflog 확인하여 이미 전송된 Alert 제거")
	fmt.Println("4. FanoutStage: 여러 Integration에 병렬 전송")
	fmt.Println("5. RetryStage: 실패 시 Exponential Backoff 재시도")
}
