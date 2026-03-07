// Alertmanager Retry Backoff PoC
//
// Alertmanager의 RetryStage에서 사용하는 Exponential Backoff 재시도를 시뮬레이션한다.
// cenkalti/backoff/v4 라이브러리의 동작을 표준 라이브러리로 재현한다.
//
// 핵심 개념:
//   - Exponential Backoff (지수 백오프)
//   - Jitter (무작위 편차로 thundering herd 방지)
//   - 최대 재시도 시간 (MaxElapsedTime)
//   - 재시도 가능/불가능 오류 구분
//
// 실행: go run main.go

package main

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"time"
)

// BackoffConfig는 Exponential Backoff 설정이다.
type BackoffConfig struct {
	InitialInterval time.Duration // 초기 대기 시간
	Multiplier      float64       // 증가 배수
	MaxInterval     time.Duration // 최대 대기 시간
	MaxElapsedTime  time.Duration // 최대 총 경과 시간
	JitterFactor    float64       // Jitter 비율 (0~1)
}

// DefaultBackoff는 기본 Backoff 설정이다.
func DefaultBackoff() BackoffConfig {
	return BackoffConfig{
		InitialInterval: 100 * time.Millisecond,
		Multiplier:      2.0,
		MaxInterval:     2 * time.Second,
		MaxElapsedTime:  10 * time.Second,
		JitterFactor:    0.5,
	}
}

// ExponentialBackoff는 지수 백오프 구현이다.
type ExponentialBackoff struct {
	config      BackoffConfig
	currentWait time.Duration
	startTime   time.Time
	attempt     int
}

// NewExponentialBackoff는 새 ExponentialBackoff를 생성한다.
func NewExponentialBackoff(config BackoffConfig) *ExponentialBackoff {
	return &ExponentialBackoff{
		config:      config,
		currentWait: config.InitialInterval,
		startTime:   time.Now(),
	}
}

// NextBackoff는 다음 대기 시간을 반환한다.
// 최대 경과 시간 초과 시 -1을 반환한다 (재시도 중단).
func (eb *ExponentialBackoff) NextBackoff() time.Duration {
	// 최대 경과 시간 확인
	elapsed := time.Since(eb.startTime)
	if elapsed >= eb.config.MaxElapsedTime {
		return -1 // 재시도 중단
	}

	eb.attempt++

	// 현재 대기 시간 계산
	wait := eb.currentWait

	// Jitter 적용 (thundering herd 방지)
	if eb.config.JitterFactor > 0 {
		jitter := 1.0 + (rand.Float64()*2-1)*eb.config.JitterFactor
		wait = time.Duration(float64(wait) * jitter)
	}

	// 다음 대기 시간 계산 (지수 증가)
	eb.currentWait = time.Duration(float64(eb.currentWait) * eb.config.Multiplier)
	if eb.currentWait > eb.config.MaxInterval {
		eb.currentWait = eb.config.MaxInterval
	}

	return wait
}

// RetryError는 재시도 가능 여부를 포함하는 오류이다.
type RetryError struct {
	Message   string
	Retryable bool
}

func (e *RetryError) Error() string {
	return e.Message
}

// Integration은 알림 전송 인터페이스이다.
type Integration struct {
	Name       string
	failUntil  int // 시뮬레이션: 이 횟수까지 실패
	attempts   int
	permanent  bool // true면 영구 오류
}

// Notify는 알림을 전송한다 (시뮬레이션).
func (i *Integration) Notify() (retry bool, err error) {
	i.attempts++

	if i.permanent {
		return false, &RetryError{
			Message:   fmt.Sprintf("%s: 영구 오류 (잘못된 API 키)", i.Name),
			Retryable: false,
		}
	}

	if i.attempts <= i.failUntil {
		return true, &RetryError{
			Message:   fmt.Sprintf("%s: 일시적 오류 (시도 %d/%d)", i.Name, i.attempts, i.failUntil),
			Retryable: true,
		}
	}

	return false, nil // 성공
}

