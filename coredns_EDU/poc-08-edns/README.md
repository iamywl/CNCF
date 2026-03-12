# PoC-08: EDNS 처리

CoreDNS의 EDNS0 (Extension mechanisms for DNS, RFC 6891) 처리 메커니즘을 시뮬레이션하는 PoC.

## 재현하는 CoreDNS 내부 구조

| 구성 요소 | 실제 소스 위치 | PoC 재현 내용 |
|-----------|---------------|--------------|
| Size() | `request/request.go` | UDP 버퍼 크기 정규화 |
| Do() | `request/request.go` | DNSSEC OK 플래그 추출 |
| SizeAndDo() | `request/request.go` | 요청 → 응답 EDNS 전파 |
| Scrub() | `request/request.go` | 응답 크기 제한, TC 비트, 압축 |
| ScrubWriter | `request/writer.go` | SizeAndDo + Scrub 자동 적용 데코레이터 |
| supportedOptions() | `request/edns0.go` | 지원 옵션 필터링 |

## 핵심 개념

### OPT RR (RFC 6891)
```
Name:  "." (루트)
Type:  41 (OPT)
Class: UDP 페이로드 크기
TTL:   [ExtRcode:8][Version:8][DO:1][Z:15]
RDATA: [OptCode:16][OptLen:16][OptData:N]...
```

### UDP 버퍼 크기 협상
- TCP: 항상 65535
- EDNS 없음: 512 (레거시 DNS)
- EDNS: max(512, min(advertised, 4096))

### Scrub 패턴
1. 메시지를 클라이언트 버퍼 크기로 자름 (Extra → Ns → Answer 순)
2. 잘린 경우 TC (Truncated) 비트 설정
3. UDP 단편화 방지: IPv4 > 1480B 또는 IPv6 > 1220B 시 압축 활성화

### 지원 EDNS0 옵션
| 코드 | 이름 | 지원 |
|------|------|------|
| 3 | NSID | O |
| 9 | EXPIRE | O |
| 10 | COOKIE | O |
| 11 | TCP_KEEPALIVE | O |
| 12 | PADDING | O |
| 8 | CLIENT_SUBNET | X |

## 실행 방법

```bash
go run main.go
```

## 데모 내용

1. **OPT RR 구조**: 바이트 레벨 인코딩/디코딩
2. **TTL 비트 레이아웃**: ExtRcode, Version, DO 플래그 인코딩
3. **UDP 버퍼 크기 협상**: 프로토콜별 크기 정규화
4. **SizeAndDo**: 요청 EDNS를 응답에 전파, 미지원 옵션 제거
5. **Scrub 패턴**: 버퍼 크기 초과 시 레코드 제거 + TC 비트
6. **DO 플래그와 AD 비트**: DNSSEC 관련 플래그 상호작용
7. **단편화 임계값**: IPv4/IPv6별 압축 활성화 기준
8. **ScrubWriter 패턴**: 데코레이터 패턴으로 자동 Scrub 적용
