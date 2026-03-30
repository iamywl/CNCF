# PoC-16: Authentication & Encryption (인증 및 암호화)

## 개요

Cilium의 **인증 및 암호화 서브시스템**을 시뮬레이션하는 PoC이다.
데이터패스에서 패킷이 도착하면 BPF `cilium_auth_map`을 조회하여 인증 상태를 확인하고,
미인증/만료 시 유저스페이스의 AuthManager가 Mutual TLS 핸드셰이크를 수행하는 전체 흐름을 재현한다.

구현 범위: AuthManager, Mutual TLS 핸드셰이크, AuthMapCache, AuthMap GC, 인증서 로테이션.

## 배경 지식

### 제로 트러스트 보안과 Mutual Authentication

제로 트러스트 모델은 네트워크 경계(perimeter) 내부도 신뢰하지 않는다.
**모든 통신 주체가 매번 신원을 증명**해야 한다. Cilium은 이를 mTLS(Mutual TLS)로 구현한다.
일반 TLS는 서버만 인증서를 제시하지만, mTLS에서는 클라이언트도 인증서를 제시하여
양쪽 모두 신원을 확인하는 상호 인증을 수행한다.

### SPIFFE/SPIRE 신원 프레임워크

SPIFFE(Secure Production Identity Framework For Everyone)는 워크로드 신원 표준이다.
각 워크로드에 `spiffe://cluster.local/identity/<id>` 형식의 SPIFFE ID를 부여하고,
X.509 인증서의 SAN에 포함시킨다. Cilium의 `CertificateProvider` 인터페이스가
Identity별 인증서 발급(`GetCertificateForIdentity`), Trust Bundle 반환(`GetTrustBundle`),
SNI 변환(`NumericIdentityToSNI`), 로테이션 구독(`SubscribeToRotatedIdentities`)을 제공한다.

### WireGuard/IPsec 투명 암호화의 원리

Cilium은 노드 간 투명 암호화를 두 가지 방식으로 지원한다.
**WireGuard**(커널 모듈 기반 터널)와 **IPsec**(XFRM 프레임워크 기반 패킷 암호화).
BPF 프로그램이 패킷을 가로채어 처리하므로 애플리케이션 변경이 불필요하다.
이 PoC는 인증(Authentication) 계층에 집중하며, 암호화 터널은 다루지 않는다.

### AuthMap -- BPF 맵을 통한 인증 상태 캐싱

핵심 설계: **인증은 유저스페이스에서 수행하되, 결과는 BPF 맵에 캐싱한다.**
BPF 데이터패스는 패킷마다 `cilium_auth_map`을 O(1)로 조회한다.
mTLS 핸드셰이크는 최초 1회만 수행하고 결과(만료 시간)를 BPF 맵에 기록하면,
이후 패킷은 맵 조회만으로 통과한다(fast path).
캐시 키: `localIdentity + remoteIdentity + remoteNodeID + authType`

## 시뮬레이션하는 개념

| 개념 | 실제 Cilium 소스 경로 | PoC 구현 |
|------|----------------------|---------|
| AuthManager | `pkg/auth/manager.go` | 인증 요청 관리, backoff, pending 중복 방지 |
| mutualAuthHandler | `pkg/auth/mutual_authhandler.go` | SPIFFE mTLS 핸드셰이크 (TLS 1.3) |
| authMapCache | `pkg/auth/authmap_cache.go` | BPF authmap 유저스페이스 캐시 (storedAt 기반 backoff) |
| authMapGC | `pkg/auth/authmap_gc.go` | 만료/노드삭제/Identity삭제/정책없음 GC |
| CertificateProvider | `pkg/auth/certs/provider.go` | 인증서 발급, Trust Bundle, 로테이션 이벤트 |
| CA | SPIRE 또는 내부 CA | ECDSA P-256 기반 자체 CA, Identity 인증서 발급 |

## 아키텍처 다이어그램

```
  ┌──────────────────────────────────────────────────────┐
  │           BPF Datapath (패킷 경로)                    │
  │  패킷 → cilium_auth_map 조회                         │
  │    ├── VALID & 미만료 → PASS (fast path)             │
  │    └── 없음/만료 → signalmap에 인증 요청 기록         │
  └─────────────────┬────────────────────────────────────┘
                    │ signal
  ┌─────────────────▼────────────────────────────────────┐
  │           AuthManager (userspace)                     │
  │  1. markPendingAuth() - 중복 인증 방지               │
  │  2. backoff 확인 (storedAt + backoffTime)            │
  │  3. mutualAuthHandler.authenticate()                 │
  │     └─ CertProvider → TCP → TLS 1.3 mTLS 핸드셰이크 │
  │  4. authmap.Update(key, expiration) - 결과 캐시      │
  └─────────────────┬────────────────────────────────────┘
                    │
  ┌─────────────────▼────────────────────────────────────┐
  │      authMapGarbageCollector (주기적)                  │
  │  - cleanupExpiredEntries (만료)                       │
  │  - cleanupDeletedNode / cleanupDeletedIdentity       │
  │  - cleanupEntriesWithoutAuthPolicy (정책 제거)       │
  └──────────────────────────────────────────────────────┘
```

## 코드 해설

### 1. `authKey` / `authInfo` / `authInfoCache` -- 인증 맵의 키와 값

```go
type authKey struct {
    localIdentity  uint32       // 로컬 Cilium Identity
    remoteIdentity uint32       // 원격 Cilium Identity
    remoteNodeID   uint16       // 원격 노드 ID
    authType       AuthType     // 인증 유형 (SPIRE 등)
}
type authInfoCache struct {
    authInfo                     // expiration time.Time
    storedAt time.Time           // 캐시 저장 시점 (backoff 판단용)
}
```

