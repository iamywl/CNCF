package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Istio Credential 관리 + 헬스체크 프록시 시뮬레이션
//
// 실제 소스 참조:
//   - pilot/pkg/credentials/kube/secrets.go   → CredentialsController
//   - pilot/pkg/credentials/kube/multicluster.go → AggregateController
//   - pkg/istio-agent/health/health_probers.go    → HealthChecker
//   - pkg/istio-agent/health/health_check.go      → WorkloadHealthChecker
//
// 핵심 알고리즘:
//   1. Secret에서 TLS 인증서 추출 → CertInfo 구조체
//   2. SubjectAccessReview로 SA 권한 확인
//   3. AggregateController: 멀티클러스터 인증서 통합
//   4. pilot-agent가 readinessProbe를 대리 실행 → 결과를 xDS로 전달
// =============================================================================

// --- CertInfo: TLS 인증서 정보 ---

type CertInfo struct {
	Cert       string // PEM 인코딩 인증서
	Key        string // PEM 인코딩 개인키
	CACert     string // CA 인증서
	CRL        string // 인증서 폐기 목록
	SecretName string
	Namespace  string
}

func (ci CertInfo) String() string {
	certLen := len(ci.Cert)
	keyLen := len(ci.Key)
	return fmt.Sprintf("CertInfo{name=%s/%s, cert=%d bytes, key=%d bytes, ca=%d bytes}",
		ci.Namespace, ci.SecretName, certLen, keyLen, len(ci.CACert))
}

// --- Secret: Kubernetes Secret 시뮬레이션 ---

type SecretType string

const (
	SecretTypeTLS     SecretType = "kubernetes.io/tls"
	SecretTypeOpaque  SecretType = "Opaque"
	SecretTypeGeneric SecretType = "generic"
)

type Secret struct {
	Name      string
	Namespace string
	Type      SecretType
	Data      map[string][]byte
}

// --- CredentialsController: 인증서 관리 ---
// 실제: pilot/pkg/credentials/kube/secrets.go

type CredentialsController struct {
	mu      sync.RWMutex
	secrets map[string]*Secret // key: ns/name
	auths   map[string]bool    // SA 권한 캐시
}

func NewCredentialsController() *CredentialsController {
	return &CredentialsController{
		secrets: make(map[string]*Secret),
		auths:   make(map[string]bool),
	}
}

func (cc *CredentialsController) AddSecret(s *Secret) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	key := s.Namespace + "/" + s.Name
	cc.secrets[key] = s
}

// GetCertInfo는 Secret에서 TLS 인증서 정보를 추출한다.
// 실제: CredentialsController.GetCertInfo()
// kubernetes.io/tls → tls.crt, tls.key
// Opaque/generic → cert, key, cacert
func (cc *CredentialsController) GetCertInfo(name, namespace string) (*CertInfo, error) {
	cc.mu.RLock()
	defer cc.mu.RUnlock()

	key := namespace + "/" + name
	secret, ok := cc.secrets[key]
	if !ok {
		// 폴백: -cacert 접미사 제거 후 재시도
		if strings.HasSuffix(name, "-cacert") {
			baseName := strings.TrimSuffix(name, "-cacert")
			return cc.getCACert(baseName, namespace)
		}
		return nil, fmt.Errorf("secret %s not found", key)
	}

	switch secret.Type {
	case SecretTypeTLS:
		cert := string(secret.Data["tls.crt"])
		privKey := string(secret.Data["tls.key"])
		if cert == "" || privKey == "" {
			return nil, fmt.Errorf("secret %s missing tls.crt or tls.key", key)
		}
		return &CertInfo{
			Cert:       cert,
			Key:        privKey,
			CACert:     string(secret.Data["ca.crt"]),
			SecretName: name,
			Namespace:  namespace,
		}, nil

	case SecretTypeOpaque, SecretTypeGeneric:
		cert := string(secret.Data["cert"])
		privKey := string(secret.Data["key"])
		if cert == "" || privKey == "" {
			return nil, fmt.Errorf("secret %s missing cert or key", key)
		}
		return &CertInfo{
			Cert:       cert,
			Key:        privKey,
			CACert:     string(secret.Data["cacert"]),
			SecretName: name,
			Namespace:  namespace,
		}, nil
	}

	return nil, fmt.Errorf("unsupported secret type: %s", secret.Type)
}

