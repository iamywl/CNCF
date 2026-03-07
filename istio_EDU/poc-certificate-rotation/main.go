// Istio 인증서 자동 갱신(Certificate Rotation) 시뮬레이션
//
// 이 PoC는 Istio의 security/pkg/nodeagent/cache/secretcache.go에 구현된
// SecretManagerClient의 인증서 자동 갱신 메커니즘을 시뮬레이션합니다.
//
// 핵심 알고리즘:
//   1. rotateTime(): gracePeriodRatio(0.5) + jitter(0.01)로 갱신 시점 계산
//      - secretLifeTime = expireTime - createdTime
//      - gracePeriod = (graceRatio + jitter) * secretLifeTime
//      - delay = expireTime - gracePeriod - now
//   2. registerSecret(): delayed queue에 갱신 작업 스케줄링
//   3. OnSecretUpdate(): SDS push 알림으로 Envoy에 새 인증서 전달
//
// 참조: istio/security/pkg/nodeagent/cache/secretcache.go
//   - SecretManagerClient 구조체
//   - rotateTime() 함수 (라인 858)
//   - registerSecret() 함수 (라인 877)
//   - GenerateSecret() 함수 (라인 248)

package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	mathrand "math/rand"
	"strings"
	"sync"
	"time"
)

// ========================================================================
// 핵심 데이터 구조
// ========================================================================

// SecretItem은 Istio의 security.SecretItem에 대응하는 인증서 정보 구조체
// 참조: istio/pkg/security/security.go
type SecretItem struct {
	ResourceName     string    // 리소스 이름 (예: "default", "ROOTCA")
	CertificateChain []byte    // PEM 인코딩된 인증서 체인
	PrivateKey       []byte    // PEM 인코딩된 개인 키
	RootCert         []byte    // PEM 인코딩된 루트 인증서
	CreatedTime      time.Time // 인증서 생성 시각
	ExpireTime       time.Time // 인증서 만료 시각
}

// secretCache는 워크로드 인증서와 루트 인증서를 캐시하는 구조체
// 참조: istio/security/pkg/nodeagent/cache/secretcache.go의 secretCache
type secretCache struct {
	mu       sync.RWMutex
	workload *SecretItem
	certRoot []byte
}

func (s *secretCache) GetWorkload() *SecretItem {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.workload == nil {
		return nil
	}
	// 복사본 반환 (원본에서는 포인터 반환이지만, 스레드 안전성을 위해)
	cp := *s.workload
	return &cp
}

func (s *secretCache) SetWorkload(item *SecretItem) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.workload = item
}

func (s *secretCache) GetRoot() []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.certRoot
}

func (s *secretCache) SetRoot(root []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.certRoot = root
}

// ========================================================================
// Delayed Queue - 지연 실행 큐
// ========================================================================

// delayedTask는 지연 실행 작업을 표현
type delayedTask struct {
	fn      func() error
	runAt   time.Time
	id      int
}

// DelayedQueue는 Istio의 pkg/queue.Delayed에 대응하는 지연 큐
// 참조: istio/pkg/queue/delayed.go
type DelayedQueue struct {
	mu      sync.Mutex
	tasks   []delayedTask
	nextID  int
	stopCh  chan struct{}
	wg      sync.WaitGroup
}

func NewDelayedQueue() *DelayedQueue {
	return &DelayedQueue{
		tasks:  make([]delayedTask, 0),
		stopCh: make(chan struct{}),
	}
}

// PushDelayed는 지정된 딜레이 후에 실행될 작업을 큐에 추가
func (q *DelayedQueue) PushDelayed(fn func() error, delay time.Duration) {
	q.mu.Lock()
	q.nextID++
	id := q.nextID
	task := delayedTask{
		fn:    fn,
		runAt: time.Now().Add(delay),
		id:    id,
	}
	q.tasks = append(q.tasks, task)
	q.mu.Unlock()
}

