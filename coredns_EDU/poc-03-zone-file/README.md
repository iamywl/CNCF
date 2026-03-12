# PoC-03: CoreDNS Zone 파일 파서

## 개요

RFC 1035 형식의 Zone 파일을 파싱하고 검색하는 기능을 시뮬레이션한다.

## CoreDNS 소스코드 참조

| 파일 | 내용 |
|------|------|
| `plugin/file/file.go:148` | Parse 함수 - Zone 파일 파싱 |
| `plugin/file/lookup.go:33` | Zone.Lookup - 레코드 검색, 와일드카드, CNAME 체이싱 |
| `plugin/file/tree/tree.go` | LLRB 트리 기반 레코드 저장 |

## 핵심 개념

### Zone 파일 형식 (RFC 1035)
```
$ORIGIN example.com.
$TTL 3600
@   IN  SOA  ns1.example.com. admin.example.com. 2024010101 7200 3600 1209600 86400
www IN  A    93.184.216.34
```

### 지원 레코드 타입
| 타입 | 설명 |
|------|------|
| SOA | 존 권한 시작 |
| NS | 네임서버 |
| A | IPv4 주소 |
| AAAA | IPv6 주소 |
| CNAME | 정식 이름 (별칭) |
| MX | 메일 교환기 |
| TXT | 텍스트 레코드 |

### Lookup 로직
1. 정확한 이름 매칭
2. CNAME 체이싱 (CNAME → 대상 레코드 재귀 조회)
3. 와일드카드 매칭 (첫 레이블을 `*`로 치환)

### 결과 코드
- `NOERROR`: 레코드 발견
- `NXDOMAIN`: 이름 자체가 존재하지 않음
- `NODATA`: 이름은 존재하지만 요청 타입 없음

## 실행

```bash
go run main.go
```
