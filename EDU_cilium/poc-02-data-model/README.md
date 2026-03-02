# PoC 02: Identity 기반 정책 결정 매커니즘 체험

Cilium의 핵심 개념 — Label → Identity → Policy → 허용/차단 — 을
코드로 직접 구현하여 동작 원리를 체험한다.

---

## 핵심 매커니즘

```
Pod 라벨 {"app":"frontend"}
         │
         ▼
Identity 할당 (라벨 해시 → 고유 숫자 ID)
         │
         ▼
정책 평가: "Identity 48312는 Identity 48313의 포트 80에 접근 가능한가?"
         │
         ▼
ALLOW 또는 DROP
```

IP 기반이 아닌 **Identity 기반** 정책의 장점:
- Pod IP가 변경되어도 라벨이 같으면 같은 Identity → 정책 재설정 불필요
- CIDR 대신 라벨 셀렉터로 직관적인 정책 정의

## 실행 방법

```bash
cd EDU/poc-02-data-model
go run main.go
```

## 출력 예시

```
=== Cilium Identity 기반 정책 결정 시뮬레이터 ===

[1] Label → Identity 매핑
    Labels: {app:frontend}  → Identity 48312
    Labels: {app:backend}   → Identity 48313
    Labels: {app:redis}     → Identity 48314

[2] IPCache 구성 (IP → Identity)
    10.0.1.5/32  → Identity 48312 (frontend)
    10.0.1.10/32 → Identity 48313 (backend)

[3] 정책 평가
    frontend(48312) → backend(48313):80/TCP  = ALLOW
    frontend(48312) → redis(48314):6379/TCP  = DROP (정책 없음)
    backend(48313)  → redis(48314):6379/TCP  = ALLOW
    world(2)        → frontend(48312):80/TCP = ALLOW
    world(2)        → backend(48313):80/TCP  = DROP (정책 없음)
```
