# PoC-07: Service Discovery (서비스 디스커버리)

## 개요

Prometheus의 **플러거블 서비스 디스커버리(SD) 프레임워크**를 Go 표준 라이브러리만으로 재현한 PoC이다.

Prometheus는 모니터링 대상(타겟)을 자동으로 발견하기 위해 20가지 이상의 SD 메커니즘을 지원한다(static, file, DNS, Kubernetes, Consul, EC2 등). 이 모든 메커니즘은 하나의 `Discoverer` 인터페이스를 구현하며, `DiscoveryManager`가 이들을 통합 관리한다.

## 실행 방법

```bash
go run main.go
```

외부 의존성 없이 Go 표준 라이브러리만 사용한다. 실행 시간 약 30초.

## 아키텍처

```
┌─────────────────────────────────────────────────────────────────┐
│                     DiscoveryManager                            │
│                                                                 │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐          │
│  │  Static SD   │  │   File SD    │  │   DNS SD     │  ...     │
│  │ (Discoverer) │  │ (Discoverer) │  │ (Discoverer) │          │
│  └──────┬───────┘  └──────┬───────┘  └──────┬───────┘          │
│         │                 │                  │                  │
│         └────────┬────────┴────────┬─────────┘                  │
│                  ▼                 ▼                             │
│         ┌──────────────┐  ┌──────────────┐                     │
│         │   updater    │  │   updater    │  ← 각 provider별    │
│         └──────┬───────┘  └──────┬───────┘                     │
│                │                 │                              │
│                ▼                 ▼                              │
│         ┌─────────────────────────────┐                        │
│         │  targets map (dedup by      │                        │
│         │  poolKey + Source)           │                        │
│         └──────────────┬──────────────┘                        │
│                        │ triggerSend                            │
│                        ▼                                       │
│         ┌─────────────────────────────┐                        │
│         │  sender (5초 디바운싱)       │                        │
│         └──────────────┬──────────────┘                        │
│                        │ syncCh                                │
└────────────────────────┼───────────────────────────────────────┘
                         ▼
               ┌──────────────────┐
               │  ScrapeManager   │
               │  (타겟 소비자)    │
               └──────────────────┘
```

## 핵심 구성 요소

### 1. TargetGroup

```
실제 코드: discovery/targetgroup/targetgroup.go
```

타겟 그룹은 동일한 레이블을 공유하는 타겟들의 집합이다.

| 필드 | 설명 |
|------|------|
| `Targets` | 개별 타겟 레이블 목록 (`__address__` 필수) |
| `Labels` | 그룹 공통 레이블 |
| `Source` | 그룹 식별자 — Manager의 중복 제거(dedup) 키 |

`Source` 필드가 핵심이다. Manager는 동일한 `Source`를 가진 그룹이 오면 기존 그룹을 교체하고, 빈 `Targets`가 오면 해당 `Source`를 삭제한다.

### 2. Discoverer 인터페이스

```
실제 코드: discovery/discovery.go:35-40
```

```go
type Discoverer interface {
    Run(ctx context.Context, ch chan<- []*targetgroup.Group)
}
```

모든 SD 메커니즘의 공통 계약:
- `Run`은 `ctx`가 취소될 때까지 **블로킹**해야 한다
- 업데이트 채널(`ch`)을 **close하면 안 된다** (Manager가 관리)
- 초기 실행 시 전체 타겟을 한 번 전송해야 한다
- 변경이 감지되면 새로운 `[]TargetGroup`을 전송한다

### 3. SD 메커니즘 구현체

| 구현체 | 실제 코드 | PoC 동작 |
|--------|-----------|----------|
| **StaticDiscoverer** | `discovery/discovery.go:159` | 고정 타겟을 한 번 전송 후 ctx 대기 |
| **FileDiscoverer** | `discovery/file/file.go` | JSON 파일을 3초 주기로 폴링, 변경 감지 시 전송 |
| **DNSDiscoverer** | `discovery/dns/dns.go` | 시뮬레이션된 SRV 레코드를 주기적 전송 |

