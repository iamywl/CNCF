# PoC-02: CoreDNS DNS 서버

## 개요

UDP/TCP 듀얼 프로토콜 DNS 서버를 구현하여 CoreDNS의 서버 엔진을 시뮬레이션한다.

## CoreDNS 소스코드 참조

| 파일 | 내용 |
|------|------|
| `core/dnsserver/server.go` | Server 구조체, ServeDNS 존 라우팅 |
| `core/dnsserver/server.go:295-330` | 레이블별 최장 매칭 존 탐색 |
| `core/dnsserver/server.go:148-183` | UDP/TCP 리스너 설정 |

## 핵심 개념

### DNS 메시지 바이너리 형식 (RFC 1035)
- 헤더: 12바이트 고정 (ID, Flags, 각 섹션 카운트)
- 질문 섹션: 이름(가변) + QTYPE(2) + QCLASS(2)
- 이름 인코딩: `[길이][문자열]...[0]` 형식

### Zone 기반 라우팅 (최장 매칭)
CoreDNS는 쿼리 이름의 레이블을 오른쪽부터 순회하며 가장 긴 매칭 존을 찾는다:
```
쿼리: app.internal.example.com.
  → "app.internal.example.com." 체크
  → "internal.example.com." 매칭! (example.com.보다 더 구체적)
```

### UDP vs TCP
- UDP: 512바이트 제한, 빠른 응답
- TCP: 2바이트 길이 프리픽스 + 메시지, 대용량 응답

## 실행

```bash
go run main.go
```

## 구현 기능

1. DNS 메시지 바이너리 파싱/직렬화
2. UDP/TCP 듀얼 서버
3. Zone 기반 최장 매칭 라우팅
4. A 레코드 응답 생성
5. 내장 dig 시뮬레이터
