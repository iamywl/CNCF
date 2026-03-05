# PoC-13: Jenkins SCM 폴링 및 변경 감지

## 목적

Jenkins의 SCM(Source Code Management) 변경 감지 시스템을 Go로 시뮬레이션한다.
SCM 폴링, 리비전 비교, ChangeLogSet 수집, 빌드 트리거까지의 전체 흐름을 재현한다.

## 핵심 개념

### 1. SCM 추상 클래스
소스 코드 관리 시스템의 공통 인터페이스:
- `checkout()`: 소스 체크아웃
- `calcRevisionsFromBuild()`: 빌드 기준 리비전 상태 계산
- `compareRemoteRevisionWith()`: 원격 변경 감지
- `createChangeLogParser()`: 변경 로그 파서

### 2. 폴링 흐름
```
SCMTrigger.run() (cron 스케줄)
  → SCM.poll(workspace, launcher, baseline)
    → calcRevisionsFromBuild() → baseline SCMRevisionState
    → compareRemoteRevisionWith(baseline) → PollingResult
      → NONE: 변경 없음
      → INSIGNIFICANT: 무시할 변경
      → SIGNIFICANT: 빌드 필요한 변경
      → INCOMPARABLE: 비교 불가
  → SIGNIFICANT이면 → Queue.schedule(job)
```

### 3. PollingResult.Change (4단계)
| 변경 수준 | 의미 | 빌드 트리거 |
|-----------|------|------------|
| NONE | 변경 없음 | X |
| INSIGNIFICANT | 무시할 변경 (문서 등) | X |
| SIGNIFICANT | 의미 있는 변경 (코드) | O |
| INCOMPARABLE | 비교 불가 (첫 빌드 등) | O |

### 4. ChangeLogSet
변경 이력 모델:
- `ChangeLogSet<T extends Entry>`: 변경 로그 컬렉션
- `Entry`: 개별 커밋/변경 (author, message, affectedFiles, timestamp)

### 5. NullSCM
SCM 미설정 시 기본값. 항상 `NO_CHANGES` 반환.

### 6. SCMTrigger
cron 표현식 기반 폴링 스케줄:
- `H/5 * * * *`: 5분마다 (H는 해시 기반 분산)
- 폴링 결과가 SIGNIFICANT면 빌드 스케줄링

## 실제 Jenkins 소스 참조

| 파일 | 역할 |
|------|------|
| `core/src/main/java/hudson/scm/SCM.java` | SCM 추상 클래스, poll/checkout |
| `core/src/main/java/hudson/scm/PollingResult.java` | 폴링 결과 (Change enum) |
| `core/src/main/java/hudson/scm/SCMRevisionState.java` | 리비전 상태 (baseline) |
| `core/src/main/java/hudson/scm/ChangeLogSet.java` | 변경 로그 모델 |
| `core/src/main/java/hudson/scm/NullSCM.java` | 기본 SCM (변경 없음) |
| `core/src/main/java/hudson/triggers/SCMTrigger.java` | cron 기반 폴링 트리거 |

## 실행 방법

```bash
cd jenkins_EDU/poc-13-scm-polling
go run main.go
```

## 예상 출력

1. SCM 구현체 등록 (GitSCM, SvnSCM, NullSCM)
2. 리포지토리 상태 시뮬레이션 (커밋 추가)
3. 폴링 사이클: baseline 계산 → 원격 비교 → PollingResult
4. 변경 감지 시 ChangeLogSet 수집
5. SIGNIFICANT 변경 시 빌드 트리거
6. 여러 폴링 사이클의 결과 요약