`storedAt`은 유저스페이스 캐시에만 존재하며, `storedAt + backoffTime` 이내의 재인증을 건너뛴다.

### 2. `authMapCacheImpl` -- BPF 맵 위의 유저스페이스 캐시

`Update()`는 BPF 맵에 먼저 쓰고 Go 맵에 `storedAt`과 함께 저장한다.
`DeleteIf()`는 predicate 조건으로 BPF+캐시 동시 제거. `Pressure()`는 맵 사용률 반환.

### 3. `AuthManager.HandleAuthRequest()` -- 인증 요청 처리 핵심 흐름

실제 `pkg/auth/manager.go`의 `handleAuthRequest()`를 재현한다. 5단계 절차:
(1) `markPendingAuth` 중복 방지 (2) backoff 확인 (3) 핸들러 선택
(4) `authenticate()` mTLS 수행 (5) `authmap.Update()` 결과 캐시

### 4. `mutualAuthHandlerImpl.authenticate()` -- SPIFFE mTLS 핸드셰이크

로컬 Identity 인증서 획득 후 TCP 연결, TLS 1.3 업그레이드. `VerifyPeerCertificate`
콜백에서 상대방 인증서 만료 시간을 추적하여 클라이언트/서버 중 더 빠른 쪽을 반환한다.

### 5. GC 함수들 -- 4가지 정리 전략

만료 항목(`cleanupExpiredEntries`), 삭제된 노드(`cleanupDeletedNode`),
삭제된 Identity(`cleanupDeletedIdentity`), 정책 없는 항목(`cleanupEntriesWithoutAuthPolicy`).

## 실행 방법 및 예상 출력

```bash
go run main.go
```

```
=== Cilium 인증 및 암호화 시뮬레이션 ===

[1] CA 및 CertificateProvider 초기화
  CA 생성 완료: Cilium CA (ECDSA P-256)

[2] Mutual TLS 핸드셰이크 (mutualAuthHandler)
  Mutual Auth 리스너: 127.0.0.1:<port> (TLS 1.3, ClientAuth=Required)
  핸드셰이크 성공! 만료: 2026-03-30 XX:XX:XX

[3] AuthMapCache (BPF authmap 캐시)
  추가: local=1001, remote=2001, nodeID=1, auth=spire (TTL=10s)
  ...
  맵 사용률: 0.0008%

[4] AuthManager 인증 흐름
    [AuthManager] 인증 성공: local=5001, remote=6001, ...
    [AuthManager] Backoff 이내 - 재인증 건너뜀 (같은 키 즉시 재요청)
    [AuthManager] 인증 성공 (reAuth=true 강제 재인증)

[5] 인증서 로테이션 이벤트 처리
    [CertRotation] 로테이션된 Identity 1001에 대해 재인증 트리거

[6] AuthMap Garbage Collection
  cleanupExpiredEntries: 1개 제거
  cleanupDeletedIdentity(identity=3001): 1개 제거

[7] 최종 AuthMap 상태 (GC 후 남은 유효 항목)

[8] 데이터패스 인증 확인
  1001 -> 2001: PASS (인증됨)    |  1001 -> 9999: DROP (항목 없음)
  5001 -> 6001: PASS (인증됨)    |  3001 -> 4001: DROP (GC로 삭제됨)
```

## 핵심 포인트

1. **Fast Path / Slow Path 분리**: BPF는 맵 조회(O(1))만, mTLS 핸드셰이크는 유저스페이스 비동기 처리. 패킷당 핸드셰이크 없이 성능 영향 최소화.
2. **Backoff 메커니즘**: `storedAt + backoffTime` 이내 동일 키 재인증 무시. 핸드셰이크 폭주 방지.
3. **Pending 중복 방지**: 동일 `authKey` 인증 진행 중이면 새 고루틴 생성 차단.
4. **인증서 로테이션 연동**: 인증서 갱신 시 해당 Identity의 모든 authmap 항목 재인증.
5. **다층 GC 전략**: 만료/노드삭제/Identity삭제/정책변경 4가지 축으로 stale 항목 정리.

## 실제 Cilium과의 차이점

| 항목 | 실제 Cilium | 이 PoC |
|------|-----------|--------|
| BPF 맵 | 커널 공간 `cilium_auth_map` (eBPF 해시맵) | Go `map[authKey]authInfo` 인메모리 시뮬레이션 |
| Signal 전달 | BPF signalmap -> epoll 기반 이벤트 루프 | 직접 `HandleAuthRequest()` 함수 호출 |
| 인증서 발급 | SPIRE 워크로드 API 또는 Cilium 내부 CA | 자체 구현한 인메모리 CA (ECDSA P-256) |
| SPIFFE ID | `spiffe://cluster.local/identity/<id>` URI SAN | `identity-<id>.cilium.local` DNS SAN |
| 노드 간 연결 | 원격 노드의 Mutual Auth 리스너로 TCP 연결 | 로컬 루프백(`127.0.0.1`)으로 연결 |
| 핸드셰이크 방식 | `InsecureSkipVerify=true` + 커스텀 `VerifyPeerCertificate` | 동일 패턴 사용 (TLS 1.3 강제) |
| GC 트리거 | hive Job 스케줄러 기반 주기적 실행 + 이벤트 기반 | 함수 직접 호출 |
| WireGuard/IPsec | 커널 XFRM/WireGuard 모듈 기반 투명 암호화 | 미구현 (인증 계층에 집중) |
| 동시성 | 고루틴 풀 + 채널 기반 비동기 처리 | 동기 호출 (시뮬레이션 단순화) |
