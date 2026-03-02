# PoC: Hubble CIDR/IP 네트워크 필터링 패턴

## 관련 문서
- [05-API-REFERENCE.md](../05-API-REFERENCE.md) - IP 필터 플래그
- [07-CODE-GUIDE.md](../07-CODE-GUIDE.md) - 필터 시스템 구현

## 개요

Hubble은 `net/netip` 패키지를 사용하여 IP 주소와 CIDR로 Flow를 필터링합니다:
- **개별 IP**: `--ip-source 10.244.0.5`
- **CIDR**: `--ip-source 10.244.0.0/24`
- **혼합**: 개별 IP와 CIDR 동시 사용

## 실행

```bash
go run main.go
```

## 시나리오

### 시나리오 1: 개별 IP 필터
정확한 IP 주소 매칭

### 시나리오 2: CIDR 필터
`10.244.0.0/24` → `10.244.0.x` 범위 내 모든 IP 매치

### 시나리오 3: 혼합 필터
개별 IP + CIDR을 OR 조건으로 조합

### 시나리오 4: src + dst 동시 필터
소스 CIDR + 목적지 개별 IP를 AND 조건으로 조합

## 핵심 학습 내용
- `net/netip` 패키지: 값 타입 IP/CIDR (Go 1.18+)
- `netip.ParseAddr`: IP 주소 파싱
- `netip.ParsePrefix`: CIDR 파싱
- `Prefix.Contains(Addr)`: O(1) CIDR 포함 검사
- `netip.Addr` vs `net.IP`: 값 타입, == 비교 가능, 맵 키 사용 가능