// RetryStage는 재시도 Stage이다.
func RetryStage(ctx context.Context, integration *Integration, config BackoffConfig) error {
	backoff := NewExponentialBackoff(config)
	startTime := time.Now()

	for {
		retry, err := integration.Notify()

		if err == nil {
			elapsed := time.Since(startTime)
			fmt.Printf("    ✅ %s 전송 성공 (시도 %d, 소요: %v)\n",
				integration.Name, integration.attempts, elapsed.Round(time.Millisecond))
			return nil
		}

		retryErr, ok := err.(*RetryError)
		if ok && !retryErr.Retryable {
			fmt.Printf("    ❌ %s 영구 오류, 재시도 중단: %v\n", integration.Name, err)
			return err
		}

		if !retry {
			fmt.Printf("    ❌ %s 실패, 재시도 불가: %v\n", integration.Name, err)
			return err
		}

		// 다음 대기 시간 계산
		wait := backoff.NextBackoff()
		if wait < 0 {
			fmt.Printf("    ❌ %s 최대 재시도 시간 초과, 포기\n", integration.Name)
			return fmt.Errorf("최대 재시도 시간 %v 초과", config.MaxElapsedTime)
		}

		fmt.Printf("    ⏳ %s 실패 (시도 %d): %v → %v 후 재시도\n",
			integration.Name, integration.attempts, err, wait.Round(time.Millisecond))

		select {
		case <-time.After(wait):
			// 재시도
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func main() {
	fmt.Println("=== Alertmanager Retry Backoff PoC ===")
	fmt.Println()

	// Backoff 간격 시각화
	fmt.Println("--- Exponential Backoff 간격 ---")
	config := DefaultBackoff()
	fmt.Printf("초기: %v, 배수: %.1f, 최대: %v, Jitter: %.0f%%\n",
		config.InitialInterval, config.Multiplier, config.MaxInterval, config.JitterFactor*100)
	fmt.Println()

	fmt.Println("Jitter 없는 이론적 간격:")
	wait := config.InitialInterval
	for i := 1; i <= 8; i++ {
		fmt.Printf("  시도 %d: %v\n", i, wait)
		wait = time.Duration(float64(wait) * config.Multiplier)
		if wait > config.MaxInterval {
			wait = config.MaxInterval
		}
	}
	fmt.Println()

	// 시나리오 1: 3번 실패 후 성공
	fmt.Println("--- 시나리오 1: 일시적 오류 후 성공 ---")
	config1 := BackoffConfig{
		InitialInterval: 50 * time.Millisecond,
		Multiplier:      2.0,
		MaxInterval:     500 * time.Millisecond,
		MaxElapsedTime:  5 * time.Second,
		JitterFactor:    0.1,
	}

	slack := &Integration{Name: "Slack", failUntil: 3}
	ctx := context.Background()
	err := RetryStage(ctx, slack, config1)
	if err != nil {
		fmt.Printf("  최종 결과: 실패 - %v\n", err)
	}
	fmt.Println()

	// 시나리오 2: 영구 오류 (재시도 불가)
	fmt.Println("--- 시나리오 2: 영구 오류 (재시도 중단) ---")
	pagerduty := &Integration{Name: "PagerDuty", permanent: true}
	err = RetryStage(ctx, pagerduty, config1)
	if err != nil {
		fmt.Printf("  최종 결과: 실패 - %v\n", err)
	}
	fmt.Println()

	// 시나리오 3: 타임아웃 (MaxElapsedTime 초과)
	fmt.Println("--- 시나리오 3: 타임아웃 (최대 재시도 시간 초과) ---")
	config3 := BackoffConfig{
		InitialInterval: 100 * time.Millisecond,
		Multiplier:      2.0,
		MaxInterval:     200 * time.Millisecond,
		MaxElapsedTime:  400 * time.Millisecond, // 짧은 타임아웃
		JitterFactor:    0.0,
	}
	email := &Integration{Name: "Email", failUntil: 100} // 계속 실패
	err = RetryStage(ctx, email, config3)
	if err != nil {
		fmt.Printf("  최종 결과: 실패 - %v\n", err)
	}
	fmt.Println()

	// Jitter 효과 시각화
	fmt.Println("--- Jitter 효과 ---")
	fmt.Println("같은 설정으로 5번 Backoff 시퀀스 생성:")
	jitterConfig := BackoffConfig{
		InitialInterval: 100 * time.Millisecond,
		Multiplier:      2.0,
		MaxInterval:     1 * time.Second,
		MaxElapsedTime:  10 * time.Second,
		JitterFactor:    0.5,
	}

	for trial := 1; trial <= 3; trial++ {
		backoff := NewExponentialBackoff(jitterConfig)
		var waits []string
		for i := 0; i < 5; i++ {
			wait := backoff.NextBackoff()
			waits = append(waits, fmt.Sprintf("%v", wait.Round(time.Millisecond)))
		}
		fmt.Printf("  시도 %d: %v\n", trial, waits)
	}

	fmt.Println()
	fmt.Println("=== 동작 원리 요약 ===")
	fmt.Println("1. 첫 실패: InitialInterval(100ms) 후 재시도")
	fmt.Println("2. 이후: 대기 시간 × Multiplier(2.0) = 지수 증가")
	fmt.Println("3. MaxInterval(2s) 도달 시 더 이상 증가하지 않음")
	fmt.Println("4. Jitter: 무작위 편차로 여러 인스턴스의 동시 재시도 방지")
	fmt.Println("5. MaxElapsedTime 초과 시 재시도 포기")
	fmt.Println("6. 영구 오류(잘못된 API 키 등)는 즉시 포기")

	_ = math.Abs // unused import 방지
}
