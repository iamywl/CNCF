# PoC: Hubble 출력 포맷터 전략 패턴

## 관련 문서
- [05-API-REFERENCE.md](../05-API-REFERENCE.md) - CLI 출력 형식
- [07-CODE-GUIDE.md](../07-CODE-GUIDE.md) - Printer 패키지

## 개요

Hubble CLI는 `--output` 플래그로 4가지 출력 형식을 지원합니다:
- **compact**: 한 줄 요약 (기본값, 사람이 읽기 좋음)
- **json**: JSON (jq와 파이프라인 조합에 최적)
- **dict**: 키-값 사전 형식 (상세 분석용)
- **tab**: 탭 정렬 테이블 (정렬된 목록)

이는 GoF Strategy 디자인 패턴입니다.

## 실행

```bash
go run main.go
```

## 시나리오

같은 4개 Flow를 4가지 형식으로 출력합니다:
- Compact: `Jan 02 15:04:05.000: source -> dest FORWARDED TCP:8080`
- JSON: `{"timestamp":"...","source":"...","verdict":"FORWARDED"}`
- Dict: 키-값 쌍으로 상세 표시
- Tab: 열 정렬된 테이블

## 핵심 학습 내용
- Strategy 디자인 패턴으로 출력 형식 교체
- Functional Options 패턴 (`WithFormat()`, `WithColor()`)
- `json.Encoder`로 스트리밍 JSON 출력
- `text/tabwriter`로 탭 정렬 테이블 출력
- JSON 출력 + jq 파이프라인 조합 활용
