// poc-01-architecture: 쿠버네티스 컴포넌트 통신 패턴 시뮬레이션
//
// 쿠버네티스의 핵심 아키텍처 원칙인 "허브 앤 스포크(Hub-and-Spoke)" 패턴을 구현한다.
// 모든 컴포넌트(Controller, Scheduler, Kubelet)는 오직 API Server를 통해서만 통신하며,
// 서로 직접 통신하지 않는다. 이는 실제 쿠버네티스의 pkg/kubelet/kubelet.go,
// pkg/scheduler/scheduler.go, pkg/controller/replicaset/replica_set.go에서
// 볼 수 있는 패턴을 재현한 것이다.
//
// 실행: go run main.go
package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// 데이터 모델 — K8s의 TypeMeta + ObjectMeta + Spec + Status 패턴 축소판
// 실제 소스: staging/src/k8s.io/api/core/v1/types.go
// ============================================================================

// PodPhase는 파드의 생명주기 단계를 나타낸다.
type PodPhase string

const (
	PodPending   PodPhase = "Pending"
	PodRunning   PodPhase = "Running"
	PodSucceeded PodPhase = "Succeeded"
	PodFailed    PodPhase = "Failed"
)

// Pod는 쿠버네티스의 최소 배포 단위이다.
type Pod struct {
	Name      string            `json:"name"`
	Namespace string            `json:"namespace"`
	Labels    map[string]string `json:"labels,omitempty"`
	// Spec
	NodeName   string `json:"nodeName,omitempty"`   // 스케줄러가 배정
	Containers []string `json:"containers"`          // 컨테이너 이미지 목록
	// Status
	Phase           PodPhase `json:"phase"`
	ResourceVersion int64    `json:"resourceVersion"`
}

// WatchEvent는 API Server의 watch 메커니즘에서 전달되는 이벤트이다.
// 실제 소스: staging/src/k8s.io/apimachinery/pkg/watch/watch.go
type WatchEvent struct {
	Type string `json:"type"` // ADDED, MODIFIED, DELETED
	Pod  Pod    `json:"pod"`
}

// ============================================================================
// API Server — 중앙 허브. 모든 상태를 저장하고, watch를 통해 변경사항을 전파한다.
// 실제 소스: staging/src/k8s.io/apiserver/pkg/server/genericapiserver.go
// ============================================================================

// APIServer는 etcd 역할(인메모리)과 REST API를 동시에 담당한다.
type APIServer struct {
	mu              sync.RWMutex
	pods            map[string]*Pod        // namespace/name → Pod (etcd 역할)
	resourceVersion int64                  // 단조 증가 버전 카운터
	watchers        []chan WatchEvent       // watch 구독자 목록
	watcherMu       sync.Mutex
}

// NewAPIServer는 API Server를 초기화한다.
func NewAPIServer() *APIServer {
	return &APIServer{
		pods:     make(map[string]*Pod),
		watchers: make([]chan WatchEvent, 0),
	}
}

// podKey는 네임스페이스/이름으로 고유 키를 생성한다.
func podKey(namespace, name string) string {
	return namespace + "/" + name
}

// CreatePod는 파드를 생성하고 watch 이벤트를 발행한다.
func (s *APIServer) CreatePod(pod *Pod) (*Pod, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := podKey(pod.Namespace, pod.Name)
	if _, exists := s.pods[key]; exists {
		return nil, fmt.Errorf("파드 %s 이미 존재", key)
	}

	s.resourceVersion++
	pod.ResourceVersion = s.resourceVersion
	if pod.Phase == "" {
		pod.Phase = PodPending
	}
	stored := *pod
	s.pods[key] = &stored

	s.notifyWatchers(WatchEvent{Type: "ADDED", Pod: stored})
	return &stored, nil
}

// UpdatePod는 파드를 업데이트한다. 낙관적 동시성 제어(optimistic concurrency)를 적용한다.
// 실제 K8s에서는 ResourceVersion이 일치하지 않으면 409 Conflict를 반환한다.
func (s *APIServer) UpdatePod(pod *Pod) (*Pod, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := podKey(pod.Namespace, pod.Name)
	existing, exists := s.pods[key]
	if !exists {
		return nil, fmt.Errorf("파드 %s 없음", key)
	}

	// 낙관적 동시성: 클라이언트가 보내온 resourceVersion이 현재와 일치해야 업데이트 가능
	if pod.ResourceVersion != 0 && pod.ResourceVersion != existing.ResourceVersion {
		return nil, fmt.Errorf("충돌: resourceVersion 불일치 (요청=%d, 현재=%d)", pod.ResourceVersion, existing.ResourceVersion)
	}

	s.resourceVersion++
	pod.ResourceVersion = s.resourceVersion
	stored := *pod
	s.pods[key] = &stored

	s.notifyWatchers(WatchEvent{Type: "MODIFIED", Pod: stored})
	return &stored, nil
}

