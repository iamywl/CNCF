# PoC-15: BulkChange 저장 트랜잭션 패턴 시뮬레이션

## 목적

Jenkins에서 Saveable 객체의 설정을 여러 번 변경하면, 매 변경마다 `save()`가 호출되어
디스크 I/O가 반복된다. **BulkChange**는 이 문제를 해결하는 트랜잭션 패턴으로,
여러 변경을 하나의 `save()` 호출로 묶어 I/O를 최적화한다.

이 PoC는 BulkChange의 핵심 알고리즘(ThreadLocal 스택, parent 체인, commit/abort,
BulkChange.ALL)을 Go 표준 라이브러리만으로 재현한다.

## 참조 소스 코드

| 클래스 | 경로 | 역할 |
|--------|------|------|
| `BulkChange` | `core/src/main/java/hudson/BulkChange.java` | ThreadLocal 기반 저장 트랜잭션 |
| `Saveable` | `core/src/main/java/hudson/model/Saveable.java` | XML 영속 대상 인터페이스 |
| `AtomicFileWriter` | `core/src/main/java/hudson/util/AtomicFileWriter.java` | 임시파일 → rename 원자적 쓰기 |
| `SaveableListener` | `core/src/main/java/hudson/model/listeners/SaveableListener.java` | 저장 이벤트 리스너 |
| `Jenkins.save()` | `core/src/main/java/jenkins/model/Jenkins.java` (3579행) | BulkChange 협력 패턴 구현 예시 |

## 핵심 개념

### BulkChange 동작 원리

BulkChange의 핵심은 **ThreadLocal 스택**과 **Saveable 협력 규약**이다.

1. `new BulkChange(saveable)` → ThreadLocal에 자신을 push (parent 보존)
2. `saveable.setX(...)` 내부에서 `save()` 호출
3. `save()` 내부에서 `BulkChange.contains(this)` 확인 → true이면 저장 억제
4. `bc.commit()` → 자신을 ThreadLocal에서 pop → `saveable.save()` 한 번 실행
5. `bc.close()` → commit 안 했으면 abort (저장 안 함)

### 시퀀스 다이어그램: BulkChange 저장 트랜잭션

```
호출자          BulkChange         ThreadLocal(INSCOPE)    Saveable           디스크
  │                │                      │                  │                 │
  │ new BulkChange │                      │                  │                 │
  │───────────────>│ parent=current()     │                  │                 │
  │                │ INSCOPE.set(this)    │                  │                 │
  │                │─────────────────────>│                  │                 │
  │                │                      │                  │                 │
  │ saveable.setX(...)                    │                  │                 │
  │──────────────────────────────────────────────────────── >│                 │
  │                │                      │   save()         │                 │
  │                │                      │<─────────────────│                 │
  │                │  contains(this)?     │                  │                 │
  │                │<─────────────────────│                  │                 │
  │                │  true → 저장 억제    │                  │                 │
  │                │─────────────────────>│  return          │                 │
  │                │                      │─────────────────>│                 │
  │                │                      │                  │                 │
  │ saveable.setY(...)  (동일하게 억제)   │                  │                 │
  │                │                      │                  │                 │
  │ bc.commit()    │                      │                  │                 │
  │───────────────>│ pop()               │                  │                 │
  │                │ INSCOPE.set(parent)  │                  │                 │
  │                │─────────────────────>│                  │                 │
  │                │ saveable.save()      │                  │                 │
  │                │─────────────────────────────────────── >│                 │
  │                │                      │  contains(this)? │                 │
  │                │                      │<─────────────────│                 │
  │                │                      │  false → 저장!   │                 │
  │                │                      │─────────────────>│ write(XML)      │
  │                │                      │                  │────────────────>│
  │                │                      │                  │ fireOnChange()  │
  │                │                      │                  │                 │
```

### 중첩 BulkChange (parent 체인)

```
BulkChange 스택 (ThreadLocal → 링크드 리스트):

  INSCOPE ──> innerBC(job)
                │
                └─ parent ──> outerBC(config)
                                │
                                └─ parent ──> null

contains(config) 검색 경로:
  innerBC.saveable == config? NO
  └─> outerBC.saveable == config? YES → return true (저장 억제)

contains(job) 검색 경로:
  innerBC.saveable == job? YES → return true (저장 억제)
```

### BulkChange.ALL

```java
// BulkChange.java, 163행
public static final Saveable ALL = () -> {};
```

`BulkChange(ALL)`로 생성하면 `contains()` 검사에서 어떤 Saveable이든 true를 반환한다.
전역 설정 변경 시 모든 Saveable의 저장을 일괄 억제할 때 사용한다.

### AtomicFileWriter

```
쓰기 시작     임시 파일 생성          commit                abort
    │              │                    │                    │
    │  createTemp  │                    │                    │
    │─────────────>│ dest-atomic*.tmp   │                    │
    │              │                    │                    │
    │  write(data) │                    │                    │
    │─────────────>│ [임시파일에 기록]   │                    │
    │              │                    │                    │
    │              │  성공 시:           │                    │
    │              │  close()           │                    │
    │              │  rename(tmp→dest)  │                    │
    │              │─────────────────── >│ 원자적 교체        │
    │              │                    │                    │
    │              │  실패 시:           │                    │
    │              │  close()           │                    │
    │              │  delete(tmp)       │                    │
    │              │───────────────────────────────────────>│
    │              │                    │  원본 보존          │
```

