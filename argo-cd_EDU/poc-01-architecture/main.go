// poc-01-architecture/main.go
//
// Argo CD 멀티 컴포넌트 아키텍처 시뮬레이션
//
// 핵심 개념:
//   - 단일 바이너리 디스패처 (os.Args[0] 기반 컴포넌트 선택)
//   - cmux 스타일 포트 멀티플렉싱 (gRPC vs HTTP 라우팅)
//   - 컴포넌트 간 통신: API Server ↔ Repo Server ↔ Application Controller
//   - K8s Secret 기반 저장소 시뮬레이션 (클러스터/레포 시크릿)
//   - 전체 흐름: Client → API Server → Repo Server → Controller → Target Cluster
//
// 실행: go run main.go

package main

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// 1. 단일 바이너리 디스패처
//    소스: cmd/main.go — os.Args[0] 또는 entrypoint 환경변수로 컴포넌트 선택
// =============================================================================

// componentEntry는 단일 바이너리 내 하나의 컴포넌트 진입점을 나타낸다.
type componentEntry struct {
	name        string
	description string
	run         func(ctx context.Context, bus *messageBus)
}

// dispatcher는 Argo CD의 단일 바이너리 패턴을 구현한다.
// 실제 Argo CD에서 argocd-server, argocd-application-controller, argocd-repo-server는
// 동일한 바이너리이며 ARGOCD_BINARY_NAME 환경변수 또는 argv[0]으로 역할이 결정된다.
func dispatcher(components map[string]componentEntry, entrypoint string) componentEntry {
	// 실제 소스: cmd/main.go
	//   switch binaryName {
	//   case "argocd-server":         return cmd.NewCommand()
	//   case "argocd-repo-server":    return reposerver.NewCommand()
	//   case "argocd-application-controller": return controller.NewCommand()
	//   ...
	//   }

	// ARGOCD_BINARY_NAME 환경변수 우선
	if env := os.Getenv("ARGOCD_BINARY_NAME"); env != "" {
		if c, ok := components[env]; ok {
			return c
		}
	}
	// argv[0] 기반 디스패치
	if c, ok := components[entrypoint]; ok {
		return c
	}
	// 기본값: argocd-server
	return components["argocd-server"]
}

// =============================================================================
// 2. cmux 스타일 포트 멀티플렉싱
//    소스: server/server.go — cmux.New(conn) 로 8080 포트에서 gRPC/HTTP 분리
// =============================================================================

// protocolType은 연결 프로토콜을 나타낸다.
type protocolType string

const (
	protoGRPC    protocolType = "gRPC"
	protoHTTP    protocolType = "HTTP/1.1"
	protoGRPCWeb protocolType = "gRPC-Web" // 브라우저 지원용
)

// connection은 클라이언트 연결을 시뮬레이션한다.
type connection struct {
	id       int
	proto    protocolType
	payload  string
	clientIP string
}

// cmuxRouter는 cmux 라이브러리의 포트 멀티플렉싱을 시뮬레이션한다.
// 실제 소스: server/server.go
//
//	tcpMux := cmux.New(conn)
//	grpcL := tcpMux.MatchWithWriters(cmux.HTTP2MatchHeaderFieldSendSettings("content-type", "application/grpc"))
//	httpL := tcpMux.Match(cmux.HTTP1Fast())
type cmuxRouter struct {
	port       int
	grpcConns  chan connection
	httpConns  chan connection
	mu         sync.Mutex
	connCount  int
}

func newCmuxRouter(port int) *cmuxRouter {
	return &cmuxRouter{
		port:      port,
		grpcConns: make(chan connection, 10),
		httpConns: make(chan connection, 10),
	}
}

// route는 프로토콜 헤더를 검사하여 gRPC 또는 HTTP로 라우팅한다.
// 실제 cmux는 Content-Type: application/grpc 헤더로 구분한다.
func (r *cmuxRouter) route(conn connection) string {
	r.mu.Lock()
	r.connCount++
	conn.id = r.connCount
	r.mu.Unlock()

	switch conn.proto {
	case protoGRPC, protoGRPCWeb:
		r.grpcConns <- conn
		return "gRPC"
	default:
		r.httpConns <- conn
		return "HTTP"
	}
}