// Run은 큐를 시작하여 작업을 주기적으로 확인하고 실행
func (q *DelayedQueue) Run(stop chan struct{}) {
	ticker := time.NewTicker(50 * time.Millisecond) // 빠른 시뮬레이션을 위해 짧은 간격
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			q.processReady()
		}
	}
}

func (q *DelayedQueue) processReady() {
	q.mu.Lock()
	now := time.Now()
	var remaining []delayedTask
	var ready []delayedTask
	for _, t := range q.tasks {
		if now.After(t.runAt) || now.Equal(t.runAt) {
			ready = append(ready, t)
		} else {
			remaining = append(remaining, t)
		}
	}
	q.tasks = remaining
	q.mu.Unlock()

	for _, t := range ready {
		_ = t.fn()
	}
}

// ========================================================================
// SecretManagerClient - 인증서 관리 클라이언트
// ========================================================================

// SecretManagerClient는 Istio의 SecretManagerClient를 시뮬레이션
// 참조: istio/security/pkg/nodeagent/cache/secretcache.go
type SecretManagerClient struct {
	workloadID string // 워크로드 식별자

	// 설정 옵션
	gracePeriodRatio       float64 // 인증서 수명 대비 갱신 시작 비율 (기본 0.5)
	gracePeriodRatioJitter float64 // 갱신 시점 지터 (기본 0.01)
	certTTL                time.Duration // 인증서 유효 기간

	// 콜백: 인증서 갱신 시 Envoy에 SDS push 알림
	secretHandler func(resourceName string)

	// 인메모리 캐시
	cache secretCache

	// 인증서 생성 직렬화를 위한 뮤텍스
	generateMutex sync.Mutex

	// 지연 큐: 갱신 스케줄링
	queue *DelayedQueue
	stop  chan struct{}

	// CA 클라이언트 (시뮬레이션에서는 자체 서명)
	caKey  *rsa.PrivateKey
	caCert *x509.Certificate

	// 이벤트 로그 (시뮬레이션 출력용)
	eventLog []string
	logMu    sync.Mutex
}

func NewSecretManagerClient(workloadID string, certTTL time.Duration, gracePeriodRatio, gracePeriodRatioJitter float64) (*SecretManagerClient, error) {
	// 시뮬레이션용 CA 키/인증서 생성
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("CA 키 생성 실패: %v", err)
	}

	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"Istio CA (시뮬레이션)"},
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	caCertDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("CA 인증서 생성 실패: %v", err)
	}

	caCert, err := x509.ParseCertificate(caCertDER)
	if err != nil {
		return nil, fmt.Errorf("CA 인증서 파싱 실패: %v", err)
	}

	sc := &SecretManagerClient{
		workloadID:             workloadID,
		gracePeriodRatio:       gracePeriodRatio,
		gracePeriodRatioJitter: gracePeriodRatioJitter,
		certTTL:                certTTL,
		queue:                  NewDelayedQueue(),
		stop:                   make(chan struct{}),
		caKey:                  caKey,
		caCert:                 caCert,
		eventLog:               make([]string, 0),
	}

	// 지연 큐 시작
	go sc.queue.Run(sc.stop)

	return sc, nil
}

func (sc *SecretManagerClient) Close() {
	close(sc.stop)
}

// RegisterSecretHandler는 SDS push 콜백 등록
// 참조: secretcache.go의 RegisterSecretHandler()
func (sc *SecretManagerClient) RegisterSecretHandler(h func(resourceName string)) {
	sc.secretHandler = h
}

// OnSecretUpdate는 인증서 변경 시 콜백 호출 (SDS push 시뮬레이션)
// 참조: secretcache.go의 OnSecretUpdate()
func (sc *SecretManagerClient) OnSecretUpdate(resourceName string) {
	if sc.secretHandler != nil {
		sc.secretHandler(resourceName)
	}
}

