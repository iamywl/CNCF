// Alertmanager Config Reload PoC
//
// Alertmanager의 설정 리로드 메커니즘을 시뮬레이션한다.
// config/coordinator.go의 Subscribe/Reload 패턴을 재현한다.
//
// 핵심 개념:
//   - Coordinator: 설정 리로드 조정자
//   - Subscribe: 구독자 콜백 등록
//   - Reload: 설정 파일 리로드 → 구독자 알림
//   - 원자적 설정 교체 (기존 중지 → 새로 시작)
//
// 실행: go run main.go

package main

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// Route는 라우팅 설정이다.
type Route struct {
	Receiver string
	GroupBy  []string
	Matchers []string // 간소화된 매처
	Children []*Route
}

// Config는 Alertmanager 설정이다.
type Config struct {
	Route      *Route
	Receivers  []string
	Inhibitors []string
}

// String은 Config의 요약을 반환한다.
func (c *Config) String() string {
	return fmt.Sprintf("Config{receiver=%s, receivers=%v, groupBy=%v}",
		c.Route.Receiver, c.Receivers, c.Route.GroupBy)
}

// Subscriber는 설정 변경 구독자이다.
type Subscriber func(conf *Config) error

// Coordinator는 설정 리로드를 조정한다.
type Coordinator struct {
	mu          sync.Mutex
	config      *Config
	subscribers []Subscriber
	loadFunc    func() (*Config, error) // 설정 로드 함수

	// 메트릭
	reloadTotal   int
	reloadSuccess int
	reloadFail    int
}

// NewCoordinator는 새 Coordinator를 생성한다.
func NewCoordinator(loadFunc func() (*Config, error)) *Coordinator {
	return &Coordinator{loadFunc: loadFunc}
}

// Subscribe는 설정 변경 구독자를 등록한다.
// 이미 설정이 있으면 즉시 콜백 호출.
func (c *Coordinator) Subscribe(sub Subscriber) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.subscribers = append(c.subscribers, sub)

	// 이미 설정이 로드되어 있으면 즉시 알림
	if c.config != nil {
		if err := sub(c.config); err != nil {
			fmt.Printf("  [Coordinator] 구독자 초기 알림 실패: %v\n", err)
		}
	}
}

// Reload는 설정을 리로드하고 모든 구독자에게 알린다.
func (c *Coordinator) Reload() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.reloadTotal++
	fmt.Println("  [Coordinator] 설정 리로드 시작")

	// 1. 설정 로드
	newConfig, err := c.loadFunc()
	if err != nil {
		c.reloadFail++
		fmt.Printf("  [Coordinator] 설정 로드 실패: %v\n", err)
		return err
	}

	fmt.Printf("  [Coordinator] 새 설정: %s\n", newConfig)

	// 2. 모든 구독자에게 알림
	for i, sub := range c.subscribers {
		fmt.Printf("  [Coordinator] 구독자 %d 알림 중...\n", i)
		if err := sub(newConfig); err != nil {
			c.reloadFail++
			fmt.Printf("  [Coordinator] 구독자 %d 오류: %v\n", i, err)
			return err
		}
	}

	c.config = newConfig
	c.reloadSuccess++
	fmt.Println("  [Coordinator] 설정 리로드 완료")
	return nil
}

// Dispatcher는 Alert 라우팅 컴포넌트 (설정 구독자).
type Dispatcher struct {
	name    string
	running bool
	config  *Config
}

func NewDispatcher(name string) *Dispatcher {
	return &Dispatcher{name: name}
}

func (d *Dispatcher) ApplyConfig(conf *Config) error {
	// 기존 Dispatcher 중지
	if d.running {
		fmt.Printf("    [%s] 기존 인스턴스 중지\n", d.name)
		d.running = false
	}

	// 새 설정으로 시작
	d.config = conf
	d.running = true
	fmt.Printf("    [%s] 새 설정으로 시작: receiver=%s, groupBy=%v\n",
		d.name, conf.Route.Receiver, conf.Route.GroupBy)
	return nil
}

// Inhibitor는 억제 컴포넌트 (설정 구독자).
type Inhibitor struct {
	name    string
	running bool
	rules   []string
}

func NewInhibitor(name string) *Inhibitor {
	return &Inhibitor{name: name}
}

