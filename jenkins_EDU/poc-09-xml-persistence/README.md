# PoC-09: XML 영속성 시뮬레이션 (XStream/XmlFile)

## 개요

Jenkins는 모든 설정과 빌드 데이터를 **XML 파일**로 저장한다. 데이터베이스를 사용하지 않고
`JENKINS_HOME` 디렉토리 아래의 XML 파일이 곧 데이터 저장소이다. 이 PoC는 Jenkins의
XML 영속성 시스템 핵심 개념을 Go 표준 라이브러리만으로 재현한다.

## 참조 소스 코드

| 클래스 | 경로 | 역할 |
|--------|------|------|
| `XmlFile` | `core/src/main/java/hudson/XmlFile.java` | XML 데이터 파일 읽기/쓰기 관리 |
| `AtomicFileWriter` | `core/src/main/java/hudson/util/AtomicFileWriter.java` | 임시파일 → rename 원자적 쓰기 |
| `BulkChange` | `core/src/main/java/hudson/BulkChange.java` | 다중 변경 트랜잭션 (save() 지연) |
| `Saveable` | `core/src/main/java/hudson/model/Saveable.java` | XML 영속 대상 객체의 공통 인터페이스 |
| `SaveableListener` | `core/src/main/java/hudson/model/listeners/SaveableListener.java` | 저장 이벤트 리스너 |

## 핵심 개념

### 1. AtomicFileWriter — 원자적 파일 쓰기
- 임시 파일에 먼저 기록 → `commit()`에서 `os.Rename()`으로 원자적 교체
- 크래시 시에도 원본 파일이 깨지지 않음
- `abort()` 호출 시 임시 파일만 삭제되고 원본 유지

### 2. XmlFile — XML 데이터 파일 관리
- `read()`: XML → 객체 역직렬화 (`encoding/xml`)
- `write()`: 객체 → XML 직렬화, AtomicFileWriter를 통해 안전하게 저장
- XML 선언 `<?xml version='1.1' encoding='UTF-8'?>` 자동 추가

### 3. BulkChange — 트랜잭션 패턴
- ThreadLocal(Go: goroutine-local) 스택으로 BulkChange 스코프 관리
- 스코프 안에서 `save()` 호출 시 실제 저장을 지연
- `commit()` 호출 시 한 번만 저장 → I/O 최적화
- `abort()` 호출 시 저장하지 않고 스코프 해제

### 4. Schema Evolution — 데이터 형식 진화
- **필드 추가**: 구 XML에 없는 필드 → zero value 초기화
- **필드 제거**: 구 필드를 `transient`로 표시 → 직렬화에서 제외
- **필드 변경**: V1 → V2 마이그레이션 함수

### 5. JENKINS_HOME 구조
```
JENKINS_HOME/
├── config.xml                    ← JenkinsConfig (전역 설정)
├── jobs/
│   └── {job-name}/
│       ├── config.xml            ← JobConfig (잡 설정)
│       └── builds/
│           └── {build-number}/
│               └── build.xml     ← BuildRecord (빌드 레코드)
├── users/
└── plugins/
```

## 실행 방법

```bash
go run main.go
```

## 데모 구성

| 데모 | 내용 |
|------|------|
| 데모 1 | AtomicFileWriter — 원자적 파일 쓰기 (성공/실패/동시 접근) |
| 데모 2 | XmlFile — XML 데이터 파일 읽기/쓰기 |
| 데모 3 | BulkChange — 다중 변경 트랜잭션 패턴 |
| 데모 4 | Schema Evolution — 데이터 형식 진화 (V1→V2 마이그레이션) |
| 데모 5 | SaveableListener — 저장 이벤트 리스너 |
| 데모 6 | JENKINS_HOME 디렉토리 구조 시뮬레이션 |
| 데모 7 | 전체 워크플로우 통합 시연 |

## Jenkins 설계 포인트

1. **"파일 시스템이 곧 데이터베이스"** — 외부 DB 의존성 없이 XML 파일만으로 모든 상태 관리
2. **원자적 쓰기로 데이터 무결성 보장** — 전원 장애나 크래시에도 파일이 깨지지 않음
3. **BulkChange로 성능 최적화** — 웹 UI에서 여러 설정 변경 시 한 번만 저장
4. **Schema Evolution으로 하위 호환성** — 버전 업그레이드 시 기존 XML 데이터 자동 마이그레이션
