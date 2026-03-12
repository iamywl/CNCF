# PoC 10: DNS 메트릭 (DNS Metrics)

## 개요

CoreDNS의 prometheus 플러그인이 수집하는 DNS 메트릭 시스템을 시뮬레이션한다. Counter, Histogram, Gauge 메트릭 타입을 구현하고, Prometheus exposition 형식으로 출력한다.

## CoreDNS 메트릭 시스템

CoreDNS는 `plugin/metrics/vars/vars.go`에서 전역 메트릭 변수를 정의하고, `vars/report.go`의 `Report()` 함수를 통해 각 DNS 요청/응답에 대한 메트릭을 수집한다.

### 핵심 메트릭

| 메트릭 | 타입 | 레이블 | 설명 |
|--------|------|--------|------|
| `coredns_dns_requests_total` | Counter | server, zone, proto, family, type | 전체 DNS 요청 수 |
| `coredns_dns_responses_total` | Counter | server, zone, rcode, plugin | 응답 코드별 응답 수 |
| `coredns_dns_request_duration_seconds` | Histogram | server, zone | 요청 처리 시간 |
| `coredns_dns_request_size_bytes` | Histogram | server, zone, proto | 요청 크기 |
| `coredns_dns_response_size_bytes` | Histogram | server, zone, proto | 응답 크기 |
| `coredns_dns_do_requests_total` | Counter | server, zone | DNSSEC DO 비트 요청 수 |
| `coredns_plugin_enabled` | Gauge | server, zone, name | 플러그인 활성화 여부 |

### Report() 함수 흐름

```
DNS 요청 수신 → 플러그인 체인 처리 → Report() 호출
  ├── RequestCount.Inc(server, zone, proto, family, qtype)
  ├── ResponseRcode.Inc(server, zone, rcode, plugin)
  ├── RequestDuration.Observe(duration, server, zone)
  ├── RequestSize.Observe(reqSize, server, zone, proto)
  ├── ResponseSize.Observe(respSize, server, zone, proto)
  └── RequestDo.Inc(server, zone)  [DO 비트 설정 시]
```

## 시뮬레이션 내용

1. **메트릭 타입 구현**: Counter, Histogram, Gauge (Prometheus 규격 준수)
2. **메트릭 레지스트리**: CoreDNS와 동일한 메트릭 세트 초기화
3. **Report() 함수**: 요청/응답 정보를 메트릭으로 기록
4. **쿼리 시뮬레이션**: 50개 랜덤 DNS 쿼리 생성 및 메트릭 수집
5. **통계 요약**: Zone별, Rcode별, 응답 시간별 분포
6. **Prometheus 형식 출력**: `/metrics` 엔드포인트 시뮬레이션

## 실행

```bash
go run main.go
```

## 출력 예시

```
=== CoreDNS DNS 메트릭 (DNS Metrics) PoC ===

--- 2. DNS 쿼리 시뮬레이션 (50개 쿼리) ---
  총 50개 쿼리 처리 완료

--- 3. 메트릭 요약 통계 ---
  [requests_total] Zone별 요청 수:
    zone=example.com.   →  18 요청
    zone=test.io.       →  15 요청

--- 4. Prometheus /metrics 출력 ---
# HELP coredns_dns_requests_total Counter of DNS requests made per zone, protocol and family.
# TYPE coredns_dns_requests_total counter
coredns_dns_requests_total{server="dns://:53",zone="example.com.",proto="udp",family="1",type="A"} 5
```