// =============================================================================
// 3. 메시지 버스 (컴포넌트 간 통신 시뮬레이션)
//    실제: gRPC 채널로 연결 (argocd-server → argocd-repo-server :8081)
// =============================================================================

// messageType은 컴포넌트 간 메시지 타입이다.
type messageType string

const (
	msgManifestRequest  messageType = "ManifestRequest"
	msgManifestResponse messageType = "ManifestResponse"
	msgSyncRequest      messageType = "SyncRequest"
	msgSyncResult       messageType = "SyncResult"
	msgHealthCheck      messageType = "HealthCheck"
	msgStatusUpdate     messageType = "StatusUpdate"
)

// message는 컴포넌트 간 통신 메시지다.
type message struct {
	id       int
	msgType  messageType
	from     string
	to       string
	payload  map[string]string
	replyTo  chan message
}

// messageBus는 컴포넌트 간 비동기 통신을 시뮬레이션한다.
// 실제 Argo CD는 gRPC를 사용하지만, 개념적으로 동일한 요청/응답 패턴이다.
type messageBus struct {
	channels map[string]chan message
	mu       sync.RWMutex
	msgCount int
}

func newMessageBus() *messageBus {
	return &messageBus{
		channels: make(map[string]chan message),
	}
}

func (b *messageBus) register(component string) chan message {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch := make(chan message, 20)
	b.channels[component] = ch
	return ch
}

func (b *messageBus) send(msg message) {
	b.mu.Lock()
	b.msgCount++
	msg.id = b.msgCount
	b.mu.Unlock()

	b.mu.RLock()
	ch, ok := b.channels[msg.to]
	b.mu.RUnlock()

	if ok {
		ch <- msg
	}
}

func (b *messageBus) request(msg message) (message, error) {
	reply := make(chan message, 1)
	msg.replyTo = reply
	b.send(msg)

	select {
	case resp := <-reply:
		return resp, nil
	case <-time.After(2 * time.Second):
		return message{}, fmt.Errorf("timeout waiting for response from %s", msg.to)
	}
}

// =============================================================================
// 4. K8s Secret 기반 저장소 시뮬레이션
//    소스: util/db/cluster.go, util/db/repository.go
//    실제: Secret에 label argocd.argoproj.io/secret-type=cluster|repository 를 붙여 저장
// =============================================================================

// secretType은 Argo CD가 관리하는 K8s Secret의 타입이다.
type secretType string

const (
	secretTypeCluster    secretType = "cluster"
	secretTypeRepository secretType = "repository"
)

// k8sSecret은 K8s Secret을 시뮬레이션한다.
// 실제 레이블: argocd.argoproj.io/secret-type: cluster|repository
type k8sSecret struct {
	name        string
	namespace   string
	labels      map[string]string
	annotations map[string]string
	data        map[string]string // base64 decoded 값
}

// clusterSecret은 대상 클러스터 연결 정보를 담는 Secret이다.
// 소스: util/db/cluster.go — clusterToSecret()
//
//	secret.Data["server"] = []byte(cluster.Server)
//	secret.Data["config"] = []byte(configJSON)
type clusterSecret struct {
	k8sSecret
	server      string
	name        string
	bearerToken string
}

// repositorySecret은 Git/Helm 레포지토리 인증 정보를 담는 Secret이다.
// 소스: util/db/repository.go — repositoryToSecret()
type repositorySecret struct {
	k8sSecret
	repoURL  string
	username string
	password string // 실제는 암호화됨
	sshKey   string
}

// secretStore는 K8s etcd를 시뮬레이션하는 인메모리 저장소다.
type secretStore struct {
	secrets map[string]*k8sSecret
	mu      sync.RWMutex
}

func newSecretStore() *secretStore {
	return &secretStore{
		secrets: make(map[string]*k8sSecret),
	}
}

func (s *secretStore) put(secret *k8sSecret) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := secret.namespace + "/" + secret.name
	s.secrets[key] = secret
}

func (s *secretStore) listByLabel(labelKey, labelValue string) []*k8sSecret {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*k8sSecret
	for _, sec := range s.secrets {
		if v, ok := sec.labels[labelKey]; ok && v == labelValue {
			result = append(result, sec)
		}
	}
	return result
}

// =============================================================================
// 5. 컴포넌트 구현
// =============================================================================

