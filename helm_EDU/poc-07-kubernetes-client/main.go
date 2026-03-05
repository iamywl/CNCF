// Helm v4 Kubernetes 클라이언트 PoC: kube.Interface, 인메모리 클러스터
//
// 이 PoC는 Helm v4의 Kubernetes 클라이언트 추상화를 시뮬레이션합니다:
//   1. kube.Interface (pkg/kube/interface.go) - Create/Update/Delete/Build/Wait
//   2. ResourceList (pkg/kube/result.go) - 리소스 목록 관리
//   3. WaitStrategy/Waiter (interface.go) - 리소스 준비 상태 대기
//   4. 인메모리 클러스터 (kube/fake 대체)
//   5. YAML 매니페스트 파싱 → 리소스 생성/업데이트/삭제 흐름
//
// 실행: go run main.go

package main

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Resource: Kubernetes 리소스 표현 (간소화)
// =============================================================================

type Resource struct {
	APIVersion string
	Kind       string
	Name       string
	Namespace  string
	Labels     map[string]string
	Data       map[string]any
	Status     string // "Pending", "Running", "Succeeded", "Failed"
	CreatedAt  time.Time
}

func (r *Resource) String() string {
	return fmt.Sprintf("%s/%s (%s)", r.Kind, r.Name, r.Namespace)
}

// ResourceList는 리소스 목록이다.
// 실제 Helm: kube.ResourceList ([]*resource.Info)
type ResourceList []*Resource

// =============================================================================
// Result: kube 작업 결과
// 실제 Helm: kube.Result{Created, Updated, Deleted}
// =============================================================================

type Result struct {
	Created ResourceList
	Updated ResourceList
	Deleted ResourceList
}

func (r *Result) String() string {
	return fmt.Sprintf("Created: %d, Updated: %d, Deleted: %d",
		len(r.Created), len(r.Updated), len(r.Deleted))
}

// =============================================================================
// WaitStrategy: 대기 전략
// 실제 Helm: kube.WaitStrategy (Watcher/Legacy)
// =============================================================================

type WaitStrategy string

const (
	WaitStrategyNone    WaitStrategy = "none"
	WaitStrategyWatcher WaitStrategy = "watcher"
	WaitStrategyLegacy  WaitStrategy = "legacy"
)

// Waiter는 리소스 준비 상태를 대기하는 인터페이스이다.
// 실제 Helm: kube.Waiter
type Waiter interface {
	Wait(resources ResourceList, timeout time.Duration) error
	WaitWithJobs(resources ResourceList, timeout time.Duration) error
	WaitForDelete(resources ResourceList, timeout time.Duration) error
}

// =============================================================================
// KubeInterface: Helm의 pkg/kube/interface.go
// Kubernetes API와 통신하는 클라이언트 인터페이스.
// 실제 Helm에서는 kubectl의 resource.Builder를 내부적으로 사용.
// =============================================================================

// KubeInterface는 Kubernetes 클라이언트 인터페이스이다.
// 실제 Helm: kube.Interface
type KubeInterface interface {
	// Get은 배포된 리소스의 상세 정보를 조회한다
	Get(resources ResourceList, related bool) (map[string][]*Resource, error)

	// Create는 하나 이상의 리소스를 생성한다
	Create(resources ResourceList) (*Result, error)

	// Update는 리소스를 업데이트하거나, 없으면 생성한다
	Update(original, target ResourceList) (*Result, error)

	// Delete는 리소스를 삭제한다
	Delete(resources ResourceList) (*Result, []error)

	// Build는 YAML Reader에서 ResourceList를 생성한다
	Build(reader io.Reader, validate bool) (ResourceList, error)

	// IsReachable는 클러스터 연결 가능 여부를 확인한다
	IsReachable() error

	// GetWaiter는 대기 전략에 맞는 Waiter를 반환한다
	GetWaiter(ws WaitStrategy) (Waiter, error)
}

// =============================================================================
// InMemoryCluster: 인메모리 Kubernetes 클러스터 시뮬레이션
// 실제 Helm의 kube/fake 패키지 역할
// =============================================================================

type InMemoryCluster struct {
	mu        sync.RWMutex
	resources map[string]*Resource // "namespace/kind/name" → Resource
	reachable bool
}

func NewInMemoryCluster() *InMemoryCluster {
	return &InMemoryCluster{
		resources: make(map[string]*Resource),
		reachable: true,
	}
}

func resourceKey(ns, kind, name string) string {
	return fmt.Sprintf("%s/%s/%s", ns, kind, name)
}

func (c *InMemoryCluster) Store(r *Resource) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := resourceKey(r.Namespace, r.Kind, r.Name)
	c.resources[key] = r
}