// addEvent는 시뮬레이션 이벤트 로그 추가
func (sc *SecretManagerClient) addEvent(msg string) {
	sc.logMu.Lock()
	defer sc.logMu.Unlock()
	timestamp := time.Now().Format("15:04:05.000")
	entry := fmt.Sprintf("[%s] [%s] %s", timestamp, sc.workloadID, msg)
	sc.eventLog = append(sc.eventLog, entry)
	fmt.Println(entry)
}

// ========================================================================
// rotateTime - 핵심 알고리즘: 갱신 시점 계산
// ========================================================================

// rotateTime은 인증서 갱신까지의 대기 시간을 계산
// 이것이 Istio 인증서 갱신의 핵심 알고리즘
//
// 참조: secretcache.go 라인 858-875
// var rotateTime = func(secret security.SecretItem, graceRatio float64, graceRatioJitter float64) time.Duration {
//     jitter := (rand.Float64() * graceRatioJitter) * float64(rand.IntN(2)*2-1)
//     jitterGraceRatio := graceRatio + jitter
//     ...
//     secretLifeTime := secret.ExpireTime.Sub(secret.CreatedTime)
//     gracePeriod := time.Duration((jitterGraceRatio) * float64(secretLifeTime))
//     delay := time.Until(secret.ExpireTime.Add(-gracePeriod))
//     ...
// }
func rotateTime(secret SecretItem, graceRatio float64, graceRatioJitter float64) time.Duration {
	// 대규모 플릿에서 동시 갱신을 방지하기 위한 지터 적용
	// rand.IntN(2)*2-1 은 -1 또는 +1을 생성하여 양방향 지터를 만듦
	jitter := (mathrand.Float64() * graceRatioJitter) * float64(mathrand.Intn(2)*2-1)
	jitterGraceRatio := graceRatio + jitter

	// 비율 범위 제한 [0, 1]
	if jitterGraceRatio > 1 {
		jitterGraceRatio = 1
	}
	if jitterGraceRatio < 0 {
		jitterGraceRatio = 0
	}

	// 인증서 전체 수명
	secretLifeTime := secret.ExpireTime.Sub(secret.CreatedTime)

	// grace period = 전체 수명의 graceRatio 비율
	// 예: 수명 10초, graceRatio 0.5이면 gracePeriod = 5초
	// → 만료 5초 전에 갱신 시작
	gracePeriod := time.Duration(jitterGraceRatio * float64(secretLifeTime))

	// 실제 대기 시간 = 만료 시각 - gracePeriod - 현재 시각
	delay := time.Until(secret.ExpireTime.Add(-gracePeriod))
	if delay < 0 {
		delay = 0
	}
	return delay
}

// ========================================================================
// 인증서 생성 및 갱신 로직
// ========================================================================

// generateNewSecret은 CA에 CSR을 보내 새 인증서를 발급받는 것을 시뮬레이션
// 참조: secretcache.go의 generateNewSecret()
func (sc *SecretManagerClient) generateNewSecret(resourceName string) (*SecretItem, error) {
	// 워크로드 키 생성
	workloadKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("워크로드 키 생성 실패: %v", err)
	}

	now := time.Now()
	expireTime := now.Add(sc.certTTL)

	// 워크로드 인증서 템플릿 (SPIFFE 형식)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject: pkix.Name{
			Organization: []string{"Istio 워크로드"},
			CommonName:   fmt.Sprintf("spiffe://cluster.local/ns/default/sa/%s", sc.workloadID),
		},
		NotBefore: now,
		NotAfter:  expireTime,
		KeyUsage:  x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageClientAuth,
			x509.ExtKeyUsageServerAuth,
		},
	}

	// CA가 워크로드 인증서에 서명 (실제 Istio에서는 istiod CA가 CSR에 서명)
	certDER, err := x509.CreateCertificate(rand.Reader, template, sc.caCert, &workloadKey.PublicKey, sc.caKey)
	if err != nil {
		return nil, fmt.Errorf("인증서 서명 실패: %v", err)
	}

	// PEM 인코딩
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(workloadKey)})
	rootPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: sc.caCert.Raw})

	return &SecretItem{
		ResourceName:     resourceName,
		CertificateChain: certPEM,
		PrivateKey:       keyPEM,
		RootCert:         rootPEM,
		CreatedTime:      now,
		ExpireTime:       expireTime,
	}, nil
}

