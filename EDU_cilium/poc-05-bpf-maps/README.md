# PoC 05: BPF 맵 동작 매커니즘 체험

Cilium이 사용하는 BPF 맵(해시맵, LRU)의 동작 원리를 체험한다.
CT 맵, Policy 맵, Service 맵의 조회/삽입/삭제를 시뮬레이션한다.

---

## 핵심 매커니즘

BPF 맵은 커널과 유저스페이스가 공유하는 **키-값 저장소**다.

```
유저스페이스 (cilium-daemon, Go)
    │
    │ bpf() 시스템콜로 맵 읽기/쓰기
    ▼
┌─────────────────────────────────┐
│  BPF 맵 (커널 메모리)             │
│                                 │
│  CT Map:     5-tuple → 연결상태   │
│  Policy Map: Identity+Port → 허용│
│  Service Map: VIP:Port → Backend │
└─────────────────────────────────┘
    ▲
    │ BPF 프로그램이 직접 읽기/쓰기
    │
커널 (BPF 프로그램, C)
```

맵 종류별 특성:
- **Hash Map**: 정확한 키 매칭. Policy, Service에 사용
- **LRU Hash Map**: 오래된 엔트리 자동 제거. CT, NAT에 사용
- **Array Map**: 인덱스 기반 접근. 설정 값, 통계에 사용

## 실행 방법

```bash
cd EDU/poc-05-bpf-maps
go run main.go
```