func (cc *CredentialsController) getCACert(name, namespace string) (*CertInfo, error) {
	key := namespace + "/" + name
	secret, ok := cc.secrets[key]
	if !ok {
		return nil, fmt.Errorf("CA secret %s not found", key)
	}
	caCert := string(secret.Data["ca.crt"])
	if caCert == "" {
		caCert = string(secret.Data["cacert"])
	}
	if caCert == "" {
		return nil, fmt.Errorf("CA cert not found in secret %s", key)
	}
	return &CertInfo{
		CACert:     caCert,
		CRL:        string(secret.Data["ca.crl"]),
		SecretName: name,
		Namespace:  namespace,
	}, nil
}

// Authorize는 ServiceAccount가 Secret에 접근할 수 있는지 확인한다.
// 실제: SubjectAccessReview API를 사용
func (cc *CredentialsController) Authorize(sa, namespace, secretName string) bool {
	cc.mu.RLock()
	defer cc.mu.RUnlock()

	// 같은 네임스페이스의 SA만 허용 (기본 정책)
	authKey := fmt.Sprintf("%s/%s→%s/%s", namespace, sa, namespace, secretName)
	if allowed, cached := cc.auths[authKey]; cached {
		return allowed
	}
	return namespace == namespace // 기본: 같은 NS면 허용
}

func (cc *CredentialsController) SetAuth(sa, namespace, secretName string, allowed bool) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	authKey := fmt.Sprintf("%s/%s→%s/%s", namespace, sa, namespace, secretName)
	cc.auths[authKey] = allowed
}

// --- AggregateController: 멀티클러스터 인증서 통합 ---
// 실제: pilot/pkg/credentials/kube/multicluster.go

type AggregateController struct {
	mu          sync.RWMutex
	controllers map[string]*CredentialsController // clusterID → controller
}

func NewAggregateController() *AggregateController {
	return &AggregateController{
		controllers: make(map[string]*CredentialsController),
	}
}

func (ac *AggregateController) AddCluster(clusterID string, ctrl *CredentialsController) {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	ac.controllers[clusterID] = ctrl
	fmt.Printf("  [AggregateCtrl] 클러스터 추가: %s\n", clusterID)
}

// GetCertInfo는 모든 클러스터에서 인증서를 조회한다.
func (ac *AggregateController) GetCertInfo(name, namespace string) (*CertInfo, string, error) {
	ac.mu.RLock()
	defer ac.mu.RUnlock()

	for clusterID, ctrl := range ac.controllers {
		cert, err := ctrl.GetCertInfo(name, namespace)
		if err == nil {
			return cert, clusterID, nil
		}
	}
	return nil, "", fmt.Errorf("cert %s/%s not found in any cluster", namespace, name)
}

// --- Health Check Prober ---
// 실제: pkg/istio-agent/health/health_probers.go

type ProbeType string

const (
	ProbeHTTP ProbeType = "HTTP"
	ProbeTCP  ProbeType = "TCP"
	ProbeExec ProbeType = "Exec"
)

type ProbeConfig struct {
	Type            ProbeType
	Path            string // HTTP path
	Port            int
	TimeoutMs       int64
	PeriodMs        int64
	SuccessThreshold int
	FailureThreshold int
}

type ProbeResult struct {
	Healthy   bool
	Message   string
	Timestamp time.Time
	Latency   time.Duration
}

// HealthProber는 프로브를 실행한다.
type HealthProber struct {
	config  ProbeConfig
	results []ProbeResult
	mu      sync.Mutex
}

