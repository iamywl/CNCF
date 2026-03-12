# PoC-05: DNS 캐시 서버

CoreDNS 캐시 플러그인(`plugin/cache/`)의 핵심 알고리즘을 시뮬레이션하는 PoC.

## 재현하는 CoreDNS 내부 구조

| 구성 요소 | 실제 소스 위치 | PoC 재현 내용 |
|-----------|---------------|--------------|
| Cache 구조체 | `plugin/cache/cache.go` | 이중 캐시(pcache/ncache), FNV-64 해시 키 |
| item 구조체 | `plugin/cache/item.go` | TTL 계산, 빈도 추적 |
| ServeDNS | `plugin/cache/handler.go` | 캐시 조회/저장 흐름, 프리페치 판단 |
| ShardedCache | `plugin/pkg/cache/cache.go` | 256-shard 분할, 랜덤 축출 |

## 핵심 개념

### 이중 캐시 (Dual Cache)
- **양성 캐시 (pcache)**: 성공 응답 (NoError, Delegation) 저장
- **음성 캐시 (ncache)**: 실패 응답 (NXDomain, NoData, ServerError) 저장
- 각 캐시는 독립적인 TTL 범위를 가짐

### 캐시 키 생성 (FNV-64 해시)
```
hash = FNV64(DO_bit + CD_bit + qtype + lowercase(qname))
shard = hash & 0xFF  (하위 8비트로 256개 샤드 선택)
```

### 프리페치
- 조건: 히트 수 >= threshold AND 남은 TTL <= origTTL * percentage%
- 백그라운드 고루틴에서 원본 서버에 재조회하여 캐시 갱신
- 빈도 정보(Hits)를 새 아이템에 복사

### TTL 클램프 (computeTTL)
- 응답의 TTL을 min/max 범위로 제한
- 양성: min=5s, max=3600s
- 음성: min=5s, max=1800s

## 실행 방법

```bash
go run main.go
```

## 데모 내용

1. **기본 캐시 히트/미스**: 첫 쿼리는 미스, 이후는 캐시 히트
2. **음성 캐시**: NXDOMAIN 응답도 캐싱하여 반복 조회 방지
3. **FNV-64 해시 키**: qname/qtype/DO/CD에 따른 키 생성
4. **TTL 감소**: 시간 경과에 따른 TTL 감소 관찰
5. **프리페치**: 만료 임박 시 백그라운드 갱신
6. **히트율 통계**: 요청/히트/미스 카운터
7. **256-샤드 분포**: 도메인별 샤드 할당
8. **TTL 클램프**: computeTTL 범위 제한
