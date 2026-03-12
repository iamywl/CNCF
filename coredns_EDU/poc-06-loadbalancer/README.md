# PoC-06: DNS 로드밸런서

CoreDNS loadbalance 플러그인(`plugin/loadbalance/`)의 DNS 레코드 셔플링 알고리즘을 시뮬레이션하는 PoC.

## 재현하는 CoreDNS 내부 구조

| 구성 요소 | 실제 소스 위치 | PoC 재현 내용 |
|-----------|---------------|--------------|
| roundRobin() | `plugin/loadbalance/loadbalance.go` | 타입별 분류 + Fisher-Yates 셔플 |
| roundRobinShuffle() | `plugin/loadbalance/loadbalance.go` | dns.Id() 기반 셔플 알고리즘 |
| weightedRoundRobin() | `plugin/loadbalance/weighted.go` | 가중치 확률 분포 기반 선택 |
| topAddressIndex() | `plugin/loadbalance/weighted.go` | 누적 확률 기반 첫 번째 주소 선택 |
| WriteMsg() | `plugin/loadbalance/loadbalance.go` | 에러/AXFR 응답 바이패스 |

## 핵심 개념

### 라운드 로빈 셔플
1. 레코드를 타입별로 분류: CNAME / A,AAAA / MX / rest
2. A/AAAA와 MX만 Fisher-Yates 변형으로 셔플
3. **CNAME은 항상 첫 번째 위치 유지** (CNAME 체인 해석에 필수)
4. 재조합 순서: CNAME → rest → address → MX

### 특수 케이스
- 레코드 0~1개: 셔플 불필요
- 레코드 2개: `dns.Id() % 2 == 0`일 때만 스왑 (50% 확률)
- 레코드 3개 이상: Fisher-Yates 변형 전체 셔플

### 가중치 기반 로드밸런싱
1. 각 주소의 가중치 조회 (기본값=1)
2. 가중치 내림차순 정렬
3. [0, 가중치합) 범위 난수로 누적 확률 비교
4. 선택된 주소를 첫 번째 위치로 스왑

### 바이패스 조건
- `Rcode != 0` (에러 응답)
- `AXFR/IXFR` (존 전송)

## 실행 방법

```bash
go run main.go
```

## 데모 내용

1. **라운드 로빈 셔플**: A 레코드 4개 반복 셔플
2. **첫 번째 레코드 분포**: 1000회 시뮬레이션으로 균등 분포 확인
3. **CNAME 순서 유지**: CNAME이 항상 첫 번째인지 검증
4. **MX 레코드 셔플**: MX 레코드도 셔플 대상
5. **가중치 기반**: 5:3:2 가중치 설정으로 선택 빈도 확인
6. **2개 레코드 특수 처리**: 50% 스왑 확률 검증
7. **에러 응답 바이패스**: NXDOMAIN은 셔플하지 않음