func NewHealthProber(config ProbeConfig) *HealthProber {
	return &HealthProber{config: config}
}

func (hp *HealthProber) Probe(addr string) ProbeResult {
	start := time.Now()
	var result ProbeResult
	result.Timestamp = start

	switch hp.config.Type {
	case ProbeHTTP:
		result = hp.probeHTTP(addr)
	case ProbeTCP:
		result = hp.probeTCP(addr)
	default:
		result = ProbeResult{Healthy: false, Message: "unsupported probe type"}
	}

	result.Latency = time.Since(start)
	hp.mu.Lock()
	hp.results = append(hp.results, result)
	hp.mu.Unlock()
	return result
}

func (hp *HealthProber) probeHTTP(addr string) ProbeResult {
	url := fmt.Sprintf("http://%s:%d%s", addr, hp.config.Port, hp.config.Path)
	client := &http.Client{
		Timeout: time.Duration(hp.config.TimeoutMs) * time.Millisecond,
	}
	resp, err := client.Get(url)
	if err != nil {
		return ProbeResult{Healthy: false, Message: fmt.Sprintf("HTTP probe failed: %v", err)}
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		return ProbeResult{Healthy: true, Message: fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
	return ProbeResult{Healthy: false, Message: fmt.Sprintf("HTTP %d", resp.StatusCode)}
}

func (hp *HealthProber) probeTCP(addr string) ProbeResult {
	target := fmt.Sprintf("%s:%d", addr, hp.config.Port)
	conn, err := net.DialTimeout("tcp", target, time.Duration(hp.config.TimeoutMs)*time.Millisecond)
	if err != nil {
		return ProbeResult{Healthy: false, Message: fmt.Sprintf("TCP probe failed: %v", err)}
	}
	conn.Close()
	return ProbeResult{Healthy: true, Message: "TCP connection successful"}
}

// --- WorkloadHealthChecker: 워크로드 헬스체크 관리자 ---
// 실제: pkg/istio-agent/health/health_check.go

type HealthEvent struct {
	Healthy bool
	Message string
}

type WorkloadHealthChecker struct {
	prober           *HealthProber
	addr             string
	consecutiveOK    int
	consecutiveFail  int
	lastHealthy      *bool
	eventCh          chan HealthEvent
}

func NewWorkloadHealthChecker(config ProbeConfig, addr string) *WorkloadHealthChecker {
	return &WorkloadHealthChecker{
		prober:  NewHealthProber(config),
		addr:    addr,
		eventCh: make(chan HealthEvent, 10),
	}
}

func (whc *WorkloadHealthChecker) Run(stop <-chan struct{}) <-chan HealthEvent {
	go func() {
		ticker := time.NewTicker(time.Duration(whc.prober.config.PeriodMs) * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				close(whc.eventCh)
				return
			case <-ticker.C:
				result := whc.prober.Probe(whc.addr)
				whc.processResult(result)
			}
		}
	}()
	return whc.eventCh
}

func (whc *WorkloadHealthChecker) processResult(result ProbeResult) {
	if result.Healthy {
		whc.consecutiveOK++
		whc.consecutiveFail = 0
		if whc.consecutiveOK >= whc.prober.config.SuccessThreshold {
			if whc.lastHealthy == nil || !*whc.lastHealthy {
				healthy := true
				whc.lastHealthy = &healthy
				whc.eventCh <- HealthEvent{
					Healthy: true,
					Message: fmt.Sprintf("Healthy after %d consecutive successes: %s",
						whc.consecutiveOK, result.Message),
				}
			}
		}
	} else {
		whc.consecutiveFail++
		whc.consecutiveOK = 0
		if whc.consecutiveFail >= whc.prober.config.FailureThreshold {
			if whc.lastHealthy == nil || *whc.lastHealthy {
				healthy := false
				whc.lastHealthy = &healthy
				whc.eventCh <- HealthEvent{
					Healthy: false,
					Message: fmt.Sprintf("Unhealthy after %d consecutive failures: %s",
						whc.consecutiveFail, result.Message),
				}
			}
		}
	}
}

// --- 시뮬레이션 헬퍼 ---

func generateFakeCert() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(1000000))
	hash := sha256.Sum256([]byte(fmt.Sprintf("cert-%d", n)))
	return "-----BEGIN CERTIFICATE-----\n" + hex.EncodeToString(hash[:]) + "\n-----END CERTIFICATE-----"
}

