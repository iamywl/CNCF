# PoC-05: Hubble 필터 체인

## 개요

Hubble의 Whitelist/Blacklist 필터 모델을 시뮬레이션한다. FilterFunc 함수 타입을 기반으로 IP, Port, Verdict, Label, Protocol 등 다양한 필터를 합성하고 적용하는 과정을 재현한다.

## 실제 소스코드 참조

| 파일 | 역할 |
|------|------|
| `pkg/hubble/filters/filters.go` | FilterFunc, FilterFuncs, Apply(), MatchOne(), MatchAll(), MatchNone(), BuildFilterList() |
| `pkg/hubble/filters/ip.go` | IPFilter, filterByIPs() - IP 주소/CIDR 매칭 |
| `pkg/hubble/filters/labels.go` | LabelsFilter, FilterByLabelSelectors() - 라벨 셀렉터 매칭 |
| `pkg/hubble/filters/verdict.go` | VerdictFilter |
| `pkg/hubble/filters/port.go` | PortFilter |

## 핵심 개념

### 1. FilterFunc 타입

```go
type FilterFunc func(ev *v1.Event) bool
type FilterFuncs []FilterFunc
```

필터는 단순한 함수. 이벤트를 받아 bool을 반환한다.

### 2. Whitelist/Blacklist 모델

```go
func Apply(whitelist, blacklist FilterFuncs, ev *Event) bool {
    return whitelist.MatchOne(ev) && blacklist.MatchNone(ev)
}
```

- **Whitelist** (OR): 하나라도 매치하면 통과, 비어있으면 모두 통과
- **Blacklist** (NOR): 하나라도 매치하면 차단, 비어있으면 모두 통과

### 3. FlowFilter 내부는 AND, FlowFilter 간은 OR

```
FlowFilter{srcIP=A, dstPort=B}  →  A AND B (모두 매치해야 통과)
[FlowFilter1, FlowFilter2]      →  1 OR 2  (하나라도 매치하면 통과)
```

### 4. IP 필터의 CIDR 지원

```go
// 정확한 IP 매칭과 CIDR 범위 매칭을 모두 지원
if strings.Contains(ip, "/") {
    prefix, _ := netip.ParsePrefix(ip)  // CIDR
} else {
    addresses = append(addresses, ip)     // 정확한 매칭
}
```

## 실행 방법

```bash
go run main.go
```

## 학습 포인트

1. **함수형 필터**: FilterFunc로 필터를 1급 함수로 취급
2. **합성 패턴**: 개별 필터를 MatchAll/MatchOne/MatchNone으로 합성
3. **빌더 패턴**: OnBuildFilter 인터페이스로 FlowFilter → FilterFunc 변환
4. **CIDR 매칭**: 정확한 IP와 서브넷 범위 매칭을 동시 지원
