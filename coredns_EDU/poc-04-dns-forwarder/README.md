# PoC-04: CoreDNS DNS 포워더

## 개요

CoreDNS forward 플러그인의 업스트림 포워딩 로직을 시뮬레이션한다.

## CoreDNS 소스코드 참조

| 파일 | 내용 |
|------|------|
| `plugin/forward/forward.go:36` | Forward 구조체 (proxies, policy, maxfails) |
| `plugin/forward/forward.go:102` | ServeDNS - 업스트림 순회 및 포워딩 |
| `plugin/forward/forward.go:127` | deadline + retry 루프 |
| `plugin/forward/forward.go:136` | proxy.Down(maxfails) 체크 |
| `plugin/forward/forward.go:150` | 모든 업스트림 다운 시 랜덤 선택 |

## 핵심 개념

### 업스트림 순회 루프
```
for time.Now().Before(deadline) {
    proxy := list[i]
    if proxy.Down(maxfails) → 건너뛰기
    ret, err := proxy.Connect(...)
    if err → 헬스체크 트리거, 재시도
    성공 → 반환
}
```

### 포워딩 정책
| 정책 | 설명 |
|------|------|
| random | 기본 정책. 업스트림 목록을 셔플 |
| round_robin | 순차적으로 업스트림 순회 |

### 장애 감지
- `maxfails`: 연속 실패 횟수 임계값 (기본 2)
- 임계값 초과 시 업스트림을 "다운"으로 표시
- 모든 업스트림 다운 시 랜덤 선택 (헬스체크가 깨진 것으로 간주)

### 동시 요청 제한
- `maxConcurrent`: 동시 처리 가능한 요청 수 (0 = 무제한)
- 초과 시 REFUSED 반환

## 시뮬레이션 시나리오

1. 라운드 로빈 분배
2. 랜덤 정책 분배
3. 업스트림 장애 및 복구
4. 동시 요청 제한
5. 존 매칭 (from/ignored)

## 실행

```bash
go run main.go
```
