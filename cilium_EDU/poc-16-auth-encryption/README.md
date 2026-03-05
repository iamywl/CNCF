# PoC-16: Authentication & Encryption (인증 및 암호화)

## 개요

Cilium의 인증 시스템을 시뮬레이션한다.
AuthManager, Mutual TLS 핸드셰이크, AuthMap 캐시, AuthMap GC, 인증서 로테이션을 재현한다.

## 시뮬레이션 대상

| 컴포넌트 | 실제 Cilium 경로 | PoC 구현 |
|---------|-----------------|---------|
| AuthManager | `pkg/auth/manager.go` | 인증 요청 관리, backoff, pending 추적 |
| mutualAuthHandler | `pkg/auth/mutual_authhandler.go` | SPIFFE mTLS 핸드셰이크 |
| authMapCache | `pkg/auth/authmap_cache.go` | BPF authmap 유저스페이스 캐시 |
| authMapGC | `pkg/auth/authmap_gc.go` | 만료/노드삭제/Identity삭제 GC |
| CertificateProvider | `pkg/auth/certs/provider.go` | 인증서 발급/로테이션 |

## 핵심 개념

1. **AuthManager 인증 흐름**: signalmap 수신 -> markPendingAuth -> backoff 확인 -> authenticate -> authmap 갱신
2. **authKey 구조**: localIdentity + remoteIdentity + remoteNodeID + authType
3. **authInfoCache**: 만료시간(expiration) + 저장시점(storedAt) - backoff 판단에 사용
4. **GC 종류**: 만료 항목, 삭제된 노드, 삭제된 Identity, 정책 없는 항목
5. **인증서 로테이션**: Identity의 인증서가 갱신되면 관련 authmap 항목을 재인증

## 실행

```bash
go run main.go
```

## 인증 흐름

```
BPF 패킷 처리
  └─ auth_map 조회
       ├── VALID → PASS
       └── 미인증/만료 → signalmap에 기록
                              │
                         AuthManager
                         1. 중복 인증 방지 (pending)
                         2. Backoff 확인 (storedAt + backoffTime)
                         3. mutualAuthHandler.authenticate()
                            └─ TCP 연결 → TLS 1.3 mTLS 핸드셰이크
                         4. authmap.Update(key, expiration)
```