func (c *InMemoryCluster) Lookup(ns, kind, name string) *Resource {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.resources[resourceKey(ns, kind, name)]
}

func (c *InMemoryCluster) Remove(ns, kind, name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := resourceKey(ns, kind, name)
	if _, ok := c.resources[key]; ok {
		delete(c.resources, key)
		return true
	}
	return false
}

func (c *InMemoryCluster) All() []*Resource {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var result []*Resource
	for _, r := range c.resources {
		result = append(result, r)
	}
	return result
}

// =============================================================================
// KubeClient: kube.Interface 구현
// 실제 Helm: kube.Client{Factory, ...}
// =============================================================================

type KubeClient struct {
	cluster *InMemoryCluster
}

func NewKubeClient(cluster *InMemoryCluster) *KubeClient {
	return &KubeClient{cluster: cluster}
}

func (k *KubeClient) IsReachable() error {
	if !k.cluster.reachable {
		return fmt.Errorf("클러스터에 연결할 수 없습니다")
	}
	return nil
}

// Build는 YAML 스트림을 파싱하여 ResourceList를 생성한다.
// 실제 Helm: resource.Builder를 사용하여 YAML → resource.Info 변환
// "---"로 구분된 여러 문서를 처리한다.
func (k *KubeClient) Build(reader io.Reader, validate bool) (ResourceList, error) {
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}

	var resources ResourceList
	docs := strings.Split(string(data), "---")

	for _, doc := range docs {
		doc = strings.TrimSpace(doc)
		if doc == "" || strings.HasPrefix(doc, "#") {
			continue
		}

		r := parseYAMLDoc(doc)
		if r == nil {
			continue
		}

		if validate {
			if r.Kind == "" || r.Name == "" {
				return nil, fmt.Errorf("유효하지 않은 리소스: kind 또는 name 누락")
			}
		}

		resources = append(resources, r)
	}

	return resources, nil
}

// parseYAMLDoc는 간단한 YAML 문서를 파싱한다 (시뮬레이션)
func parseYAMLDoc(doc string) *Resource {
	r := &Resource{
		Labels: make(map[string]string),
		Data:   make(map[string]any),
	}

	lines := strings.Split(doc, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])

		switch key {
		case "apiVersion":
			r.APIVersion = val
		case "kind":
			r.Kind = val
		case "name":
			r.Name = val
		case "namespace":
			r.Namespace = val
		}
	}

	if r.Kind == "" {
		return nil
	}
	if r.Namespace == "" {
		r.Namespace = "default"
	}

	return r
}

// Create는 리소스를 생성한다.
// 실제 Helm: kube.Client.Create(resources, options...)
func (k *KubeClient) Create(resources ResourceList) (*Result, error) {
	if err := k.IsReachable(); err != nil {
		return nil, err
	}

	result := &Result{}
	for _, r := range resources {
		existing := k.cluster.Lookup(r.Namespace, r.Kind, r.Name)
		if existing != nil {
			return nil, fmt.Errorf("리소스 %s 가 이미 존재합니다", r)
		}

		r.Status = "Running"
		r.CreatedAt = time.Now()
		k.cluster.Store(r)
		result.Created = append(result.Created, r)
		fmt.Printf("    [Kube] Created: %s\n", r)
	}

	return result, nil
}

// Update는 원본과 대상을 비교하여 업데이트한다.
// 실제 Helm: kube.Client.Update(original, target, options...)
// 3-way merge: 원본에만 있으면 삭제, 대상에만 있으면 생성, 둘 다 있으면 업데이트
func (k *KubeClient) Update(original, target ResourceList) (*Result, error) {
	if err := k.IsReachable(); err != nil {
		return nil, err
	}

	result := &Result{}

	// 대상에 있는 리소스 처리 (생성 또는 업데이트)
	targetMap := make(map[string]*Resource)
	for _, r := range target {
		key := resourceKey(r.Namespace, r.Kind, r.Name)
		targetMap[key] = r

		existing := k.cluster.Lookup(r.Namespace, r.Kind, r.Name)
		if existing == nil {
			// 새 리소스 생성
			r.Status = "Running"
			r.CreatedAt = time.Now()
			k.cluster.Store(r)
			result.Created = append(result.Created, r)
			fmt.Printf("    [Kube] Created (new): %s\n", r)
		} else {
			// 기존 리소스 업데이트
			r.Status = "Running"
			r.CreatedAt = existing.CreatedAt
			k.cluster.Store(r)
			result.Updated = append(result.Updated, r)
			fmt.Printf("    [Kube] Updated: %s\n", r)
		}
	}

	// 원본에만 있는 리소스 삭제 (대상에서 제거됨)
	for _, r := range original {
		key := resourceKey(r.Namespace, r.Kind, r.Name)
		if _, found := targetMap[key]; !found {
			k.cluster.Remove(r.Namespace, r.Kind, r.Name)
			result.Deleted = append(result.Deleted, r)
			fmt.Printf("    [Kube] Deleted (removed): %s\n", r)
		}
	}

	return result, nil
}

