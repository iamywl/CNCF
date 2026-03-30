# PoC-09: Maglev 로드밸런싱 시뮬레이션

## 개요

Cilium의 eBPF 기반 Maglev 일관된 해싱(Consistent Hashing) 로드밸런서를 Go 표준 라이브러리만으로 시뮬레이션한다.
Google의 Maglev 논문에서 제안한 룩업 테이블 알고리즘, 가중치 분배, 세션 어피니티, 백엔드 상태 관리까지
Cilium의 `pkg/maglev/maglev.go`와 `pkg/loadbalancer/` 패키지의 핵심 동작을 재현한다.

---

## 배경 지식

### Maglev 일관된 해싱이란

Google이 2016년 NSDI에서 발표한 **Maglev** 논문은 소프트웨어 네트워크 로드밸런서를 위한 일관된 해싱 알고리즘을 제안했다.
핵심 아이디어는 크기 M(소수)인 **룩업 테이블**을 미리 계산해두고, 패킷의 5-tuple 해시를 이 테이블에 인덱싱하여
O(1)로 백엔드를 선택하는 것이다. 전통적인 해싱 링(Consistent Hashing Ring)과 비교하면:

- **균등 분배**: 링 기반은 가상 노드 수에 따라 편차가 크지만, Maglev는 테이블 전체를 채우므로 분포가 균일
- **최소 재분배**: 백엔드 N개 중 1개가 변경될 때 이상적으로 약 1/N 슬롯만 재배치
- **O(1) 룩업**: 패킷당 선택은 단순 배열 인덱싱이므로 BPF datapath에 적합

### 왜 BPF 기반 LB인가 (kube-proxy 대체)

kube-proxy는 iptables 규칙 체인으로 로드밸런싱을 수행하여, 서비스 수 증가 시 O(N) 규칙 순회와 conntrack 경합 문제가 발생한다.
Cilium은 kube-proxy를 완전히 대체하여 **eBPF tc 훅**에서 직접 패킷을 처리한다:

- iptables 없이 BPF 맵 조회 한 번으로 백엔드 결정
- Maglev 룩업 테이블을 BPF 맵에 저장하여 커널 내 O(1) 선택
- Connection Tracking도 BPF 맵으로 관리

### 관련 개념

| 개념 | 설명 |
|------|------|
| **DSR (Direct Server Return)** | 응답이 LB를 거치지 않고 백엔드에서 클라이언트로 직접 전송. 대역폭 병목 제거 |
| **SNAT** | 소스 NAT. 백엔드가 LB를 통해 응답 반환. 클라이언트 원본 IP가 숨겨짐 |
| **세션 어피니티** | 동일 클라이언트를 같은 백엔드로 라우팅. BPF LRU 맵으로 매핑 |
| **일관된 해싱** | 백엔드 풀 변경 시 최소한의 매핑만 변경하는 해싱 기법 |

---

## 시뮬레이션하는 개념

| 컴포넌트 | 실제 Cilium 소스 | 시뮬레이션 내용 |
|----------|-----------------|----------------|
| Maglev 해싱 | `pkg/maglev/maglev.go` (getOffsetAndSkip, computeLookupTable) | offset+skip 기반 순열 생성 → 라운드 로빈으로 룩업 테이블 채우기 |
| Frontend/Backend 모델 | `pkg/loadbalancer/{frontend,backend,service}.go` | 서비스 타입, 백엔드 상태(Active/Terminating/Quarantined/Maintenance) |
| 세션 어피니티 | BPF `lb4_affinity_map` (LRU hash map) | 클라이언트 IP 기반 LRU 맵 + 타임아웃 만료 |
| 가중치 분배 | `computeLookupTable()` weightCntr 로직 | 가중치에 비례한 턴 할당으로 슬롯 분배 |
| 백엔드 상태 전이 | `BackendState` + Terminating 폴백 | Active만 선택, 전부 종료 시 Terminating 폴백 |
| 해시 일관성 | hashString 기준 정렬 | 입력 순서 무관하게 모든 노드에서 동일 테이블 생성 |

---

## 아키텍처 / 흐름 다이어그램

```
패킷 수신 (tc ingress)
       │
       ▼
┌──────────────────────┐
│ lb4_lookup_service() │ ← BPF 맵에서 Frontend(VIP:Port) 조회
│  서비스 조회          │
└──────┬───────────────┘
       │ 서비스 발견
       ▼
┌──────────────────────┐     ┌─────────────────────┐
│ lb4_affinity_lookup()│────▶│ SessionAffinityMap   │
│  세션 어피니티 확인   │     │ (BPF LRU hash map)  │
└──────┬───────────────┘     └─────────────────────┘
       │ 미스 (신규 연결)
       ▼
┌──────────────────────┐     ┌─────────────────────────────────┐
│ svc_lookup_maglev()  │────▶│ Maglev Lookup Table (크기 M)    │
│  flowHash % M 인덱싱 │     │ [b2|b1|b3|b1|b2|b3|b1|b2|...] │
└──────┬───────────────┘     └─────────────────────────────────┘
       │ 백엔드 결정
       ▼
┌──────────────────────┐
│ CT(ConnTrack) 업데이트│ ← 이후 동일 연결은 CT에서 처리
│ + 어피니티 맵 저장    │
└──────┬───────────────┘
       │
       ▼
  백엔드로 패킷 전달
  (DNAT 또는 DSR)
```

### Maglev 룩업 테이블 생성 예시 (M=7, 백엔드 3개)