FileDiscoverer의 핵심 동작:
- 파일 파싱 후 각 JSON 엔트리를 별도 `Source`로 관리
- 이전 폴링에서 존재했지만 현재는 없는 `Source`에 대해 **빈 TargetGroup을 전송** → Manager가 해당 그룹 삭제

### 4. DiscoveryManager

```
실제 코드: discovery/manager.go:162-198
```

핵심 메커니즘 3가지:

#### a) poolKey 기반 타겟 저장

```go
// poolKey = {scrape job 이름, provider 이름}
targets map[poolKey]map[string]TargetGroup
//                       ↑ Source 키로 중복 제거
```

동일한 scrape job에 여러 provider가 타겟을 공급할 수 있다. `poolKey`로 provider별 타겟을 분리하고, `Source`로 그룹 단위 중복 제거를 수행한다.

#### b) updater → triggerSend → sender 파이프라인

```
Discoverer.Run() → updates 채널 → updater() → targets 맵 갱신 → triggerSend 시그널
                                                                         │
sender() ← ticker(5s) 체크 시 triggerSend 확인 ←─────────────────────────┘
    │
    └→ allGroups() 호출 → 전체 병합 → syncCh 전송 → ScrapeManager
```

#### c) 5초 디바운싱

```
실제 코드: manager.go:97 → updatert: 5 * time.Second
```

`sender()` 고루틴은 5초 주기 ticker와 `triggerSend` 채널을 조합한다:
1. ticker가 울린다
2. `triggerSend`에 시그널이 있는지 확인
3. 있으면 `allGroups()`로 전체 스냅샷을 `syncCh`에 전송
4. 없으면 아무것도 안 함

이 설계의 이유: DNS SD는 30초마다, Kubernetes SD는 실시간으로 업데이트를 보낸다. 짧은 시간 내 여러 discoverer에서 온 업데이트를 하나의 스냅샷으로 묶어 ScrapeManager에 전달하면 불필요한 scrape pool 재구성을 줄일 수 있다.

### 5. ScrapeManager (소비자)

`syncCh`에서 전체 타겟 스냅샷을 받아 현재 상태와 비교하여:
- 새로 추가된 타겟 → scrape 시작
- 사라진 타겟 → scrape 중지
- 변경 없는 타겟 → 유지

## 데모 시나리오

| 시간 | 이벤트 | 결과 |
|------|--------|------|
| 0초 | Manager 시작, 4개 Discoverer 등록 | Static(3), File(2), DNS(2) = 7 타겟 |
| ~7초 | 첫 디바운싱 완료 | ScrapeManager에 7 타겟 전달 |
| ~7초 | 파일 변경: 타겟 추가 (host-3, host-4) | 9 타겟으로 증가 |
| ~17초 | 파일 변경: canary 그룹 제거, staging 축소 | 6 타겟으로 감소 |
| ~27초 | Manager 종료 | 모든 discoverer ctx 취소 |

## Prometheus 실제 코드와의 대응

| PoC | Prometheus 실제 코드 |
|-----|---------------------|
| `TargetGroup` | `discovery/targetgroup/targetgroup.go` → `Group` struct |
| `Discoverer` 인터페이스 | `discovery/discovery.go:35` → `Discoverer` interface |
| `StaticDiscoverer` | `discovery/discovery.go:159` → `staticDiscoverer` |
| `FileDiscoverer` | `discovery/file/file.go` → `Discovery` struct (fsnotify + polling) |
| `DNSDiscoverer` | `discovery/dns/dns.go` → `Discovery` struct (net.LookupSRV) |
| `DiscoveryManager` | `discovery/manager.go:162` → `Manager` struct |
| `poolKey` | `discovery/manager.go:33` → `poolKey` struct |
| `updater()` | `discovery/manager.go:355` → `updater()` |
| `sender()` | `discovery/manager.go:385` → `sender()` |
| `allGroups()` | `discovery/manager.go:449` → `allGroups()` |
| `triggerSend` | `discovery/manager.go:98` → `triggerSend chan struct{}` |
| `updatert = 5s` | `discovery/manager.go:97` → `updatert: 5 * time.Second` |
| `ScrapeManager` | `scrape/manager.go` → `Manager` (실제는 더 복잡한 pool 관리) |
