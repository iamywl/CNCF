# PoC-14: 네임스페이스 매니저 (TTL 캐시)

## 개요

Hubble Observer의 네임스페이스 자동 추적 시스템을 시뮬레이션한다. Flow 이벤트에서 source/destination 네임스페이스를 추출하여 TTL 캐시에 저장하고, 주기적으로 만료된 항목을 정리하는 패턴을 재현한다.

`hubble list namespaces` 커맨드가 반환하는 네임스페이스 목록이 이 매니저를 통해 관리된다.

## 핵심 개념

### 1. Manager 인터페이스

두 개의 메서드만 가진 간결한 인터페이스이다:

```go
type Manager interface {
    GetNamespaces() []*Namespace
    AddNamespace(ns *Namespace)
}
```

### 2. TTL 캐시

- 기본 TTL: 1시간 (`namespaceTTL`)
- 기본 정리 주기: 5분 (`cleanupInterval`)
- 키: `cluster/namespace` 조합으로 유니크 식별
- 동일 네임스페이스에 새 Flow가 도착하면 TTL이 갱신됨

### 3. trackNamespaces

`LocalObserverServer`가 Flow를 처리할 때 `trackNamespaces()`를 호출하여 source와 destination의 네임스페이스를 매니저에 등록한다.

### 4. 주기적 GC

`cleanupNamespaces()`가 5분 간격으로 실행되어 TTL이 만료된 네임스페이스를 제거한다. 더 이상 트래픽이 없는 네임스페이스는 자동으로 목록에서 사라진다.

### 5. 정렬

`GetNamespaces()` 결과는 cluster 먼저, 같으면 namespace 이름순으로 정렬된다. 멀티 클러스터 환경에서 일관된 출력을 보장한다.

### 6. 동시성

`sync.RWMutex`로 읽기/쓰기를 분리한다. 여러 goroutine에서 동시에 Flow를 처리해도 안전하다.

## 실행 방법

```bash
go run main.go
```

6가지 테스트를 실행한다: Flow 기반 자동 추적, 멀티 클러스터, TTL 만료, TTL 갱신, 동시성 테스트, 주기적 GC.

## 실제 소스코드 참조

| 파일 | 핵심 함수/구조체 |
|------|-----------------|
| `cilium/pkg/hubble/observer/namespace/manager.go` | `namespaceManager` - 핵심 구현체 |
| `cilium/pkg/hubble/observer/namespace/manager.go` | `cleanupNamespaces()` - TTL 기반 정리 |
| `cilium/pkg/hubble/observer/namespace/defaults.go` | `namespaceTTL=1h`, `cleanupInterval=5m` |
| `cilium/pkg/hubble/observer/local_observer.go` | `trackNamespaces()` - Flow에서 네임스페이스 추출 |
| `cilium/pkg/hubble/observer/local_observer.go` | `GetNamespaces()` - RPC 핸들러 |
