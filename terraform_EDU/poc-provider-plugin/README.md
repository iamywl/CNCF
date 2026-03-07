# PoC: Terraform Provider 플러그인 시스템 시뮬레이션

## 개요

Terraform의 프로바이더 플러그인 시스템을 시뮬레이션한다.
프로바이더는 별도 프로세스로 실행되며 gRPC를 통해 Terraform Core와 통신한다.
이 PoC에서는 파일 시스템을 관리하는 "local" 프로바이더를 구현한다.

## 대응하는 Terraform 소스코드

| 이 PoC | Terraform 소스 | 설명 |
|--------|---------------|------|
| `Provider` 인터페이스 | `internal/providers/provider.go` | Provider 인터페이스 |
| `PluginServer` | `internal/plugin6/serve.go` | gRPC 서버 |
| `PluginClient` | `internal/plugin/plugin.go` | gRPC 클라이언트 |
| `LocalProvider` | 프로바이더 구현체 (별도 레포) | 실제 리소스 관리 |
| `ProviderSchema` | `internal/providers/provider.go` → `GetProviderSchemaResponse` | 스키마 |

## 구현 내용

### 1. Provider 인터페이스
- `GetSchema()`: 프로바이더가 지원하는 리소스 타입과 속성 스키마 반환
- `Configure()`: 인증 정보, 리전 등 프로바이더 설정
- `PlanResourceChange()`: 변경 계획 계산 (Create/Update/Delete/Replace)
- `ApplyResourceChange()`: 실제 변경 적용
- `ReadResource()`: 현재 리소스 상태 읽기

### 2. Local 프로바이더
- 파일을 "리소스"로 관리하는 mock 프로바이더
- `local_file`: 파일 생성/수정/삭제
- `local_sensitive_file`: 민감 파일 관리
- ForceNew 속성 (filename) 변경 시 Replace 동작

### 3. RPC 통신
- 채널 기반 RPC로 gRPC 시뮬레이션
- 요청/응답을 JSON으로 직렬화
- goroutine을 별도 프로세스로 시뮬레이션

### 4. 스키마 캐싱
- 첫 조회 시 RPC 호출, 이후 캐시에서 반환
- `sync.Mutex`로 동시 접근 보호

## 실행 방법

```bash
go run main.go
```

## Plan → Apply 워크플로우

```
1. GetSchema()         → 프로바이더가 지원하는 리소스 타입 확인
2. Configure()         → 인증 정보, 리전 등 설정
3. PlanResourceChange() → 현재 상태 vs 원하는 설정 비교 → 계획
4. ApplyResourceChange() → 실제 인프라 변경 → 새 상태 반환
5. ReadResource()      → 리소스의 현재 실제 상태 확인 (drift 감지)
```

## 핵심 포인트

- 프로바이더를 별도 프로세스로 분리하여 Crash Isolation을 달성한다
- gRPC 기반 통신으로 다양한 언어로 프로바이더를 구현할 수 있다
- 스키마 시스템으로 속성 유효성 검사, ForceNew 판단, Plan 계산이 가능하다
- go-plugin 라이브러리가 프로세스 관리와 gRPC 연결을 자동화한다
