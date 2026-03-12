# PoC 09: 역방향 DNS (Reverse DNS)

## 개요

CoreDNS의 역방향 DNS 조회 메커니즘을 시뮬레이션한다. PTR 레코드를 통해 IP 주소에서 도메인 이름으로의 역방향 조회를 수행하며, IPv4(in-addr.arpa)와 IPv6(ip6.arpa) 형식을 모두 지원한다.

## CoreDNS에서의 역방향 DNS 처리

CoreDNS의 `plugin/pkg/dnsutil/reverse.go`에서 역방향 DNS 변환의 핵심 로직을 구현한다.

### 핵심 함수

| 함수 | 역할 |
|------|------|
| `ExtractAddressFromReverse()` | 역방향 이름(in-addr.arpa/ip6.arpa)에서 IP 주소 추출 |
| `IsReverse()` | 이름이 역방향 zone에 속하는지 판단 (0=아님, 1=IPv4, 2=IPv6) |
| `reverse()` | IPv4 옥텟 역순 조합 |
| `reverse6()` | IPv6 니블 역순 조합 (RFC 3596) |

### 역방향 이름 형식

```
IPv4: 192.168.1.10 → 10.1.168.192.in-addr.arpa.
      (옥텟 역순 + .in-addr.arpa.)

IPv6: 2001:db8::567:89ab → b.a.9.8.7.6.5.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.8.b.d.0.1.0.0.2.ip6.arpa.
      (각 니블 역순 + .ip6.arpa.)
```

## 시뮬레이션 내용

1. **IP → 역방향 이름 변환**: IPv4 옥텟 역순, IPv6 니블 역순
2. **역방향 이름 → IP 추출**: `ExtractAddressFromReverse` 알고리즘 재현
3. **IsReverse 판단**: 역방향 zone 유형 식별
4. **역방향 Zone 관리**: Zone 데이터 구조 및 PTR 레코드 저장
5. **PTR 조회 데모**: 역방향 이름으로 도메인 조회
6. **왕복 변환 검증**: IP → 역방향 → IP 무손실 변환 확인

## 실행

```bash
go run main.go
```

## 출력 예시

```
=== CoreDNS 역방향 DNS (Reverse DNS) PoC ===

--- 1. IP → 역방향 이름 변환 ---
  IPv4: 192.168.1.10        → 10.1.168.192.in-addr.arpa.
  IPv4: 10.0.0.1            → 1.0.0.10.in-addr.arpa.

--- 2. 역방향 이름 → IP 추출 ---
  [OK] 54.119.58.176.in-addr.arpa.    → 176.58.119.54

--- 5. PTR 레코드 조회 데모 ---
  쿼리: 10.1.168.192.in-addr.arpa.
    IP: 192.168.1.10       → PTR: web-server.example.com.
```
