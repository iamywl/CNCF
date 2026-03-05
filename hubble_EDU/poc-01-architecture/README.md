# PoC-01: Hubble 서버 아키텍처

## 개요

Hubble 서버의 초기화 흐름과 gRPC 서비스 등록 구조를 시뮬레이션한다. Hubble 서버는 gRPC 서버 위에 Observer, Peer, Health 세 가지 핵심 서비스를 등록하여 네트워크 관찰 기능을 제공한다.

## 실제 소스코드 참조

| 파일 | 역할 |
|------|------|
| `pkg/hubble/server/server.go` | Server 구조체, NewServer(), initGRPCServer(), Serve(), Stop() |
| `pkg/hubble/server/serveroption/option.go` | Options 구조체, WithTCPListener, WithObserverService 등 옵션 함수 |
| `pkg/hubble/observer/local_observer.go` | LocalObserverServer - Observer 서비스 구현 |
| `pkg/hubble/peer/service.go` | Peer 서비스 구현 |

## 핵심 개념

### 1. Functional Options 패턴

```
type Option func(o *Options) error

func NewServer(log *slog.Logger, options ...serveroption.Option) (*Server, error)
```

서버 생성 시 가변 개수의 옵션 함수를 받아 Options 구조체에 적용한다. 옵션 적용 중 에러가 발생하면 즉시 반환한다.

### 2. 서버 초기화 흐름

```
NewServer()
  ├── 옵션 순차 적용
  ├── Listener 검증 (없으면 errNoListener)
  ├── TLS 검증 (없고 Insecure도 아니면 errNoServerTLSConfig)
  └── initGRPCServer()
        ├── grpc.NewServer() - 인터셉터, TLS 등 설정
        ├── healthpb.RegisterHealthServer()
        ├── observerpb.RegisterObserverServer()
        ├── peerpb.RegisterPeerServer()
        ├── reflection.Register()
        └── GRPCMetrics.InitializeMetrics()
```

### 3. 서비스 등록

실제 코드에서 세 가지 서비스는 조건부로 등록된다:
- **Health**: `opts.HealthService != nil`이면 등록
- **Observer**: `opts.ObserverService != nil`이면 등록
- **Peer**: `opts.PeerService != nil`이면 등록

## 실행 방법

```bash
go run main.go
```

## 학습 포인트

1. **Functional Options 패턴**: Go에서 설정이 많은 구조체를 초기화하는 표준 패턴
2. **서비스 레지스트리**: gRPC 서버에 여러 서비스를 등록하는 구조
3. **검증 우선**: 리스너와 TLS 설정을 먼저 검증하고 실패하면 즉시 에러 반환
4. **관심사 분리**: Observer/Peer/Health가 독립적으로 구현되고 서버에서 조합
