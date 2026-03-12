# PoC-01: CoreDNS 플러그인 체인

## 개요

CoreDNS의 핵심 설계 패턴인 `Plugin func(Handler) Handler` 미들웨어 체인을 시뮬레이션한다.

## CoreDNS 소스코드 참조

| 파일 | 내용 |
|------|------|
| `plugin/plugin.go` | Handler 인터페이스, Plugin 타입, NextOrFailure 함수 |
| `core/dnsserver/server.go:103-128` | 역순 루프 체인 구축 로직 |

## 핵심 개념

### Plugin func(Handler) Handler 패턴

CoreDNS에서 모든 플러그인은 `func(Handler) Handler` 타입의 팩토리 함수로 등록된다:

```
type Plugin func(Handler) Handler
```

이 패턴은 다음 핸들러(next)를 인자로 받아, 자신의 로직을 감싼 새 핸들러를 반환한다.

### 체인 구축 (역순 루프)

```go
// core/dnsserver/server.go
var stack plugin.Handler
for i := len(site.Plugin) - 1; i >= 0; i-- {
    stack = site.Plugin[i](stack)
}
```

역순으로 wrapping하여 첫 번째 플러그인이 가장 바깥(먼저 실행)에 위치한다.

### NextOrFailure

다음 핸들러가 nil이면 SERVFAIL을 반환하는 안전 장치.

## 구현 플러그인

| 플러그인 | 역할 | 유형 |
|----------|------|------|
| ErrorsPlugin | 하위 오류 감지/로깅 | 비터미널 (래퍼) |
| LogPlugin | 요청/응답 로깅 | 비터미널 (래퍼) |
| CachePlugin | 응답 캐싱 | 조건부 (캐시 히트 시 중단) |
| EchoPlugin | 레코드 응답 생성 | 터미널 (fallthrough 지원) |

## 실행

```bash
go run main.go
```

## 예상 출력

- 첫 번째 쿼리: 전체 체인 통과 (errors → log → cache miss → echo)
- 동일 쿼리 반복: 캐시 히트로 echo 호출 생략
- 존재하지 않는 레코드: echo fallthrough → SERVFAIL
