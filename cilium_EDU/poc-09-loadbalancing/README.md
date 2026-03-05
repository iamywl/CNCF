# PoC-09: Maglev 로드밸런싱 시뮬레이션

## 개요

Cilium의 eBPF 기반 Maglev 일관된 해싱 로드밸런서를 시뮬레이션한다.

## 시뮬레이션 대상

| 컴포넌트 | 실제 소스 | 시뮬레이션 |
|----------|----------|-----------|
| Maglev 해싱 | `pkg/maglev/maglev.go` (getOffsetAndSkip, computeLookupTable) | offset+skip 기반 순열 → 룩업 테이블 생성 |
| Frontend/Backend | `pkg/loadbalancer/{frontend,backend,service}.go` | 서비스 타입, 백엔드 상태 관리 |
| 세션 어피니티 | BPF `lb4_affinity_map` (LRU hash) | 클라이언트 IP 기반 LRU 타임아웃 |
| 가중치 분배 | `computeLookupTable()` weightCntr 로직 | Envoy 영감 가중치 턴 선택 |

## 핵심 알고리즘

```
Maglev Lookup Table 생성 (크기 M, 백엔드 N개):
  1. 각 백엔드 b_i에 대해:
     offset_i = h1(b_i) % M
     skip_i   = (h2(b_i) % (M-1)) + 1
     perm[i][j] = (offset_i + j * skip_i) % M
  2. 라운드 로빈으로 빈 슬롯 채우기:
     for n = 0..M-1:
       i = n % N
       entry[perm[i][next[i]]] = b_i.ID  (빈 슬롯 찾을 때까지)
  3. 패킷 선택: entry[flowHash % M]
```

## 실행

```bash
go run main.go
```

## 데모 항목

1. **Maglev 기본 동작**: 균등 분배 검증
2. **최소 재분배**: 백엔드 추가/제거 시 변경 비율 (일관된 해싱)
3. **가중치 분배**: Weight 200/100/50 비율 검증
4. **세션 어피니티**: 동일 클라이언트 → 동일 백엔드 (LRU 타임아웃)
5. **Graceful 제거**: Active → Terminating 상태 전이와 폴백
6. **해시 일관성**: 입력 순서와 무관한 동일 룩업 테이블
7. **재분배 비율 요약**: N 변경 시 실제 vs 이상적 disruption
