# PoC 03: 패킷 처리 파이프라인 시뮬레이션

Cilium BPF 데이터패스의 패킷 처리 과정을 Go 코드로 시뮬레이션한다.
CT 조회 → 정책 평가 → CT 생성 → 전달/드롭의 전체 과정을 체험한다.

---

## 핵심 매커니즘

```
패킷 도착
    │
    ▼
[CT 조회] ─── HIT ──► 기존 연결, 바로 전달
    │
   MISS (새 연결)
    │
    ▼
[Policy 조회] ─── DENY ──► DROP (패킷 드롭, Hubble에 이벤트 기록)
    │
  ALLOW
    │
    ▼
[CT 엔트리 생성] ──► 이후 같은 연결은 CT HIT로 바로 전달
    │
    ▼
[패킷 전달] ──► 목적지로 리다이렉트 (tail call)
```

핵심 포인트:
- 첫 패킷만 정책을 평가하고, 이후 패킷은 CT 캐시로 빠르게 처리
- 이것이 iptables 대비 Cilium이 빠른 이유 중 하나

## 실행 방법

```bash
cd EDU/poc-03-packet-pipeline
go run main.go
```