// Delete는 리소스를 삭제한다.
func (k *KubeClient) Delete(resources ResourceList) (*Result, []error) {
	result := &Result{}
	var errs []error

	for _, r := range resources {
		if k.cluster.Remove(r.Namespace, r.Kind, r.Name) {
			result.Deleted = append(result.Deleted, r)
			fmt.Printf("    [Kube] Deleted: %s\n", r)
		} else {
			errs = append(errs, fmt.Errorf("리소스 %s 를 찾을 수 없습니다", r))
		}
	}

	return result, errs
}

// Get은 리소스 상태를 조회한다.
func (k *KubeClient) Get(resources ResourceList, related bool) (map[string][]*Resource, error) {
	result := make(map[string][]*Resource)
	for _, r := range resources {
		existing := k.cluster.Lookup(r.Namespace, r.Kind, r.Name)
		if existing != nil {
			result[r.Kind] = append(result[r.Kind], existing)
		}
	}
	return result, nil
}

// GetWaiter는 대기 전략에 맞는 Waiter를 반환한다.
func (k *KubeClient) GetWaiter(ws WaitStrategy) (Waiter, error) {
	return &SimpleWaiter{cluster: k.cluster}, nil
}

// =============================================================================
// SimpleWaiter: Waiter 인터페이스 구현
// 실제 Helm: kube.StatusWaiter (watcher) / kube.LegacyWaiter (polling)
// =============================================================================

type SimpleWaiter struct {
	cluster *InMemoryCluster
}

func (w *SimpleWaiter) Wait(resources ResourceList, timeout time.Duration) error {
	fmt.Printf("    [Waiter] %d개 리소스 준비 대기 (timeout: %v)\n", len(resources), timeout)
	for _, r := range resources {
		existing := w.cluster.Lookup(r.Namespace, r.Kind, r.Name)
		if existing == nil {
			return fmt.Errorf("리소스 %s 를 찾을 수 없습니다", r)
		}
		fmt.Printf("    [Waiter] %s: %s (준비됨)\n", r, existing.Status)
	}
	return nil
}

func (w *SimpleWaiter) WaitWithJobs(resources ResourceList, timeout time.Duration) error {
	return w.Wait(resources, timeout)
}

func (w *SimpleWaiter) WaitForDelete(resources ResourceList, timeout time.Duration) error {
	fmt.Printf("    [Waiter] %d개 리소스 삭제 대기\n", len(resources))
	for _, r := range resources {
		existing := w.cluster.Lookup(r.Namespace, r.Kind, r.Name)
		if existing != nil {
			return fmt.Errorf("리소스 %s 가 아직 존재합니다", r)
		}
		fmt.Printf("    [Waiter] %s: 삭제 확인\n", r)
	}
	return nil
}

// =============================================================================
// main: 데모 실행
// =============================================================================

func main() {
	fmt.Println("=== Helm v4 Kubernetes 클라이언트 PoC ===")
	fmt.Println()

	cluster := NewInMemoryCluster()
	client := NewKubeClient(cluster)

	// 1) Build: YAML → ResourceList
	demoBuild(client)

	// 2) Create: 리소스 생성
	resources := demoCreate(client)

	// 3) Wait: 리소스 준비 대기
	demoWait(client, resources)

	// 4) Update: 리소스 업데이트 (3-way merge)
	demoUpdate(client, resources)

	// 5) Delete: 리소스 삭제
	demoDelete(client)

	// 6) 클러스터 연결 불가 시나리오
	demoUnreachable(cluster, client)

	// 7) 전체 클러스터 상태 출력
	demoClusterState(cluster)
}

func demoBuild(client *KubeClient) {
	fmt.Println("--- 1. Build: YAML → ResourceList ---")

	manifest := `---
apiVersion: apps/v1
kind: Deployment
name: myapp
namespace: production
---
apiVersion: v1
kind: Service
name: myapp-svc
namespace: production
---
apiVersion: v1
kind: ConfigMap
name: myapp-config
namespace: production`

	resources, err := client.Build(strings.NewReader(manifest), true)
	if err != nil {
		fmt.Printf("  Build 에러: %v\n", err)
		return
	}

	fmt.Printf("  파싱된 리소스: %d개\n", len(resources))
	for _, r := range resources {
		fmt.Printf("    %s (apiVersion: %s)\n", r, r.APIVersion)
	}
	fmt.Println()
}