// GetPod는 파드를 조회한다.
func (s *APIServer) GetPod(namespace, name string) (*Pod, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	pod, ok := s.pods[podKey(namespace, name)]
	if ok {
		cp := *pod
		return &cp, true
	}
	return nil, false
}

// ListPods는 모든 파드를 반환한다.
func (s *APIServer) ListPods() []Pod {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]Pod, 0, len(s.pods))
	for _, p := range s.pods {
		result = append(result, *p)
	}
	return result
}

// Watch는 변경사항을 구독하기 위한 채널을 반환한다.
// 실제 소스: staging/src/k8s.io/apiserver/pkg/storage/etcd3/watcher.go
func (s *APIServer) Watch() <-chan WatchEvent {
	ch := make(chan WatchEvent, 100)
	s.watcherMu.Lock()
	s.watchers = append(s.watchers, ch)
	s.watcherMu.Unlock()
	return ch
}

// notifyWatchers는 모든 구독자에게 이벤트를 전달한다.
func (s *APIServer) notifyWatchers(event WatchEvent) {
	s.watcherMu.Lock()
	defer s.watcherMu.Unlock()
	for _, ch := range s.watchers {
		select {
		case ch <- event:
		default:
			// 버퍼 초과 시 이벤트 드롭 (실제 K8s에서는 bookmark/resync로 복구)
		}
	}
}

// ============================================================================
// HTTP 핸들러 — API Server의 RESTful 인터페이스
// ============================================================================

func (s *APIServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch {
	case r.Method == "GET" && r.URL.Path == "/api/v1/pods":
		pods := s.ListPods()
		json.NewEncoder(w).Encode(pods)

	case r.Method == "POST" && r.URL.Path == "/api/v1/pods":
		var pod Pod
		if err := json.NewDecoder(r.Body).Decode(&pod); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if pod.Namespace == "" {
			pod.Namespace = "default"
		}
		created, err := s.CreatePod(&pod)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(created)

	case r.Method == "PUT" && strings.HasPrefix(r.URL.Path, "/api/v1/pods/"):
		var pod Pod
		if err := json.NewDecoder(r.Body).Decode(&pod); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		updated, err := s.UpdatePod(&pod)
		if err != nil {
			if strings.Contains(err.Error(), "충돌") {
				http.Error(w, err.Error(), http.StatusConflict)
			} else {
				http.Error(w, err.Error(), http.StatusNotFound)
			}
			return
		}
		json.NewEncoder(w).Encode(updated)

	default:
		http.Error(w, "Not Found", http.StatusNotFound)
	}
}

// ============================================================================
// Scheduler — 미배정 파드를 감시하고 노드를 배정한다.
// 실제 소스: pkg/scheduler/scheduler.go의 scheduleOne 함수 참조
// ============================================================================

type Scheduler struct {
	apiServer *APIServer
	nodes     []string
}

func NewScheduler(api *APIServer, nodes []string) *Scheduler {
	return &Scheduler{apiServer: api, nodes: nodes}
}

// Run은 watch를 통해 미배정 파드를 감시하고 스케줄링한다.
func (s *Scheduler) Run(stopCh <-chan struct{}) {
	fmt.Println("[스케줄러] 시작 — 미배정 파드 감시 중...")
	watchCh := s.apiServer.Watch()

	for {
		select {
		case event := <-watchCh:
			// 새로 추가되었거나 업데이트된 파드 중 노드 미배정인 것만 스케줄링
			if event.Type == "ADDED" || event.Type == "MODIFIED" {
				if event.Pod.NodeName == "" && event.Pod.Phase == PodPending {
					s.schedulePod(event.Pod)
				}
			}
		case <-stopCh:
			fmt.Println("[스케줄러] 종료")
			return
		}
	}
}

