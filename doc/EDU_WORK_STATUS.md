# EDU 프로젝트 작업 현황

> 최종 업데이트: 2026-03-07 (kafka 완료)

---

## 디렉토리 명명 규칙

`{프로젝트명}_EDU` (예: `argo-cd_EDU`, `helm_EDU`)

---

## 작업 순서 (16개)

| 순서 | 프로젝트 | EDU 디렉토리 | 소스 언어 | 상태 |
|------|----------|-------------|----------|------|
| 1 | cilium | cilium_EDU | Go/C | **완료** (기본7 + 심화12 + PoC18) |
| 2 | kubernetes | kubernetes_EDU | Go | **완료** (기본7 + 심화20 + PoC26) |
| 3 | argo-cd | argo-cd_EDU | Go | **완료** (기본7 + 심화12 + PoC16) |
| 4 | jenkins | jenkins_EDU | Java | **완료** (기본7 + 심화12 + PoC16) |
| 5 | containerd | containerd_EDU | Go | **완료** (기본7 + 심화14 + PoC16) |
| 6 | grpc-go | grpc-go_EDU | Go | **완료** (기본7 + 심화12 + PoC16) |
| 7 | tart | tart_EDU | Swift | **완료** (기본7 + 심화12 + PoC16) |
| 8 | helm | helm_EDU | Go | **완료** (기본7 + 심화12 + PoC16) |
| 9 | terraform | terraform_EDU | Go | **완료** (기본7 + 심화12 + PoC16) |
| 10 | alertmanager | alertmanager_EDU | Go | **완료** (기본7 + 심화12 + PoC16) |
| 11 | hubble | hubble_EDU | Go | **완료** (기본7 + 심화12 + PoC16) |
| 12 | kafka | kafka_EDU | Java/Scala | **완료** (기본7 + 심화12 + PoC16) |
| 13 | grafana | grafana_EDU | Go/TS | **완료** (기본7 + 심화12 + PoC18) |
| 14 | loki | loki_EDU | Go | **완료** (기본7 + 심화12 + PoC18) |
| 15 | jaeger | jaeger_EDU | Go | **완료** (기본7 + 심화12 + PoC16) |
| 16 | istio | istio_EDU | Go | **완료** (기본7 + 심화12 + PoC16) |

> 1~7: 사용자 지정 순서, 8~14: 자율 배정, 16: 긴급 우선

---

## 작업 방식

1. **한 세션에 하나의 프로젝트만** 진행
2. 소스코드를 충분히 읽은 뒤 문서 작성 (추측 금지)
3. PoC는 `go run main.go` 실행 검증 필수
4. 완료 후 이 파일 업데이트

---

## 품질 기준

| 항목 | 기준 |
|------|------|
| 기본문서 | 6~7개 (README + 01~06) |
| 심화문서 | 10~12개 (각 500줄 이상, 실제 소스코드 검증된 참조만) |
| PoC | 16~18개 (main.go + README.md, 표준 라이브러리만, 실행 확인) |
| 언어 | 전체 한국어 |