// apiServer는 argocd-server 컴포넌트를 시뮬레이션한다.
// 소스: server/server.go — ArgoCDServer struct
//   - sessionMgr: 세션 관리
//   - enf: RBAC enforcer
//   - settingsMgr: 설정 관리
//   - db: 데이터베이스 레이어 (K8s Secret 기반)
func runAPIServer(ctx context.Context, bus *messageBus, store *secretStore) {
	inbox := bus.register("api-server")
	router := newCmuxRouter(8080)

	fmt.Println("[API Server] 시작 — 포트 :8080 (gRPC + HTTP/gRPC-Web 멀티플렉싱)")
	fmt.Printf("[API Server] cmux 라우터 초기화: 포트 %d\n", router.port)

	// 클러스터 시크릿 등록 (소스: util/db/cluster.go — CreateCluster)
	store.put(&k8sSecret{
		name:      "mycluster-secret",
		namespace: "argocd",
		labels: map[string]string{
			"argocd.argoproj.io/secret-type": string(secretTypeCluster),
		},
		data: map[string]string{
			"server": "https://production.k8s.local:6443",
			"name":   "production",
			"config": `{"bearerToken":"eyJhbGci...","tlsClientConfig":{"insecure":false}}`,
		},
	})

	// 레포지토리 시크릿 등록 (소스: util/db/repository.go — CreateRepository)
	store.put(&k8sSecret{
		name:      "myrepo-secret",
		namespace: "argocd",
		labels: map[string]string{
			"argocd.argoproj.io/secret-type": string(secretTypeRepository),
		},
		data: map[string]string{
			"url":      "https://github.com/myorg/myapp.git",
			"username": "git-user",
			"password": "<encrypted>",
		},
	})

	fmt.Println("[API Server] 클러스터/레포지토리 시크릿 등록 완료")

	// 인바운드 연결 처리
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case conn := <-router.grpcConns:
				fmt.Printf("[API Server] gRPC 연결 수신 [conn#%d] from %s: %q\n",
					conn.id, conn.clientIP, conn.payload)
				handleAPIRequest(ctx, bus, conn, store)
			case conn := <-router.httpConns:
				fmt.Printf("[API Server] HTTP 연결 수신 [conn#%d] from %s: %q\n",
					conn.id, conn.clientIP, conn.payload)
			case msg := <-inbox:
				fmt.Printf("[API Server] 메시지 수신 from %s: type=%s\n", msg.from, msg.msgType)
				if msg.replyTo != nil {
					msg.replyTo <- message{
						msgType: msgStatusUpdate,
						payload: map[string]string{"status": "ok"},
					}
				}
			}
		}
	}()

	// 클라이언트 연결 시뮬레이션
	time.Sleep(200 * time.Millisecond)
	connections := []connection{
		{proto: protoGRPC, payload: "ApplicationService.Create(myapp)", clientIP: "10.0.0.5"},
		{proto: protoHTTP, payload: "GET /api/v1/applications", clientIP: "10.0.0.10"},
		{proto: protoGRPCWeb, payload: "ApplicationService.Sync(myapp)", clientIP: "browser:8443"},
		{proto: protoGRPC, payload: "ApplicationService.Get(myapp)", clientIP: "10.0.0.5"},
	}
	for _, conn := range connections {
		proto := router.route(conn)
		fmt.Printf("[API Server] 라우팅: %-10s → %s 핸들러\n", conn.proto, proto)
		time.Sleep(50 * time.Millisecond)
	}
}

// handleAPIRequest는 API 서버가 수신한 gRPC 요청을 처리한다.
// 실제 흐름: API Server → Repo Server (매니페스트 조회) → Controller에 작업 위임
func handleAPIRequest(ctx context.Context, bus *messageBus, conn connection, store *secretStore) {
	if strings.Contains(conn.payload, "Create") || strings.Contains(conn.payload, "Sync") {
		// Repo Server에 매니페스트 생성 요청
		fmt.Printf("[API Server] → Repo Server: 매니페스트 생성 요청\n")
		resp, err := bus.request(message{
			msgType: msgManifestRequest,
			from:    "api-server",
			to:      "repo-server",
			payload: map[string]string{
				"repoURL":    "https://github.com/myorg/myapp.git",
				"revision":   "HEAD",
				"appPath":    "helm/myapp",
				"sourceType": "Helm",
			},
		})
		if err != nil {
			fmt.Printf("[API Server] Repo Server 오류: %v\n", err)
			return
		}
		fmt.Printf("[API Server] ← Repo Server: manifests=%s\n", resp.payload["manifestCount"])

		// Controller에 동기화 요청
		bus.send(message{
			msgType: msgSyncRequest,
			from:    "api-server",
			to:      "controller",
			payload: map[string]string{
				"app":       "myapp",
				"namespace": "production",
				"operation": "sync",
			},
		})
		fmt.Printf("[API Server] → Controller: 동기화 요청 전송\n")
	}
}