func generateFakeKey() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(1000000))
	hash := sha256.Sum256([]byte(fmt.Sprintf("key-%d", n)))
	return "-----BEGIN PRIVATE KEY-----\n" + hex.EncodeToString(hash[:]) + "\n-----END PRIVATE KEY-----"
}

// --- 메인 함수 ---

func main() {
	fmt.Println("=== Istio Credential 관리 + 헬스체크 프록시 시뮬레이션 ===")
	fmt.Println()

	// 1. CredentialsController: Secret에서 인증서 추출
	fmt.Println("--- 1단계: CredentialsController - Secret에서 인증서 추출 ---")
	ctrl := NewCredentialsController()

	// TLS Secret 추가
	ctrl.AddSecret(&Secret{
		Name: "app-tls", Namespace: "default",
		Type: SecretTypeTLS,
		Data: map[string][]byte{
			"tls.crt": []byte(generateFakeCert()),
			"tls.key": []byte(generateFakeKey()),
			"ca.crt":  []byte(generateFakeCert()),
		},
	})

	// Opaque Secret 추가
	ctrl.AddSecret(&Secret{
		Name: "gateway-cert", Namespace: "istio-system",
		Type: SecretTypeOpaque,
		Data: map[string][]byte{
			"cert":   []byte(generateFakeCert()),
			"key":    []byte(generateFakeKey()),
			"cacert": []byte(generateFakeCert()),
		},
	})

	// TLS Secret 조회
	cert1, err := ctrl.GetCertInfo("app-tls", "default")
	if err != nil {
		fmt.Printf("  오류: %v\n", err)
	} else {
		fmt.Printf("  TLS Secret 조회 성공: %s\n", cert1)
	}

	// Opaque Secret 조회
	cert2, err := ctrl.GetCertInfo("gateway-cert", "istio-system")
	if err != nil {
		fmt.Printf("  오류: %v\n", err)
	} else {
		fmt.Printf("  Opaque Secret 조회 성공: %s\n", cert2)
	}

	// 존재하지 않는 Secret 조회
	_, err = ctrl.GetCertInfo("nonexist", "default")
	fmt.Printf("  존재하지 않는 Secret: %v\n", err)

	// 2. 멀티클러스터 인증서 통합
	fmt.Println()
	fmt.Println("--- 2단계: AggregateController - 멀티클러스터 인증서 통합 ---")

	cluster2Ctrl := NewCredentialsController()
	cluster2Ctrl.AddSecret(&Secret{
		Name: "remote-cert", Namespace: "default",
		Type: SecretTypeTLS,
		Data: map[string][]byte{
			"tls.crt": []byte(generateFakeCert()),
			"tls.key": []byte(generateFakeKey()),
		},
	})

	aggregate := NewAggregateController()
	aggregate.AddCluster("cluster-1", ctrl)
	aggregate.AddCluster("cluster-2", cluster2Ctrl)

	// 클러스터 1에만 있는 인증서
	cert, clusterID, err := aggregate.GetCertInfo("app-tls", "default")
	if err == nil {
		fmt.Printf("  app-tls 발견: %s (클러스터: %s)\n", cert, clusterID)
	}

	// 클러스터 2에만 있는 인증서
	cert, clusterID, err = aggregate.GetCertInfo("remote-cert", "default")
	if err == nil {
		fmt.Printf("  remote-cert 발견: %s (클러스터: %s)\n", cert, clusterID)
	}

	// 3. 권한 검증 (SubjectAccessReview 시뮬레이션)
	fmt.Println()
	fmt.Println("--- 3단계: 권한 검증 (SubjectAccessReview) ---")

	ctrl.SetAuth("app-sa", "default", "app-tls", true)
	ctrl.SetAuth("malicious-sa", "hacker-ns", "app-tls", false)

	fmt.Printf("  app-sa → app-tls: %v\n", ctrl.Authorize("app-sa", "default", "app-tls"))
	fmt.Printf("  malicious-sa → app-tls: %v\n", ctrl.Authorize("malicious-sa", "hacker-ns", "app-tls"))

	// 4. 헬스체크 프록시 시뮬레이션
	fmt.Println()
	fmt.Println("--- 4단계: 헬스체크 프록시 ---")

	// 간단한 HTTP 서버 시작 (애플리케이션 시뮬레이션)
	mux := http.NewServeMux()
	probeCount := 0
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		probeCount++
		if probeCount <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Printf("  서버 시작 실패: %v\n", err)
		return
	}
	port := listener.Addr().(*net.TCPAddr).Port
	server := &http.Server{Handler: mux}
	go server.Serve(listener)
	defer server.Close()

	fmt.Printf("  애플리케이션 서버 시작: 127.0.0.1:%d\n", port)
	fmt.Println("  처음 2회는 503 반환, 이후 200 반환")

	// WorkloadHealthChecker 시작
	checker := NewWorkloadHealthChecker(ProbeConfig{
		Type:             ProbeHTTP,
		Path:             "/healthz",
		Port:             port,
		TimeoutMs:        500,
		PeriodMs:         50,
		SuccessThreshold: 1,
		FailureThreshold: 2,
	}, "127.0.0.1")

	stop := make(chan struct{})
	events := checker.Run(stop)

	// 헬스 이벤트 수집
	timeout := time.After(500 * time.Millisecond)
	for {
		select {
		case event, ok := <-events:
			if !ok {
				goto done
			}
			status := "UNHEALTHY"
			if event.Healthy {
				status = "HEALTHY"
			}
			fmt.Printf("  [HealthEvent] %s: %s\n", status, event.Message)
		case <-timeout:
			goto done
		}
	}
