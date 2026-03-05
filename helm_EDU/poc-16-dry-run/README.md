# PoC-16: Helm Dry Run

## 개요

Helm의 Dry Run은 실제 리소스를 생성하지 않고 설치/업그레이드 결과를 미리 확인하는 기능이다. 세 가지 전략(None, Client, Server)에 따라 서버 통신 여부와 검증 수준이 달라진다.

## 참조 소스코드

| 파일 | 설명 |
|------|------|
| `pkg/action/action.go` | DryRunStrategy (DryRunNone/Client/Server) |
| `pkg/action/install.go` | Install.RunWithContext — isDryRun, interactWithServer 분기 |

## 핵심 개념

### 1. DryRunStrategy
| 전략 | isDryRun | interactWithServer | 설명 |
|------|----------|-------------------|------|
| DryRunNone | false | true | 실제 설치 |
| DryRunClient | true | false | 클라이언트 측 (렌더링만) |
| DryRunServer | true | true | 서버 측 (API 검증) |

### 2. 주요 분기점
- `interactWithServer`: 클러스터 접근 확인, Capabilities 조회, 리소스 충돌 검사
- `isDryRun`: 실제 리소스 생성 여부, 릴리스 저장 여부
- DryRunClient: DefaultCapabilities, FakeKubeClient, MemoryStorage 사용
- DryRunServer: API 서버에 dry-run 파라미터 전달 (어드미션 컨트롤러 실행)

### 3. 클라이언트 vs 서버 dry-run
- Client: 클러스터 없이도 실행 가능 (helm template과 유사)
- Server: 어드미션 웹훅, 스키마 검증, 리소스 충돌 검사 수행

## 실행

```bash
go run main.go
```

## 시뮬레이션 내용

1. DryRunStrategy 3종 비교
2. DryRunClient 시나리오 (렌더링 결과 출력)
3. DryRunServer 시나리오 (API 서버 검증)
4. DryRunNone 시나리오 (실제 설치)
5. 클러스터 접근 불가 시나리오 (Client는 성공, Server는 실패)
6. 서버 측 검증 실패 시나리오
7. 전략별 동작 비교 표
8. 아키텍처 다이어그램