func demoCreate(client *KubeClient) ResourceList {
	fmt.Println("--- 2. Create: 리소스 생성 ---")

	manifest := `---
apiVersion: apps/v1
kind: Deployment
name: webapp
namespace: default
---
apiVersion: v1
kind: Service
name: webapp-svc
namespace: default
---
apiVersion: v1
kind: ConfigMap
name: webapp-config
namespace: default`

	resources, _ := client.Build(strings.NewReader(manifest), false)
	result, err := client.Create(resources)
	if err != nil {
		fmt.Printf("  Create 에러: %v\n", err)
		return nil
	}

	fmt.Printf("  결과: %s\n", result)
	fmt.Println()
	return resources
}

func demoWait(client *KubeClient, resources ResourceList) {
	fmt.Println("--- 3. Wait: 리소스 준비 대기 ---")

	waiter, _ := client.GetWaiter(WaitStrategyWatcher)
	err := waiter.Wait(resources, 30*time.Second)
	if err != nil {
		fmt.Printf("  Wait 에러: %v\n", err)
	}
	fmt.Println()
}

func demoUpdate(client *KubeClient, original ResourceList) {
	fmt.Println("--- 4. Update: 리소스 업데이트 (3-way merge) ---")

	// 새 매니페스트: ConfigMap 제거, Ingress 추가, Deployment 유지
	newManifest := `---
apiVersion: apps/v1
kind: Deployment
name: webapp
namespace: default
---
apiVersion: v1
kind: Service
name: webapp-svc
namespace: default
---
apiVersion: networking.k8s.io/v1
kind: Ingress
name: webapp-ingress
namespace: default`

	target, _ := client.Build(strings.NewReader(newManifest), false)
	result, err := client.Update(original, target)
	if err != nil {
		fmt.Printf("  Update 에러: %v\n", err)
		return
	}

	fmt.Printf("  결과: %s\n", result)
	fmt.Println("  (ConfigMap 삭제, Ingress 생성, Deployment/Service 업데이트)")
	fmt.Println()
}

func demoDelete(client *KubeClient) {
	fmt.Println("--- 5. Delete: 리소스 삭제 ---")

	toDelete := ResourceList{
		{Kind: "Ingress", Name: "webapp-ingress", Namespace: "default"},
	}

	result, errs := client.Delete(toDelete)
	fmt.Printf("  결과: %s\n", result)
	if len(errs) > 0 {
		for _, e := range errs {
			fmt.Printf("  에러: %v\n", e)
		}
	}

	// 삭제 확인
	waiter, _ := client.GetWaiter(WaitStrategyLegacy)
	waiter.WaitForDelete(toDelete, 10*time.Second)
	fmt.Println()
}

func demoUnreachable(cluster *InMemoryCluster, client *KubeClient) {
	fmt.Println("--- 6. 클러스터 연결 불가 시나리오 ---")

	cluster.reachable = false
	err := client.IsReachable()
	fmt.Printf("  IsReachable: %v\n", err)

	_, err = client.Create(ResourceList{{Kind: "Pod", Name: "test", Namespace: "default"}})
	fmt.Printf("  Create 시도: %v\n", err)

	cluster.reachable = true
	fmt.Println()
}

func demoClusterState(cluster *InMemoryCluster) {
	fmt.Println("--- 7. 최종 클러스터 상태 ---")

	all := cluster.All()
	if len(all) == 0 {
		fmt.Println("  (리소스 없음)")
	} else {
		fmt.Printf("  총 %d개 리소스:\n", len(all))
		for _, r := range all {
			fmt.Printf("    %s  status=%s  created=%s\n", r, r.Status, r.CreatedAt.Format("15:04:05"))
		}
	}

	fmt.Println()
	fmt.Println("=== Kubernetes 클라이언트 PoC 완료 ===")
	fmt.Println()
	fmt.Println("핵심 패턴 요약:")
	fmt.Println("  1. kube.Interface: Create/Update/Delete/Build/Wait 메서드 (인터페이스 기반)")
	fmt.Println("  2. Build: YAML 스트림(---로 구분) → ResourceList 파싱")
	fmt.Println("  3. Update: 원본/대상 비교 → 생성/업데이트/삭제 (3-way merge)")
	fmt.Println("  4. WaitStrategy: watcher(이벤트 기반) / legacy(폴링) 전략 선택")
	fmt.Println("  5. IsReachable: 클러스터 연결 가능 여부 사전 확인")
}