func (inh *Inhibitor) ApplyConfig(conf *Config) error {
	// 기존 Inhibitor 중지
	if inh.running {
		fmt.Printf("    [%s] 기존 인스턴스 중지\n", inh.name)
		inh.running = false
	}

	// 새 설정으로 시작
	inh.rules = conf.Inhibitors
	inh.running = true
	fmt.Printf("    [%s] 새 규칙 적용: %v\n", inh.name, inh.rules)
	return nil
}

func main() {
	fmt.Println("=== Alertmanager Config Reload PoC ===")
	fmt.Println()

	// 설정 버전 시뮬레이션
	configVersion := 0
	configs := []*Config{
		{
			Route: &Route{
				Receiver: "default",
				GroupBy:  []string{"alertname"},
			},
			Receivers:  []string{"default", "slack"},
			Inhibitors: []string{"critical-inhibits-warning"},
		},
		{
			Route: &Route{
				Receiver: "pager",
				GroupBy:  []string{"alertname", "cluster"},
			},
			Receivers:  []string{"pager", "slack", "email"},
			Inhibitors: []string{"critical-inhibits-warning", "critical-inhibits-info"},
		},
		nil, // 오류 시뮬레이션
	}

	loadFunc := func() (*Config, error) {
		if configVersion >= len(configs) || configs[configVersion] == nil {
			return nil, fmt.Errorf("설정 파일 파싱 오류 (버전 %d)", configVersion)
		}
		conf := configs[configVersion]
		configVersion++
		return conf, nil
	}

	coordinator := NewCoordinator(loadFunc)

	// 구독자 등록
	dispatcher := NewDispatcher("Dispatcher")
	inhibitor := NewInhibitor("Inhibitor")

	fmt.Println("--- 1. 초기 설정 로드 ---")
	coordinator.Reload()
	fmt.Println()

	// 구독자 등록 (이미 설정이 있으므로 즉시 알림)
	fmt.Println("--- 2. 구독자 등록 ---")
	coordinator.Subscribe(func(conf *Config) error {
		return dispatcher.ApplyConfig(conf)
	})
	coordinator.Subscribe(func(conf *Config) error {
		return inhibitor.ApplyConfig(conf)
	})
	fmt.Println()

	// 설정 리로드 (SIGHUP 또는 POST /-/reload)
	fmt.Println("--- 3. 설정 리로드 (SIGHUP) ---")
	fmt.Println("변경사항: receiver default→pager, groupBy에 cluster 추가, 억제 규칙 추가")
	if err := coordinator.Reload(); err != nil {
		fmt.Printf("리로드 실패: %v\n", err)
	}
	fmt.Println()

	// 잘못된 설정으로 리로드 시도
	fmt.Println("--- 4. 잘못된 설정 리로드 시도 ---")
	if err := coordinator.Reload(); err != nil {
		fmt.Printf("리로드 실패 (예상된 오류): %v\n", err)
	}
	fmt.Println()

	// 현재 상태 확인
	fmt.Println("--- 5. 현재 상태 ---")
	fmt.Printf("Dispatcher 동작 중: %v\n", dispatcher.running)
	fmt.Printf("Dispatcher 설정: receiver=%s, groupBy=%v\n",
		dispatcher.config.Route.Receiver, dispatcher.config.Route.GroupBy)
	fmt.Printf("Inhibitor 동작 중: %v\n", inhibitor.running)
	fmt.Printf("Inhibitor 규칙: %v\n", inhibitor.rules)
	fmt.Println()

	// 메트릭
	fmt.Println("--- 6. 메트릭 ---")
	fmt.Printf("리로드 시도: %d\n", coordinator.reloadTotal)
	fmt.Printf("리로드 성공: %d\n", coordinator.reloadSuccess)
	fmt.Printf("리로드 실패: %d\n", coordinator.reloadFail)

	fmt.Println()
	fmt.Println("=== 동작 원리 요약 ===")
	fmt.Println("1. SIGHUP 또는 POST /-/reload → Coordinator.Reload()")
	fmt.Println("2. 설정 파일 로드 및 유효성 검증")
	fmt.Println("3. 실패 시 기존 설정 유지 (안전)")
	fmt.Println("4. 성공 시 구독자 순서대로 알림:")
	fmt.Printf("   %s\n", strings.Join([]string{
		"Inhibitor 중지 → 새 규칙 시작",
		"Dispatcher 중지 → 새 Route 시작",
	}, " → "))
	fmt.Println("5. 원자적 교체: 기존 컴포넌트 중지 후 새로 시작")

	_ = time.Second // 사용하지 않는 import 방지
}
