# PoC-07: etcd Watch + ResourceVersion 메커니즘

## 개요

Kubernetes API 서버의 핵심인 etcd Watch와 ResourceVersion 메커니즘을 시뮬레이션한다.

Kubernetes의 모든 리소스 변경은 etcd에 저장되며, 컨트롤러와 클라이언트는 Watch API를 통해 변경사항을 실시간으로 구독한다. ResourceVersion은 etcd의 MVCC revision에 기반하며, 이벤트 순서 보장과 낙관적 동시성 제어의 핵심이다.

## 실제 코드 참조

| 파일 | 역할 |
|------|------|
| `staging/src/k8s.io/apiserver/pkg/storage/etcd3/store.go` | etcd3 스토리지 구현, GuaranteedUpdate |
| `staging/src/k8s.io/apiserver/pkg/storage/cacher/watch_cache.go` | Watch 캐시, 이벤트 히스토리 관리 |

## 시뮬레이션하는 개념

### 1. 전역 Revision (= ResourceVersion)
- 모든 쓰기 연산(Put, Delete)은 전역 revision을 1씩 증가시킨다
- 이는 etcd의 MVCC global revision에 대응한다
- Kubernetes에서는 이 값이 리소스의 `metadata.resourceVersion`이 된다

### 2. Watch API
- 특정 revision 이후의 변경 이벤트를 스트리밍한다
- 히스토리 재생: startRev 이후의 과거 이벤트를 즉시 전송
- 실시간 스트리밍: 새로운 이벤트를 실시간으로 전달
- prefix 매칭으로 특정 리소스 타입만 감시 가능

### 3. OptimisticPut (Create Semantics)
- 키가 존재하지 않을 때만 생성한다
- 이미 존재하면 AlreadyExists 오류를 반환한다
- Kubernetes의 `kubectl create` 동작에 대응한다

### 4. GuaranteedUpdate (Compare-And-Swap)
- 현재 값을 읽고, 수정하고, CAS로 원자적으로 업데이트한다
- 중간에 다른 클라이언트가 수정하면 자동으로 재시도한다
- Kubernetes의 낙관적 동시성 제어의 핵심 메커니즘이다

### 5. ResourceVersion 일관성
- 모든 이벤트의 ResourceVersion은 단조 증가한다
- Watch 클라이언트는 이를 통해 이벤트 순서를 보장받는다

## 실행

```bash
go run main.go
```

## 출력 예시

```
=== Kubernetes etcd Watch + ResourceVersion 시뮬레이션 ===

--- 1. 기본 CRUD + ResourceVersion 추적 ---
  PUT nginx → revision=1
  PUT redis → revision=2
  PUT nginx (update) → revision=3
  GET nginx: value={"name":"nginx","image":"nginx:1.21"}, ModRevision=3, CreateRevision=1
  ...
```
