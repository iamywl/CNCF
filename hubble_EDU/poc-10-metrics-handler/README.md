# PoC-10: 메트릭 핸들러 (FlowProcessor)

## 개요

Hubble의 메트릭 수집 시스템을 시뮬레이션한다. `FlowProcessor`가 디코딩된 Flow를 수신하면, 등록된 모든 `MetricHandler`에 순차적으로 전달하여 프로토콜별 메트릭(DNS, HTTP, TCP, Drop)을 추출한다.

실제 Hubble에서는 Prometheus 클라이언트 라이브러리로 Counter, Histogram 등을 등록하지만, 이 PoC에서는 `sync/atomic`과 map 기반으로 동일한 패턴을 재현한다.

## 핵심 개념

### 1. MetricHandler 인터페이스

모든 메트릭 핸들러는 동일한 인터페이스를 구현한다:

```go
type MetricHandler interface {
    Init() error                                    // 메트릭 등록
    ProcessFlow(ctx context.Context, flow *Flow) error  // Flow 처리
    Status() string                                 // 핸들러 이름
    Deinit()                                       // 정리
}
```

### 2. FlowProcessor (StaticFlowProcessor)

`OnDecodedFlow()`가 호출되면 등록된 모든 핸들러의 `ProcessFlow()`를 순차 호출한다. 하나의 핸들러가 실패해도 나머지 핸들러는 계속 실행된다.

### 3. 프로토콜별 메트릭

- **DNS**: `hubble_dns_queries_total`, `hubble_dns_responses_total`, `hubble_dns_response_types_total`
- **HTTP**: `hubble_http_requests_total`, `hubble_http_responses_total`, `hubble_http_request_duration_seconds`
- **TCP**: `hubble_tcp_flags_total`
- **Drop**: `hubble_drop_total` (사유별 분류)

### 4. 라벨 기반 메트릭 분류

CounterVec/HistogramVec로 라벨 조합별로 독립적인 카운터를 관리한다. 예를 들어 HTTP 응답은 `{method, protocol, status_code, reporter}` 라벨 조합으로 분류된다.

## 실행 방법

```bash
go run main.go
```

DNS/HTTP/TCP/Drop 핸들러를 초기화하고, 테스트 Flow를 생성하여 처리한 뒤 Prometheus 형식으로 메트릭 결과를 출력한다.

## 실제 소스코드 참조

| 파일 | 핵심 함수/구조체 |
|------|-----------------|
| `cilium/pkg/hubble/metrics/flow_processor.go` | `StaticFlowProcessor.OnDecodedFlow()` - 핸들러 순차 호출 |
| `cilium/pkg/hubble/metrics/api/api.go` | `Handler` 인터페이스 - Init/ProcessFlow/Status/Deinit |
| `cilium/pkg/hubble/metrics/dns/handler.go` | `dnsHandler.ProcessFlow()` - DNS 메트릭 추출 |
| `cilium/pkg/hubble/metrics/http/handler.go` | `httpHandler.ProcessFlow()` - HTTP 메트릭 추출 |
| `cilium/pkg/hubble/metrics/drop/handler.go` | `dropHandler.ProcessFlow()` - Drop 메트릭 추출 |
| `cilium/pkg/hubble/metrics/tcp/handler.go` | `tcpHandler.ProcessFlow()` - TCP 메트릭 추출 |
