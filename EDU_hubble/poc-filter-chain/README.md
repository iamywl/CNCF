# PoC: Hubble Filter Chain 패턴

> **관련 문서**: [07-CODE-GUIDE.md](../07-CODE-GUIDE.md) - Filter Chain 패턴, [03-DATA-MODEL.md](../03-DATA-MODEL.md) - 필터 모델, [05-API-REFERENCE.md](../05-API-REFERENCE.md) - Observe 필터 시스템

## 이 PoC가 보여주는 것

Hubble의 **Whitelist/Blacklist 필터 시스템**을 4가지 시나리오로 실행해볼 수 있습니다.

```
필터 로직:
  Whitelist(OR): [FlowFilter1 OR FlowFilter2]
                  ↓ 하나라도 매치하면 통과
  Blacklist(OR): [FlowFilter3 OR FlowFilter4]
                  ↓ 하나라도 매치하면 제외

  FlowFilter 내부(AND): [Condition1 AND Condition2]
                         ↓ 모두 매치해야 함
```

## 실행 방법

```bash
cd EDU/poc-filter-chain
go run main.go
```

## 4가지 시나리오

1. `--verdict DROPPED` → 단일 조건 whitelist
2. `--source-pod frontend --protocol tcp` → AND 조건 (두 조건 모두 매치)
3. 여러 whitelist → DROPPED **OR** DNS (OR 조건)
4. Whitelist + Blacklist → TCP이면서 untrusted 네임스페이스 제외

## 핵심 학습 포인트

- **FilterFunc**: 가장 작은 단위 (하나의 조건)
- **FlowFilter**: FilterFunc들의 AND 결합
- **Whitelist**: FlowFilter들의 OR 결합 (하나라도 매치 → 포함)
- **Blacklist**: FlowFilter들의 OR 결합 (하나라도 매치 → 제외)
- **실행 순서**: Whitelist 확인 → Blacklist 확인 → 통과/거부