// schedulePod는 노드를 선택하고 파드를 바인딩한다.
// 실제 K8s에서는 Filter → Score → Reserve → Bind 단계를 거친다.
func (s *Scheduler) schedulePod(pod Pod) {
	// 간단한 라운드 로빈: 실제로는 Filter/Score 플러그인 체인을 거침
	selectedNode := s.nodes[rand.Intn(len(s.nodes))]

	fmt.Printf("[스케줄러] 파드 %s/%s → 노드 %s 배정\n", pod.Namespace, pod.Name, selectedNode)

	pod.NodeName = selectedNode
	_, err := s.apiServer.UpdatePod(&pod)
	if err != nil {
		fmt.Printf("[스케줄러] 파드 %s/%s 배정 실패: %v\n", pod.Namespace, pod.Name, err)
	}
}

// ============================================================================
// Controller (ReplicaSet Controller 유사) — 원하는 상태 ↔ 현재 상태를 조정한다.
// 실제 소스: pkg/controller/replicaset/replica_set.go의 syncReplicaSet 함수 참조
// ============================================================================

type ReplicaSetController struct {
	apiServer    *APIServer
	desiredCount int
	appLabel     string
}

func NewReplicaSetController(api *APIServer, appLabel string, desiredCount int) *ReplicaSetController {
	return &ReplicaSetController{
		apiServer:    api,
		desiredCount: desiredCount,
		appLabel:     appLabel,
	}
}

// Run은 주기적으로 원하는 파드 수와 실제 파드 수를 비교하여 조정한다.
func (c *ReplicaSetController) Run(stopCh <-chan struct{}) {
	fmt.Printf("[컨트롤러] 시작 — app=%s, 원하는 복제본=%d\n", c.appLabel, c.desiredCount)

	// 초기 조정 — 파드가 없으면 생성
	c.reconcile()

	watchCh := c.apiServer.Watch()
	for {
		select {
		case <-watchCh:
			// 어떤 이벤트가 오든 reconcile 실행 (실제로는 WorkQueue를 거침)
			c.reconcile()
		case <-stopCh:
			fmt.Println("[컨트롤러] 종료")
			return
		}
	}
}

// reconcile은 선언적 조정 루프의 핵심이다.
// desired - actual > 0 이면 파드 생성, < 0 이면 삭제
func (c *ReplicaSetController) reconcile() {
	pods := c.apiServer.ListPods()

	// 라벨 셀렉터로 매칭되는 파드만 카운트
	var matching []Pod
	for _, p := range pods {
		if p.Labels["app"] == c.appLabel {
			matching = append(matching, p)
		}
	}

	current := len(matching)
	diff := c.desiredCount - current

	if diff > 0 {
		fmt.Printf("[컨트롤러] 조정: 현재=%d, 원하는=%d → %d개 파드 생성\n", current, c.desiredCount, diff)
		for i := 0; i < diff; i++ {
			pod := &Pod{
				Name:       fmt.Sprintf("%s-%s", c.appLabel, randomSuffix()),
				Namespace:  "default",
				Labels:     map[string]string{"app": c.appLabel},
				Containers: []string{"nginx:1.25"},
				Phase:      PodPending,
			}
			_, err := c.apiServer.CreatePod(pod)
			if err != nil {
				fmt.Printf("[컨트롤러] 파드 생성 실패: %v\n", err)
			}
		}
	} else if diff < 0 {
		fmt.Printf("[컨트롤러] 조정: 현재=%d, 원하는=%d → %d개 파드 제거 필요\n", current, c.desiredCount, -diff)
	}
}

// ============================================================================
// Kubelet — 노드에 배정된 파드를 실행한다.
// 실제 소스: pkg/kubelet/kubelet.go의 syncLoop/syncLoopIteration 함수 참조
// ============================================================================

type Kubelet struct {
	apiServer *APIServer
	nodeName  string
}

func NewKubelet(api *APIServer, nodeName string) *Kubelet {
	return &Kubelet{apiServer: api, nodeName: nodeName}
}

// Run은 watch를 통해 자기 노드에 배정된 파드를 감시하고 상태를 업데이트한다.
func (k *Kubelet) Run(stopCh <-chan struct{}) {
	fmt.Printf("[kubelet-%s] 시작 — 배정된 파드 감시 중...\n", k.nodeName)
	watchCh := k.apiServer.Watch()

	for {
		select {
		case event := <-watchCh:
			if event.Pod.NodeName == k.nodeName && event.Pod.Phase != PodRunning {
				// 자기 노드에 배정된 파드가 아직 Running이 아니면 실행
				k.syncPod(event.Pod)
			}
		case <-stopCh:
			fmt.Printf("[kubelet-%s] 종료\n", k.nodeName)
			return
		}
	}
}