// repoServer는 argocd-repo-server 컴포넌트를 시뮬레이션한다.
// 소스: reposerver/server.go — Server struct
//   - 매니페스트 생성 (Helm, Kustomize, YAML, Plugin)
//   - Git 클론/페치
//   - 캐시를 통한 중복 생성 방지
func runRepoServer(ctx context.Context, bus *messageBus) {
	inbox := bus.register("repo-server")

	fmt.Println("[Repo Server] 시작 — 포트 :8081 (gRPC only)")
	fmt.Println("[Repo Server] 캐시 초기화 (Redis 연결 또는 인메모리)")

	// 간단한 캐시
	cache := make(map[string][]string)
	cacheMu := sync.Mutex{}

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case msg := <-inbox:
				if msg.msgType != msgManifestRequest {
					continue
				}

				repoURL := msg.payload["repoURL"]
				revision := msg.payload["revision"]
				sourceType := msg.payload["sourceType"]
				cacheKey := fmt.Sprintf("%s:%s", repoURL, revision)

				fmt.Printf("[Repo Server] 매니페스트 생성 요청: sourceType=%s, revision=%s\n",
					sourceType, revision)

				cacheMu.Lock()
				manifests, hit := cache[cacheKey]
				cacheMu.Unlock()

				if hit {
					fmt.Printf("[Repo Server] 캐시 HIT: %s\n", cacheKey)
				} else {
					// 실제: Git clone → 소스 타입 감지 → 도구 실행
					manifests = generateManifests(sourceType)
					cacheMu.Lock()
					cache[cacheKey] = manifests
					cacheMu.Unlock()
					fmt.Printf("[Repo Server] 매니페스트 생성 완료: %d 개 리소스\n", len(manifests))
				}

				if msg.replyTo != nil {
					msg.replyTo <- message{
						msgType: msgManifestResponse,
						from:    "repo-server",
						to:      msg.from,
						payload: map[string]string{
							"manifestCount": fmt.Sprintf("%d", len(manifests)),
							"revision":      "a1b2c3d",
						},
					}
				}
			}
		}
	}()
}

// generateManifests는 소스 타입에 따른 매니페스트 생성을 시뮬레이션한다.
func generateManifests(sourceType string) []string {
	switch sourceType {
	case "Helm":
		time.Sleep(80 * time.Millisecond) // Helm template 실행 시간
		return []string{
			"Deployment/myapp",
			"Service/myapp",
			"ConfigMap/myapp-config",
			"HorizontalPodAutoscaler/myapp",
		}
	case "Kustomize":
		time.Sleep(60 * time.Millisecond)
		return []string{
			"Deployment/myapp",
			"Service/myapp",
			"Kustomization/myapp",
		}
	default:
		time.Sleep(30 * time.Millisecond)
		return []string{"Deployment/myapp", "Service/myapp"}
	}
}