## 실행 방법

```bash
go run main.go
```

## 데모 구성

| 데모 | 내용 |
|------|------|
| 데모 1 | BulkChange 없이 저장 — 매 변경마다 디스크 I/O (5회 쓰기) |
| 데모 2 | BulkChange로 배치 저장 — commit 시 한 번만 I/O (1회 쓰기) |
| 데모 3 | AtomicFileWriter — 원자적 파일 쓰기 (commit/abort) |
| 데모 4 | 중첩 BulkChange — parent 체인으로 스택 관리 |
| 데모 5 | BulkChange.ALL — 모든 Saveable의 저장 일괄 억제 |
| 데모 6 | BulkChange abort — commit 없이 close하면 저장 안 됨 |
| 데모 7 | SaveableListener — 전체 저장 이벤트 요약 |

## 예상 출력

```
╔══════════════════════════════════════════════════════════════════╗
║  Jenkins BulkChange 저장 트랜잭션 패턴 시뮬레이션               ║
║  참조: jenkins/core/src/main/java/hudson/BulkChange.java        ║
╚══════════════════════════════════════════════════════════════════╝

==================================================================
데모 1: BulkChange 없이 저장 — 매 변경마다 디스크 I/O 발생
==================================================================

  [Save #1] 디스크에 저장 실행 → .../config.xml
  [SaveableListener] onChange: JenkinsConfig → .../config.xml
  [Save #2] 디스크에 저장 실행 → .../config.xml
  ...
  [Save #5] 디스크에 저장 실행 → .../config.xml

  결과: save() 5회 호출, 디스크 쓰기 5회

==================================================================
데모 2: BulkChange로 배치 저장 — 한 번만 디스크 I/O
==================================================================

  [Save #1] BulkChange 활성 → 저장 억제됨 (JenkinsConfig)
  [Save #2] BulkChange 활성 → 저장 억제됨 (JenkinsConfig)
  ...
  [Save #5] BulkChange 활성 → 저장 억제됨 (JenkinsConfig)

  → bc.commit() 호출:
  [Save #6] 디스크에 저장 실행 → .../config2.xml

  결과: save() 6회 호출, 디스크 쓰기 1회 (5회→1회로 감소)

==================================================================
데모 3: AtomicFileWriter — 원자적 파일 쓰기
==================================================================

  --- 성공 케이스: commit ---
  commit 전 원본 파일 내용: <original>data</original>
  commit 후 파일 내용: <updated>new data</updated>

  --- 실패 케이스: abort ---
  abort 후 원본 파일 내용 (보존됨): <updated>new data</updated>
  임시 파일이 정상적으로 삭제됨

==================================================================
데모 4: 중첩 BulkChange — parent 체인을 통한 스택 관리
==================================================================

  BulkChangeContains(config) = true (외부 BC에 의해 억제)
  BulkChangeContains(job)    = true (내부 BC에 의해 억제)
  ...
  config: save() 3회 호출, 디스크 쓰기 1회
  job:    save() 3회 호출, 디스크 쓰기 1회

==================================================================
데모 5: BulkChange.ALL — 모든 Saveable의 저장 억제
==================================================================

  BulkChangeContains(config) = true
  BulkChangeContains(job)    = true
  config: save() 1회, 디스크 쓰기 0회 (억제됨)
  job:    save() 1회, 디스크 쓰기 0회 (억제됨)

==================================================================
데모 6: BulkChange abort — commit 없이 close하면 저장 안 됨
==================================================================

  save() 호출 4회, 디스크 쓰기 1회 (모두 억제)
  bc.close() 호출 (commit 안 함) → abort 실행
  디스크 파일: 초기 상태 유지
  메모리 상태: 변경된 상태 (롤백되지 않음!)

==================================================================
데모 7: SaveableListener — 저장 이벤트 통합 확인
==================================================================

  총 9건의 저장 이벤트 발생 (BulkChange 사용으로 대폭 감소)
```

## Jenkins 설계 포인트

1. **협력 규약 기반 최적화** — BulkChange는 Saveable 구현체의 협력이 필수적이다. `save()` 내부에서 `BulkChange.contains(this)`를 확인하지 않으면 최적화가 작동하지 않는다.

2. **commit-then-save 순서** — `commit()`에서 자신을 스코프에서 먼저 제거(pop)한 후 `save()`를 호출한다. 이 순서가 바뀌면 `save()` 내부에서 `contains()`가 여전히 true를 반환하여 저장이 영원히 억제된다.

3. **메모리 롤백 없음** — BulkChange는 디스크 저장만 억제할 뿐, 메모리 상태를 롤백하지 않는다. Jenkins 주석: *"unlike a real transaction, this will not roll back the state of the object"* (BulkChange.java, 113행)

4. **try-with-resources 안전장치** — Java의 `try (BulkChange bc = ...)` 패턴으로 commit을 호출하지 않아도 `close()` → `abort()`가 자동 실행되어 스코프 누출을 방지한다.

5. **BulkChange.ALL** — 매직 Saveable 인스턴스로 모든 저장을 일괄 억제한다. Jenkins의 전역 설정 변경(doConfigSubmit)에서 여러 Saveable 객체의 저장을 한꺼번에 제어할 때 사용한다.