// registerSecret은 새 인증서를 캐시에 저장하고 갱신 작업을 스케줄링
// 참조: secretcache.go 라인 877-901
//
// 핵심 로직:
// 1. rotateTime()으로 갱신 대기 시간 계산
// 2. 이미 캐시된 인증서가 있으면 스킵 (중복 방지)
// 3. 캐시에 저장
// 4. delayed queue에 갱신 작업 푸시
// 5. 갱신 시: 캐시 초기화 후 OnSecretUpdate() 호출 → SDS push
func (sc *SecretManagerClient) registerSecret(item SecretItem) {
	delay := rotateTime(item, sc.gracePeriodRatio, sc.gracePeriodRatioJitter)
	item.ResourceName = "default" // WorkloadKeyCertResourceName

	// 중복 등록 방지 (실제 코드에서도 동일한 체크)
	if sc.cache.GetWorkload() != nil {
		sc.addEvent("갱신 스케줄링 스킵 - 이미 스케줄링됨")
		return
	}

	sc.cache.SetWorkload(&item)

	sc.addEvent(fmt.Sprintf("인증서 발급 완료 | 만료: %s | 갱신 예약: %v 후 (grace ratio: %.2f, jitter: %.3f)",
		item.ExpireTime.Format("15:04:05.000"),
		delay.Round(time.Millisecond),
		sc.gracePeriodRatio,
		sc.gracePeriodRatioJitter,
	))

	// 지연 큐에 갱신 작업 등록
	// 실제 코드: sc.queue.PushDelayed(func() error { ... }, delay)
	createdTime := item.CreatedTime // 클로저에서 사용할 복사본
	sc.queue.PushDelayed(func() error {
		// 오래된(stale) 스케줄된 작업인지 확인
		if cached := sc.cache.GetWorkload(); cached != nil {
			if cached.CreatedTime.Equal(createdTime) {
				sc.addEvent("갱신 트리거! 캐시 초기화 후 SDS push 전송")
				// 캐시 초기화 → 다음 GenerateSecret 호출 시 새 인증서 생성
				sc.cache.SetWorkload(nil)
				// SDS push 알림 → Envoy가 새 인증서 요청
				sc.OnSecretUpdate(item.ResourceName)
			} else {
				sc.addEvent("스케줄된 갱신 무시 - 이미 더 새로운 인증서가 있음")
			}
		}
		return nil
	}, delay)
}

// GenerateSecret은 SDS 요청에 대한 인증서 생성/반환
// 참조: secretcache.go 라인 248-323
func (sc *SecretManagerClient) GenerateSecret(resourceName string) (*SecretItem, error) {
	// 1. 캐시 확인
	if ns := sc.cache.GetWorkload(); ns != nil {
		sc.addEvent("캐시에서 인증서 반환")
		return ns, nil
	}

	// 2. 뮤텍스 획득 (동시 CSR 방지)
	sc.generateMutex.Lock()
	defer sc.generateMutex.Unlock()

	// 3. 뮤텍스 획득 후 다시 캐시 확인 (CA 과부하 방지)
	if ns := sc.cache.GetWorkload(); ns != nil {
		sc.addEvent("(뮤텍스 후) 캐시에서 인증서 반환")
		return ns, nil
	}

	// 4. 새 인증서 발급
	sc.addEvent("CA에 CSR 전송 → 새 인증서 발급 중...")
	ns, err := sc.generateNewSecret(resourceName)
	if err != nil {
		return nil, err
	}

	// 5. 캐시에 저장하고 갱신 스케줄링
	sc.registerSecret(*ns)

	return ns, nil
}

// ========================================================================
// 시뮬레이션 실행
// ========================================================================

