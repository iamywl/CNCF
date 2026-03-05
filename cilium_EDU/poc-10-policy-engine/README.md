# PoC-10: 정책 엔진 시뮬레이션 (SelectorCache + PolicyMap)

## 개요

Cilium 정책 엔진의 핵심 흐름을 시뮬레이션한다:
- Identity 기반 정책 모델
- SelectorCache를 통한 라벨→Identity 매핑
- L3/L4 정책 매칭
- BPF PolicyMap (LPM Trie) 시뮬레이션

## 시뮬레이션 대상

| 컴포넌트 | 실제 소스 | 시뮬레이션 |
|----------|----------|-----------|
| SelectorCache | `pkg/policy/selectorcache.go` | Identity 캐시, 셀렉터 매칭, 동적 업데이트 |
| Repository | `pkg/policy/repository.go` | 규칙 저장, resolvePolicyLocked() |
| PolicyMap | `pkg/policy/mapstate.go` (bitlpm.Trie) | LPM 매칭 시뮬레이션 |
| Identity | `pkg/identity/` | 라벨 집합 → 숫자 ID 매핑 |
| L4Policy | `pkg/policy/l4.go` | L3/L4 필터 매칭 |

## 핵심 흐름

```
CiliumNetworkPolicy
  → Repository.AddRule()
  → resolvePolicyLocked(identity)
    → computePolicyEnforcementAndRules()  // 어떤 규칙이 적용되나?
    → resolveL4Policy()                   // L3/L4 필터 계산
    → PolicyMap (LPM Trie)                // BPF 맵으로 변환
  → 패킷 도착 시 PolicyMap.Lookup()       // O(log n) LPM 매칭
```

## 실행

```bash
go run main.go
```

## 데모 항목

1. **Identity 모델**: 라벨 집합 → NumericIdentity 매핑
2. **SelectorCache**: 셀렉터 → Identity 자동 매칭, 동적 업데이트
3. **L3/L4 정책 매칭**: 규칙 → PolicyMap 엔트리 생성
4. **PolicyMap LPM 매칭**: specificity 기반 우선순위
5. **동적 업데이트**: 새 포드 배포 시 정책 자동 전파
6. **Deny 우선순위**: specific deny > wildcard allow
