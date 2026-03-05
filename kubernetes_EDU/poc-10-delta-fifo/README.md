# PoC-10: DeltaFIFO 큐

## 개요

Kubernetes client-go의 핵심 데이터 구조인 DeltaFIFO를 시뮬레이션한다.

DeltaFIFO는 Reflector(생산자)가 API 서버의 Watch 이벤트를 넣고, Informer의 processLoop(소비자)가 꺼내는 생산자-소비자 큐이다. 일반 큐와 달리, 같은 키의 변경사항을 Deltas 슬라이스에 축적하여 소비자가 전체 변경 이력을 한 번에 받을 수 있다.

## 실제 코드 참조

| 파일 | 역할 |
|------|------|
| `staging/src/k8s.io/client-go/tools/cache/delta_fifo.go` | DeltaFIFO 전체 구현 |

## 시뮬레이션하는 개념

### 1. 키별 Delta 축적
- 같은 Pod의 Added → Updated → Updated → Deleted가 하나의 Deltas 슬라이스에 누적
- Pop()이 전체 이력을 한 번에 반환

### 2. FIFO 순서
- queue 슬라이스가 키의 처리 순서를 유지
- 같은 키의 추가 이벤트는 queue에 중복 추가하지 않음

### 3. 중복 제거 (dedupDeltas)
- 연속된 두 Deleted Delta를 하나로 병합
- DeletedFinalStateUnknown보다 실제 객체를 가진 Delta를 선호

### 4. Replace (re-list)
- Watch 연결이 끊어진 후 전체 목록을 다시 받을 때 사용
- 새 목록에 없는 기존 객체는 DeletedFinalStateUnknown으로 삭제 처리

### 5. 블로킹 Pop
- 큐가 비어있으면 sync.Cond.Wait()로 대기
- 새 아이템이 추가되면 Broadcast()로 깨움

### 6. HasSynced
- 초기 Replace()의 모든 아이템이 Pop되었는지 추적
- Informer가 캐시 동기화 완료를 판단하는 데 사용

## 실행

```bash
go run main.go
```