// applicationController는 argocd-application-controller 컴포넌트를 시뮬레이션한다.
// 소스: controller/appcontroller.go — ApplicationController struct
//   - appRefreshQueue: 앱 상태 갱신 워크큐
//   - appOperationQueue: sync 작업 워크큐
//   - K8s Informer로 Application/AppProject Watch
func runController(ctx context.Context, bus *messageBus, store *secretStore) {
	inbox := bus.register("controller")

	fmt.Println("[Controller] 시작 — Application Controller")
	fmt.Println("[Controller] K8s Informer 시작: Application, AppProject Watch")
	fmt.Println("[Controller] 워크큐 초기화: appRefreshQueue, appOperationQueue")

	// appRefreshQueue: 상태 갱신이 필요한 앱 목록
	appRefreshQueue := make(chan string, 100)
	// appOperationQueue: sync 등 작업이 필요한 앱 목록
	appOperationQueue := make(chan string, 100)

	// 앱 상태 저장
	appStatus := make(map[string]string)
	statusMu := sync.Mutex{}

	// Refresh 워커
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case appName := <-appRefreshQueue:
				statusMu.Lock()
				appStatus[appName] = "Syncing"
				statusMu.Unlock()

				// Repo Server에서 매니페스트를 받아 live state와 비교
				fmt.Printf("[Controller] appRefreshQueue 처리: app=%s → CompareAppState 실행\n", appName)
				time.Sleep(100 * time.Millisecond)

				statusMu.Lock()
				appStatus[appName] = "Synced"
				statusMu.Unlock()
				fmt.Printf("[Controller] 상태 업데이트: app=%s, status=Synced, health=Healthy\n", appName)
			}
		}
	}()

	// Operation 워커
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case appName := <-appOperationQueue:
				fmt.Printf("[Controller] appOperationQueue 처리: app=%s → Sync 실행\n", appName)

				// 대상 클러스터 조회 (K8s Secret에서)
				clusters := store.listByLabel("argocd.argoproj.io/secret-type", "cluster")
				if len(clusters) > 0 {
					server := clusters[0].data["server"]
					fmt.Printf("[Controller] 대상 클러스터: %s\n", server)
				}

				time.Sleep(150 * time.Millisecond)

				// 실제: kubectl apply를 통해 리소스 적용
				applyResources("myapp", []string{
					"Deployment/myapp",
					"Service/myapp",
					"ConfigMap/myapp-config",
				})

				statusMu.Lock()
				appStatus[appName] = "Healthy"
				statusMu.Unlock()

				// API Server에 결과 통보
				bus.send(message{
					msgType: msgSyncResult,
					from:    "controller",
					to:      "api-server",
					payload: map[string]string{
						"app":    appName,
						"result": "Succeeded",
					},
				})
				fmt.Printf("[Controller] → API Server: 동기화 결과 전송 (Succeeded)\n")
			}
		}
	}()

	// 수신 메시지 처리
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case msg := <-inbox:
				switch msg.msgType {
				case msgSyncRequest:
					appName := msg.payload["app"]
					fmt.Printf("[Controller] Sync 요청 수신: app=%s\n", appName)
					appRefreshQueue <- appName
					appOperationQueue <- appName
				}
			}
		}
	}()

	// 주기적 reconciliation (실제: 3분마다 전체 앱 재조정)
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		iteration := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				iteration++
				if iteration <= 2 {
					fmt.Printf("[Controller] 주기적 reconciliation #%d: 전체 앱 상태 검사\n", iteration)
				}
			}
		}
	}()
}

// applyResources는 대상 클러스터에 리소스를 적용하는 것을 시뮬레이션한다.
// 실제: controller/sync.go — sync.NewContext().runTasks()
func applyResources(appName string, resources []string) {
	for _, res := range resources {
		fmt.Printf("[Controller] apply → %s/%s\n", appName, res)
		time.Sleep(20 * time.Millisecond)
	}
}

// =============================================================================
// 6. 전체 흐름 시뮬레이션
// =============================================================================