done:
	close(stop)

	// 5. TCP 프로브 시뮬레이션
	fmt.Println()
	fmt.Println("--- 5단계: TCP 프로브 ---")

	tcpProber := NewHealthProber(ProbeConfig{
		Type:      ProbeTCP,
		Port:      port,
		TimeoutMs: 200,
	})

	// 열려 있는 포트
	result := tcpProber.Probe("127.0.0.1")
	fmt.Printf("  TCP 프로브 (포트 %d): healthy=%v, msg=%s\n", port, result.Healthy, result.Message)

	// 닫힌 포트
	result = tcpProber.Probe("127.0.0.1")
	tcpProber2 := NewHealthProber(ProbeConfig{Type: ProbeTCP, Port: 19999, TimeoutMs: 100})
	result = tcpProber2.Probe("127.0.0.1")
	fmt.Printf("  TCP 프로브 (포트 19999): healthy=%v, msg=%s\n", result.Healthy, result.Message)

	// 요약
	fmt.Println()
	fmt.Println("=== 요약 ===")
	fmt.Println("  - CredentialsController: kubernetes.io/tls + Opaque Secret에서 인증서 추출")
	fmt.Println("  - AggregateController: 멀티클러스터 인증서 통합 조회")
	fmt.Println("  - SubjectAccessReview: SA 기반 Secret 접근 권한 검증")
	fmt.Println("  - WorkloadHealthChecker: HTTP/TCP 프로브 실행 + 연속 성공/실패 임계값")
	fmt.Println("  - 헬스 이벤트: 상태 변경 시만 이벤트 발생 (플래핑 방지)")

	_ = strings.Join(nil, "")
}
