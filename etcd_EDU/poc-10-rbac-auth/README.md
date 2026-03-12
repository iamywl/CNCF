# PoC-10: RBAC 인증/인가 (Role-Based Access Control)

## 개요

etcd의 역할 기반 접근 제어(RBAC) 시스템을 시뮬레이션한다. 사용자, 역할, 키 범위 권한을 관리하고, 인증 토큰 발급 및 권한 검사를 수행한다.

## 핵심 개념

| 개념 | 설명 |
|------|------|
| User | 이름 + 패스워드 해시 + 역할 목록 |
| Role | 이름 + Permission 목록 |
| Permission | 키 범위(key~rangeEnd) + 유형(READ/WRITE/READWRITE) |
| SimpleToken | 인증 성공 시 발급되는 세션 토큰 |
| root 역할 | 자동으로 모든 키에 대한 READWRITE 권한 보유 |

## 키 범위 매칭

| Key | RangeEnd | 의미 |
|-----|----------|------|
| `/app/config` | `""` | 단일 키 `/app/config` |
| `/app/` | `/app0` | 프리픽스 `/app/*` (사전순 범위) |
| `""` | `"\x00"` | 모든 키 (root 전용) |

## etcd 소스코드 참조

- `server/auth/store.go` — `AuthStore` 인터페이스, `UserAdd`, `Authenticate`, `IsPutPermitted`
- `server/auth/simple_token.go` — `SimpleToken` 생성 및 관리
- `server/auth/range_perm_cache.go` — 키 범위 기반 권한 캐시
- `api/authpb/auth.proto` — `User`, `Role`, `Permission` protobuf 정의

## 실행 방법

```bash
go run main.go
```

## 데모 시나리오

1. root + 일반 사용자(alice, bob, charlie) 생성
2. 역할 생성 및 키 범위 권한 부여
3. 사용자에게 역할 부여 (단일/다중)
4. 인증 활성화 → 패스워드 인증 → 토큰 발급
5. 토큰 검증
6. 권한 검사: 14개 허용/거부 시나리오 테스트
7. 키 범위 매칭 원리 시연
8. 에러 케이스 (중복 생성, 잘못된 인증 등)