func runFullFlow() {
	fmt.Println("=================================================================")
	fmt.Println(" Argo CD 멀티 컴포넌트 아키텍처 시뮬레이션")
	fmt.Println("=================================================================")
	fmt.Println()

	// 공유 인프라 초기화
	bus := newMessageBus()
	store := newSecretStore()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// --- 단일 바이너리 디스패처 시뮬레이션 ---
	fmt.Println("[ 1단계: 단일 바이너리 디스패처 ]")
	fmt.Println("-----------------------------------------------------------------")
	components := map[string]componentEntry{
		"argocd-server":                {name: "argocd-server", description: "API Server + UI"},
		"argocd-repo-server":           {name: "argocd-repo-server", description: "Repository Server"},
		"argocd-application-controller": {name: "argocd-application-controller", description: "GitOps Controller"},
		"argocd-dex-server":            {name: "argocd-dex-server", description: "OIDC Provider"},
	}
	for binary, entry := range components {
		fmt.Printf("  %-40s → %s\n", binary, entry.description)
	}

	entrypoint := dispatcher(components, "argocd-server")
	fmt.Printf("\n  현재 엔트리포인트: %s (%s)\n", entrypoint.name, entrypoint.description)
	fmt.Println()

	// --- 컴포넌트 시작 ---
	fmt.Println("[ 2단계: 컴포넌트 시작 ]")
	fmt.Println("-----------------------------------------------------------------")

	go runRepoServer(ctx, bus)
	time.Sleep(50 * time.Millisecond)
	go runController(ctx, bus, store)
	time.Sleep(50 * time.Millisecond)
	go runAPIServer(ctx, bus, store)
	time.Sleep(300 * time.Millisecond)

	// --- 전체 흐름 시뮬레이션 ---
	fmt.Println()
	fmt.Println("[ 3단계: Client → API Server → Repo Server → Controller → Cluster ]")
	fmt.Println("-----------------------------------------------------------------")

	// 클라이언트가 API Server에 App 생성 + Sync 요청
	router := newCmuxRouter(8080)
	conn := connection{
		proto:    protoGRPC,
		payload:  "ApplicationService.Create(myapp)",
		clientIP: "developer-laptop",
	}
	fmt.Printf("\n[Client] gRPC 요청 → argocd-server:8080\n")
	fmt.Printf("[Client] payload: %q\n", conn.payload)
	router.route(conn)
	time.Sleep(800 * time.Millisecond)

	// --- K8s Secret 저장소 상태 출력 ---
	fmt.Println()
	fmt.Println("[ 4단계: K8s Secret 저장소 (etcd 시뮬레이션) ]")
	fmt.Println("-----------------------------------------------------------------")

	clusters := store.listByLabel("argocd.argoproj.io/secret-type", "cluster")
	repos := store.listByLabel("argocd.argoproj.io/secret-type", "repository")

	fmt.Printf("  등록된 클러스터: %d개\n", len(clusters))
	for _, c := range clusters {
		fmt.Printf("    - %s → %s\n", c.name, c.data["server"])
	}
	fmt.Printf("  등록된 레포지토리: %d개\n", len(repos))
	for _, r := range repos {
		fmt.Printf("    - %s → %s\n", r.name, r.data["url"])
	}
	fmt.Printf("  메시지 버스 처리 건수: %d\n", bus.msgCount)

	// 종료 대기
	<-ctx.Done()
}

// =============================================================================
// 7. 아키텍처 요약 출력
// =============================================================================

func printArchitectureSummary() {
	fmt.Println()
	fmt.Println("=================================================================")
	fmt.Println(" 아키텍처 요약")
	fmt.Println("=================================================================")
	summary := `
  [ 단일 바이너리 패턴 ]
  argocd (하나의 바이너리)
    ├─ argocd-server                 :8080  gRPC + HTTP (cmux 멀티플렉싱)
    ├─ argocd-repo-server            :8081  gRPC only
    ├─ argocd-application-controller :8082  내부 메트릭만
    └─ argocd-dex-server             :5556  OIDC

  [ 포트 멀티플렉싱 (cmux) ]
  :8080 → Content-Type: application/grpc   → gRPC 핸들러
       → Content-Type: application/grpc-web → gRPC-Web 핸들러 (브라우저)
       → 그 외                              → HTTP 핸들러 (REST/UI)

  [ K8s Secret 기반 저장소 ]
  argocd 네임스페이스 Secret
    label: argocd.argoproj.io/secret-type=cluster    → 클러스터 연결 정보
    label: argocd.argoproj.io/secret-type=repository → Git/Helm 인증 정보

  [ 컴포넌트 통신 ]
  Client → API Server → Repo Server  (매니페스트 생성)
  Client → API Server → Controller   (Sync 작업)
  Controller → Target Cluster        (kubectl apply)
  Controller → API Server            (상태 업데이트)
`
	fmt.Print(summary)

	// 랜덤 트래픽 통계 (실제 Argo CD에는 Prometheus 메트릭 존재)
	fmt.Println("  [ 처리 통계 (시뮬레이션) ]")
	fmt.Printf("  gRPC 연결: %d, HTTP 연결: %d, 총 메시지: %d\n",
		2+rand.Intn(5), 1+rand.Intn(3), 10+rand.Intn(20))
}

func main() {
	runFullFlow()
	printArchitectureSummary()
}
