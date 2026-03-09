# PoC: Terraform RPC API 서버 프레임워크 시뮬레이션

## 개요

Terraform의 RPC API 서버 프레임워크를 시뮬레이션한다.
HCP Terraform 등 자동화 도구가 Terraform Core를 프로그래밍 방식으로 제어하기 위한
gRPC 기반 인터페이스의 핵심 패턴을 재현한다.

## 대응하는 Terraform 소스코드

| 이 PoC | Terraform 소스 | 설명 |
|--------|---------------|------|
| `ValidateHandshake()` | `internal/rpcapi/server.go` | 매직 쿠키 핸드셰이크 |
| `HandleTable` | `internal/rpcapi/handles.go` | 제네릭 핸들 테이블 |
| `Stopper` | `internal/rpcapi/stopper.go` | 정지 신호 관리 |
| `RPCServer.Handshake()` | `internal/rpcapi/setup.go` | 능력 협상 |
| `DynServer` | `internal/rpcapi/dynrpcserver/` | 동적 서비스 등록 |
| `CLICommand` | `internal/rpcapi/cli.go` | CLI 진입점 |
| `Telemetry` | `internal/rpcapi/telemetry.go` | OpenTelemetry 통합 |

## 구현 내용

### 1. 매직 쿠키 핸드셰이크
- go-plugin 프레임워크의 환경변수 기반 인증
- 직접 실행 방지 (매직 쿠키가 없으면 거부)

### 2. Handle 테이블
- 정수 핸들로 서버 측 객체 참조 (gRPC 경계 넘김)
- Allocate / Get / Close 생명주기 관리
- 뮤텍스 기반 동시성 안전

### 3. Stopper 패턴
- sync.Once로 한 번만 정지 신호 전파
- 채널 기반 워커 종료 통보

### 4. 능력 협상
- Handshake 시 클라이언트가 필요한 능력을 요청
- 서버가 활성화할 서비스를 결정 (plan, apply, state_inspect)

### 5. 텔레메트리
- RPC 호출별 스팬 기록 (이름, 지속 시간, 오류)

## 실행 방법

```bash
go run main.go
```

## 핵심 포인트

- 매직 쿠키는 플러그인이 올바른 부모 프로세스에서 실행되는지 검증한다
- Handle 테이블은 gRPC 경계를 넘어 서버 측 객체를 안전하게 참조하는 패턴이다
- 핸드셰이크 이후에만 서비스가 활성화되어 불필요한 리소스 초기화를 방지한다