```
1) offset/skip:  b0=(3,2) → [3,5,0,2,4,6,1]
                 b1=(0,3) → [0,3,6,2,5,1,4]
                 b2=(5,1) → [5,6,0,1,2,3,4]

2) 라운드 로빈:  n=0 b0→[3]  n=1 b1→[0]  n=2 b2→[5]  n=3 b0→[2] ...
   결과: [b1|_|b0|b0|_|b2|_] → ... → 모든 슬롯 채움

3) 패킷 선택: entry[flowHash % 7]
```

---

## 코드 해설

### 핵심 구조체 및 함수

**1. `Backend` 구조체 / `hashString()`** --
백엔드의 ID, 주소, 가중치, 상태를 보유. `hashString()`은 `[10.0.1.1:80/TCP,State:active]` 형식 문자열을 반환하며, 이 값이 Maglev 순열 계산의 입력이 된다. 모든 노드에서 동일 형식을 써야 테이블이 일치한다.

**2. `getOffsetAndSkip()`** --
백엔드 hashString으로부터 두 독립 해시(h1, h2)를 계산하여 `offset = h1 % M`, `skip = (h2 % (M-1)) + 1`을 반환한다. skip이 0이면 순열이 진행되지 않으므로 반드시 +1.

**3. `GetLookupTable()`** --
백엔드 목록을 받아 크기 M의 룩업 테이블을 반환한다. hashString 기준 정렬 → permutation 계산 → 라운드 로빈으로 빈 슬롯 채우기. 가중치 설정 시 weightCntr로 턴 빈도를 조절한다.

**4. `SessionAffinityMap`** --
BPF LRU hash map 시뮬레이션. `(clientIP, serviceAddr)` → `(backendID, timestamp)` 매핑. 타임아웃 경과 항목은 조회 시 무효, 최대 크기 도달 시 만료 항목 제거.

**5. `SelectBackend()`** --
BPF datapath 흐름 재현: Active 백엔드 필터링 → 어피니티 맵 조회 → Maglev `flowHash % M` 선택 → 어피니티 업데이트. 모든 백엔드가 Terminating이면 폴백으로 사용.

---

## 실행 방법 및 예상 출력

```bash
cd cilium_EDU/poc-09-loadbalancing
go run main.go
```

### 예상 출력 (요약)

```
━━━ 데모 1: Maglev 해싱 기본 동작 ━━━
  백엔드 분포: 84/84/83 (≈33.3% 균등)

━━━ 데모 2: 최소 재분배 ━━━
  3→4개 추가: 변경 ≈25% (이상적 25.0%)
  3→2개 제거: 변경 ≈33% (이상적 33.3%)

━━━ 데모 3: 가중치 [200,100,50] → 약 57%/29%/14%

━━━ 데모 4: 세션 어피니티 ━━━
  동일 클라이언트 5회 → 모두 같은 백엔드

━━━ 데모 5: 상태 변경 ━━━
  Terminating 제외, 모두 종료 시 폴백

━━━ 데모 6: 입력 순서 [1,2,3] vs [3,2,1] → 동일 테이블: true

━━━ 데모 7: 재분배 비율 ━━━
  5→6, 5→4, 5→3 모두 이상적 1/N 수준
```

---

## 핵심 포인트

1. **O(M*N) 전처리, O(1) 룩업**: 테이블 생성은 비용이 있지만 패킷당 선택은 단순 배열 인덱싱. BPF datapath에서 분기 없이 동작하므로 지연이 극히 낮다.
2. **최소 재분배**: 백엔드 1개 변경 시 이상적으로 약 1/N 슬롯만 변경. 기존 연결 끊김을 최소화한다.
3. **클러스터 전역 일관성**: hashString 정렬 + 동일 시드로 모든 노드에서 동일 테이블 생성. 깨지면 노드별 라우팅 불일치로 장애 발생.
4. **세션 어피니티 이중 구조**: 첫 요청은 Maglev, 이후는 BPF LRU 맵 O(1) 조회. 타임아웃으로 stale 매핑 자동 정리.
5. **Graceful 상태 전이**: Terminating 백엔드는 신규 요청 제외, 전부 종료 시 폴백으로 사용하여 완전한 서비스 중단 방지.

---

## 실제 Cilium과의 차이점

| 항목 | 이 시뮬레이션 | 실제 Cilium |
|------|-------------|------------|
| **해시 함수** | FNV-1a + MD5 조합 | Murmur3 128-bit (h1, h2 동시 생성) |
| **테이블 크기** | 251 (시뮬레이션용) | 기본 16381, 최대 65521 (모두 소수) |
| **순열 계산** | 단일 스레드 | Worker pool로 병렬 계산 (`pkg/maglev/maglev.go`) |
| **세션 어피니티** | Go map + mutex | BPF LRU hash map (`lb4_affinity_map`), 커널 공간에서 동작 |
| **Connection Tracking** | 미구현 | BPF CT 맵으로 established 연결 상태 추적 |
| **DSR/SNAT** | 미구현 | `bpf_lxc.c`에서 L3/L4 헤더 조작, IPIP/Geneve 터널링 지원 |
| **Health Check** | 미구현 | Quarantined 상태 자동 전환, preferred 백엔드 선택 |
| **해시 시드** | 고정 uint32 | base64 인코딩된 12바이트에서 murmur3 시드 파생, `--bpf-lb-maglev-hash-seed` 옵션 |
| **XDP 가속** | 미구현 | XDP 훅에서 조기 처리로 tc보다 빠른 경로 지원 |