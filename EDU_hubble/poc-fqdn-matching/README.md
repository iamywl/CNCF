# PoC: Hubble FQDN 패턴 매칭

## 관련 문서
- [05-API-REFERENCE.md](../05-API-REFERENCE.md) - FQDN/DNS 필터 플래그
- [07-CODE-GUIDE.md](../07-CODE-GUIDE.md) - 필터 시스템

## 개요

Hubble은 DNS 이름(FQDN)을 와일드카드 패턴으로 필터링합니다:
- 와일드카드: `*.google.com` → `maps.google.com`, `api.google.com` 매치
- 정확한 매치: `api.example.com` → 해당 이름만 매치
- 와일드카드를 정규식으로 변환하여 효율적으로 매칭

## 실행

```bash
go run main.go
```

## 시나리오

### 시나리오 1: 와일드카드
`*.google.com` 패턴 매칭

### 시나리오 2: K8s 서비스 패턴
`*.*.svc.cluster.local` 으로 클러스터 내 서비스 매칭

### 시나리오 3: 복합 패턴 (OR)
여러 패턴을 | 로 결합

### 시나리오 4: DNS Query 정규식
`--dns-query` 플래그의 정규식 필터

### 시나리오 5: 노드 이름 패턴
`cluster/node` 형식의 멀티클러스터 노드 매칭

## 핵심 학습 내용
- 와일드카드 → 정규식 변환 (안전한 문자만 허용)
- 앵커(`\A`, `\z`)로 전체 매치 강제
- `strings.Builder`로 효율적 정규식 문자열 구성
- 서브도메인 위장 공격 방지 (리터럴 점 이스케이프)
