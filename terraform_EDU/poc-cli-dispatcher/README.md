# PoC 10: CLI 커맨드 디스패치 시뮬레이션

## 개요

Terraform CLI의 명령어 디스패치 시스템을 시뮬레이션합니다. 사용자가 `terraform plan`, `terraform state list` 등의 명령어를 입력하면 적절한 핸들러를 찾아 실행하는 패턴을 구현합니다.

## 학습 목표

1. **Command 인터페이스**: Run(), Help(), Synopsis() 메서드의 역할
2. **CommandFactory 패턴**: 지연 생성으로 메모리 효율 확보
3. **Meta 임베딩**: 모든 명령어가 공유하는 기반 기능 (플래그 파싱, UI)
4. **서브커맨드**: "state list", "state show" 같은 중첩 명령어 처리
5. **오타 제안**: Levenshtein 거리 기반 유사 명령어 추천

## Terraform 실제 코드 참조

| 개념 | 실제 파일 |
|------|----------|
| CLI 구성 | `main.go`, `commands.go` |
| Meta 구조체 | `internal/command/meta.go` |
| Command 인터페이스 | `github.com/mitchellh/cli` |
| Plan 명령어 | `internal/command/plan.go` |
| Apply 명령어 | `internal/command/apply.go` |
| State 서브커맨드 | `internal/command/state_*.go` |

## 구현 내용

### 명령어 목록

| 명령어 | 설명 |
|--------|------|
| init | 프로바이더 설치, 백엔드 구성 |
| plan | 변경 사항 미리보기 |
| apply | 변경 사항 적용 |
| destroy | 리소스 삭제 |
| fmt | 설정 파일 포맷 정리 |
| validate | 설정 유효성 검증 |
| output | 출력값 표시 |
| version | 버전 출력 |
| state list | 리소스 목록 |
| state show | 리소스 상세 정보 |
| state mv | 리소스 이동 |
| state rm | 리소스 제거 |

### 주요 패턴

```
Command 인터페이스
├── Run(args) → int (exit code)
├── Help() → string (상세 도움말)
└── Synopsis() → string (한 줄 요약)

Meta 구조체 (모든 Command에 임베딩)
├── Color (컬러 출력)
├── WorkingDir (작업 디렉토리)
├── StatePath (상태 파일 경로)
├── ParseArgs() → 공통 플래그 파싱
└── Ui() → SimpleUI (출력 도우미)
```

## 실행 방법

```bash
go run main.go
```

## 데모 시나리오

1. 전체 도움말 출력
2. 기본 명령어 실행 (init, plan, apply, destroy)
3. 서브커맨드 실행 (state list, state show, state mv, state rm)
4. 플래그 처리 (-target, -out, -auto-approve, -upgrade)
5. 명령어별 도움말 (-help)
6. 오타 입력 시 유사 명령어 제안 ("destory" -> "destroy")