// syncPod는 파드를 "실행"하고 상태를 Running으로 업데이트한다.
// 실제로는 CRI를 통해 컨테이너 런타임을 호출한다.
func (k *Kubelet) syncPod(pod Pod) {
	fmt.Printf("[kubelet-%s] 파드 %s/%s 실행 시작 (컨테이너: %v)\n",
		k.nodeName, pod.Namespace, pod.Name, pod.Containers)

	// 컨테이너 시작 시뮬레이션 (실제로는 containerd/CRI-O 호출)
	time.Sleep(100 * time.Millisecond)

	pod.Phase = PodRunning
	_, err := k.apiServer.UpdatePod(&pod)
	if err != nil {
		fmt.Printf("[kubelet-%s] 상태 업데이트 실패: %v\n", k.nodeName, err)
	} else {
		fmt.Printf("[kubelet-%s] 파드 %s/%s → Running\n", k.nodeName, pod.Namespace, pod.Name)
	}
}

// ============================================================================
// 유틸리티
// ============================================================================

func randomSuffix() string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 5)
	for i := range b {
		b[i] = chars[rand.Intn(len(chars))]
	}
	return string(b)
}

// ============================================================================
// 메인 — 모든 컴포넌트를 시작하고 허브 앤 스포크 패턴을 시연한다.
// ============================================================================

func main() {
	fmt.Println("========================================")
	fmt.Println("Kubernetes 아키텍처 시뮬레이션")
	fmt.Println("허브 앤 스포크(Hub-and-Spoke) 패턴")
	fmt.Println("========================================")
	fmt.Println()
	fmt.Println("아키텍처:")
	fmt.Println("  Controller ──┐")
	fmt.Println("  Scheduler  ──┤── API Server (Hub) ──── etcd (인메모리)")
	fmt.Println("  Kubelet(s) ──┘")
	fmt.Println()

	// 1. API Server 시작
	apiServer := NewAPIServer()
	go func() {
		server := &http.Server{Addr: ":0", Handler: apiServer}
		_ = server
		// HTTP 서버는 데모에서 직접 메서드 호출로 대체
	}()
	fmt.Println("[API Server] 시작 완료")

	// 2. 종료 채널
	stopCh := make(chan struct{})

	// 3. 노드 목록
	nodes := []string{"node-1", "node-2", "node-3"}

	// 4. Kubelet 시작 (각 노드마다 하나)
	for _, nodeName := range nodes {
		kubelet := NewKubelet(apiServer, nodeName)
		go kubelet.Run(stopCh)
	}
	time.Sleep(50 * time.Millisecond)

	// 5. Scheduler 시작
	scheduler := NewScheduler(apiServer, nodes)
	go scheduler.Run(stopCh)
	time.Sleep(50 * time.Millisecond)

	// 6. ReplicaSet Controller 시작 (3개 복제본)
	controller := NewReplicaSetController(apiServer, "web-server", 3)
	go controller.Run(stopCh)

	// 7. 잠시 대기하여 전체 흐름 관찰
	//    Controller가 파드 생성 → Scheduler가 노드 배정 → Kubelet이 실행
	time.Sleep(2 * time.Second)

	// 8. 최종 상태 출력
	fmt.Println()
	fmt.Println("========================================")
	fmt.Println("최종 파드 상태:")
	fmt.Println("========================================")
	pods := apiServer.ListPods()
	for _, p := range pods {
		fmt.Printf("  %-30s 노드=%-8s 상태=%-10s labels=%v rv=%d\n",
			p.Namespace+"/"+p.Name, p.NodeName, string(p.Phase), p.Labels, p.ResourceVersion)
	}

	fmt.Println()
	fmt.Println("========================================")
	fmt.Println("핵심 포인트:")
	fmt.Println("========================================")
	fmt.Println("1. 모든 컴포넌트는 API Server를 통해서만 통신 (직접 통신 없음)")
	fmt.Println("2. Watch 메커니즘으로 변경사항을 실시간 전파")
	fmt.Println("3. Controller는 선언적 조정 루프 (desired vs actual)")
	fmt.Println("4. Scheduler는 미배정 파드를 감시하고 노드를 배정")
	fmt.Println("5. Kubelet은 자기 노드에 배정된 파드만 처리")
	fmt.Println("6. ResourceVersion으로 낙관적 동시성 제어")

	close(stopCh)
	time.Sleep(100 * time.Millisecond)
}
