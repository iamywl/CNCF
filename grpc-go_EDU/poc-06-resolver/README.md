# PoC-06: 이름 해석 및 주소 갱신

## 개념

gRPC의 이름 해석(Name Resolution) 시스템과 리졸버 → 밸런서 업데이트 흐름을 시뮬레이션한다.

```
URI 파싱                 레지스트리              리졸버               밸런서
"dns:///svc.example.com"
  │                    ┌──────────┐         ┌──────────┐        ┌──────────┐
  ├─ scheme="dns" ────▶│ Get("dns")│────────▶│ dnsResolver│       │          │
  │                    └──────────┘         │  watcher() │──────▶│UpdateState│
  │                                         │  resolve() │       │          │
  │                                         └──────────┘        └──────────┘
  │                                              ▲
  └─ ResolveNow() ──────────────────────────────┘ (즉시 재해석 힌트)
```

## 시뮬레이션하는 gRPC 구조

| 구조체/함수 | 실제 위치 | 역할 |
|------------|----------|------|
| `Builder` | `resolver/resolver.go:301` | Resolver 팩토리 인터페이스 |
| `Resolver` | `resolver/resolver.go:319` | 이름 → 주소 해석 |
| `Target` | `resolver/resolver.go` | 파싱된 URI (Scheme/Authority/Endpoint) |
| `ClientConn` | `resolver/resolver.go` | 리졸버 결과 수신 인터페이스 |
| `Register/Get` | `resolver/resolver.go` | 스킴별 Builder 레지스트리 |
| `dnsResolver` | `dns_resolver.go` | DNS 기반 리졸버 (주기적 갱신) |
| `passthrough` | `passthrough.go` | 주소를 그대로 전달 |

## 실행 방법

```bash
cd poc-06-resolver
go run main.go
```

## 예상 출력

```
=== 이름 해석 및 주소 갱신 시뮬레이션 ===

── 1. Builder 레지스트리 ──
[레지스트리] Builder 등록: scheme=dns
[레지스트리] Builder 등록: scheme=passthrough

── 2. Target 파싱 ──
  'dns:///myservice.example.com' → scheme=dns, authority='', endpoint='myservice.example.com'
  'passthrough:///10.0.0.1:8080' → scheme=passthrough, authority='', endpoint='10.0.0.1:8080'
  '10.0.0.2:9090' → scheme=passthrough, authority='', endpoint='10.0.0.2:9090'

── 4. DNS 리졸버 (초기 해석) ──
  [DNS] 업데이트 #1: 주소=[10.0.1.1:8080 10.0.1.2:8080 10.0.1.3:8080]

── 5. DNS 레코드 변경 (스케일 아웃) ──
  [DNS] 업데이트 #2: 주소=[10.0.1.1:8080 ... 10.0.1.5:8080]

── 6. ResolveNow (즉시 재해석) ──
  [DNS] 업데이트: 주소=[10.0.1.1:8080 10.0.1.5:8080]
...

=== 시뮬레이션 완료 ===
```

## 핵심 포인트

1. **스킴 기반 레지스트리**: `dns://`, `passthrough://`, 커스텀 스킴을 Builder로 등록
2. **DNS 리졸버 watcher**: 주기적으로(기본 30분) DNS를 다시 해석하여 주소 변경을 감지
3. **ResolveNow**: 밸런서가 리졸버에게 즉시 재해석을 요청하는 힌트 (보장은 아님)
4. **Passthrough**: 해석 없이 주소를 그대로 전달 (개발/테스트용)
5. **확장성**: Builder 인터페이스를 구현하면 Consul, etcd 등 커스텀 서비스 디스커버리 가능