func main() {
	fmt.Println("=" + strings.Repeat("=", 79))
	fmt.Println("  Istio 인증서 자동 갱신(Certificate Rotation) 시뮬레이션")
	fmt.Println("  참조: istio/security/pkg/nodeagent/cache/secretcache.go")
	fmt.Println("=" + strings.Repeat("=", 79))
	fmt.Println()

	// =================================================================
	// 1단계: rotateTime 알고리즘 데모
	// =================================================================
	fmt.Println("[1단계] rotateTime 알고리즘 분석")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println()

	// 인증서 수명 10초, gracePeriodRatio 0.5 → 만료 5초 전에 갱신
	demoSecret := SecretItem{
		CreatedTime: time.Now(),
		ExpireTime:  time.Now().Add(10 * time.Second),
	}

	fmt.Printf("인증서 수명: %v\n", demoSecret.ExpireTime.Sub(demoSecret.CreatedTime))
	fmt.Printf("gracePeriodRatio: 0.5 (인증서 수명의 50%%를 grace period로 사용)\n")
	fmt.Printf("gracePeriodRatioJitter: 0.01 (±1%%의 지터)\n")
	fmt.Println()

	// 여러 번 계산하여 지터 효과 확인
	fmt.Println("rotateTime 계산 결과 (지터에 의한 변동 확인):")
	for i := 0; i < 5; i++ {
		delay := rotateTime(demoSecret, 0.5, 0.01)
		gracePeriod := demoSecret.ExpireTime.Sub(demoSecret.CreatedTime) - delay
		fmt.Printf("  시행 %d: delay=%v (만료 %v 전에 갱신 시작)\n",
			i+1, delay.Round(time.Millisecond), gracePeriod.Round(time.Millisecond))
	}
	fmt.Println()

	// =================================================================
	// 2단계: 단일 워크로드 인증서 갱신 사이클
	// =================================================================
	fmt.Println("[2단계] 단일 워크로드 인증서 갱신 사이클")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println()

	// 짧은 TTL로 빠른 시뮬레이션 (3초 TTL, 50% grace → ~1.5초 후 갱신)
	certTTL := 3 * time.Second

	sc, err := NewSecretManagerClient("productpage-v1", certTTL, 0.5, 0.01)
	if err != nil {
		fmt.Printf("에러: %v\n", err)
		return
	}

	// SDS push 핸들러 등록
	rotationCount := 0
	var rotationMu sync.Mutex
	sc.RegisterSecretHandler(func(resourceName string) {
		rotationMu.Lock()
		rotationCount++
		count := rotationCount
		rotationMu.Unlock()

		sc.addEvent(fmt.Sprintf("SDS PUSH 수신! (갱신 #%d) → Envoy에 새 인증서 전달", count))

		// SDS push를 받은 Envoy가 새 인증서를 요청하는 것을 시뮬레이션
		go func() {
			time.Sleep(10 * time.Millisecond) // 약간의 지연
			_, _ = sc.GenerateSecret(resourceName)
		}()
	})

	// 최초 인증서 요청 (Envoy 시작 시)
	sc.addEvent("Envoy 시작 → 최초 인증서 요청")
	_, err = sc.GenerateSecret("default")
	if err != nil {
		fmt.Printf("에러: %v\n", err)
		return
	}

	// 갱신 사이클 관찰 (7초간 → 최소 2번 갱신 예상)
	time.Sleep(7 * time.Second)
	sc.Close()

	fmt.Println()

	// =================================================================
	// 3단계: 다중 워크로드 독립 갱신
	// =================================================================
	fmt.Println("[3단계] 다중 워크로드 독립 인증서 갱신")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println()

	type workloadInfo struct {
		id      string
		ttl     time.Duration
		grace   float64
		jitter  float64
	}

	workloads := []workloadInfo{
		{id: "reviews-v1", ttl: 2 * time.Second, grace: 0.5, jitter: 0.01},
		{id: "ratings-v1", ttl: 3 * time.Second, grace: 0.5, jitter: 0.01},
		{id: "details-v1", ttl: 4 * time.Second, grace: 0.5, jitter: 0.01},
	}

	var wg sync.WaitGroup
	clients := make([]*SecretManagerClient, len(workloads))

	for i, w := range workloads {
		client, err := NewSecretManagerClient(w.id, w.ttl, w.grace, w.jitter)
		if err != nil {
			fmt.Printf("에러: %v\n", err)
			return
		}
		clients[i] = client

		localClient := client
		localW := w

		client.RegisterSecretHandler(func(resourceName string) {
			localClient.addEvent(fmt.Sprintf("SDS PUSH (TTL=%v) → 새 인증서 요청", localW.ttl))
			go func() {
				time.Sleep(10 * time.Millisecond)
				_, _ = localClient.GenerateSecret(resourceName)
			}()
		})

		wg.Add(1)
		go func() {
			defer wg.Done()
			localClient.addEvent(fmt.Sprintf("워크로드 시작 (TTL=%v, grace=%.1f%%)", localW.ttl, localW.grace*100))
			_, _ = localClient.GenerateSecret("default")
		}()
	}

	// 6초간 관찰
	time.Sleep(6 * time.Second)

	for _, c := range clients {
		c.Close()
	}
	wg.Wait()

	fmt.Println()

	// =================================================================
	// 4단계: 타임라인 요약
	// =================================================================
	fmt.Println("[4단계] 인증서 갱신 타임라인 요약")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println()

	fmt.Println("인증서 수명 주기:")
	fmt.Println()
	fmt.Println("  시간 ──────────────────────────────────────────────────►")
	fmt.Println("  │")
	fmt.Println("  │  cert 발급          갱신 시점            만료")
	fmt.Println("  │  ├──────────────────┤──────────────────┤")
	fmt.Println("  │  │   사용 기간       │   grace period   │")
	fmt.Println("  │  │  (1-graceRatio)  │   (graceRatio)   │")
	fmt.Println("  │  │                  │                  │")
	fmt.Println("  │  ▼                  ▼                  ▼")
	fmt.Println("  │  CreatedTime     rotateTime         ExpireTime")
	fmt.Println()

	fmt.Println("갱신 프로세스:")
	fmt.Println()
	fmt.Println("  1. rotateTime 도달")
	fmt.Println("     │")
	fmt.Println("     ▼")
	fmt.Println("  2. DelayedQueue에서 갱신 작업 실행")
	fmt.Println("     │")
	fmt.Println("     ├── cache.SetWorkload(nil)     ← 캐시 초기화")
	fmt.Println("     │")
	fmt.Println("     ▼")
	fmt.Println("  3. OnSecretUpdate(\"default\")")
	fmt.Println("     │")
	fmt.Println("     ▼")
	fmt.Println("  4. secretHandler 콜백 실행         ← SDS push 트리거")
	fmt.Println("     │")
	fmt.Println("     ▼")
	fmt.Println("  5. Envoy가 GenerateSecret() 호출")
	fmt.Println("     │")
	fmt.Println("     ├── 캐시 미스 → generateNewSecret()")
	fmt.Println("     │")
	fmt.Println("     ▼")
	fmt.Println("  6. CA에 CSR → 새 인증서 발급")
	fmt.Println("     │")
	fmt.Println("     ├── registerSecret()            ← 다음 갱신 스케줄링")
	fmt.Println("     │")
	fmt.Println("     ▼")
	fmt.Println("  7. 새 인증서를 Envoy에 반환 → mTLS 연결 갱신")
	fmt.Println()

	fmt.Println("핵심 공식:")
	fmt.Printf("  delay = expireTime - gracePeriod - now\n")
	fmt.Printf("  gracePeriod = (graceRatio ± jitter) × secretLifeTime\n")
	fmt.Printf("  graceRatio = %.1f (기본값), jitter = %.2f (기본값)\n", 0.5, 0.01)
	fmt.Println()

	fmt.Println("=" + strings.Repeat("=", 79))
	fmt.Println("  시뮬레이션 완료")
	fmt.Println("=" + strings.Repeat("=", 79))
}
