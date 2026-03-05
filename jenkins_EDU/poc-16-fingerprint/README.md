# PoC-16: Jenkins Fingerprint (아티팩트 추적) 시스템

## 목적

Jenkins의 Fingerprint 시스템을 Go로 시뮬레이션한다.
MD5 해시 기반의 아티팩트 추적, RangeSet을 이용한 빌드 번호 범위 관리,
FingerprintMap을 통한 전역 저장소, isAlive/trim을 통한 생명주기 관리까지의 전체 흐름을 재현한다.

## 핵심 개념

### 1. Fingerprint란?

Jenkins에서 Fingerprint는 파일의 MD5 해시를 기반으로 아티팩트의 출처와 사용 이력을 추적하는 시스템이다.
"이 JAR 파일은 어떤 빌드에서 만들어졌고, 어떤 빌드들에서 사용되었는가?"에 대한 답을 제공한다.

### 2. 전체 추적 흐름

```
Job A (빌드 #5)                           FingerprintMap
  │                                       ┌──────────────┐
  │ artifact.jar 생성                     │ MD5 → FP 매핑 │
  │ → MD5 해시 계산: a1b2c3...            │              │
  │ → getOrCreate(A#5, jar, md5)    ────→ │ 새 FP 생성    │
  │                                       │ original=A#5 │
  │                                       │ usages:      │
  │                                       │  A: {5}      │
                                          └──────────────┘
Job B (빌드 #3)                                  │
  │                                              │
  │ artifact.jar 복사 (Copy Artifact)            │
  │ → MD5 해시 계산: a1b2c3... (동일!)           │
  │ → getOrCreate(null, jar, md5)    ──────────→ │
  │ → 기존 FP에 사용 기록 추가                    │
  │   fp.add("B", 3)                             │
  │                                       ┌──────────────┐
                                          │ usages:      │
Job C (빌드 #1)                           │  A: {5}      │
  │ artifact.jar 복사                     │  B: {3}      │
  │ → fp.add("C", 1)               ────→ │  C: {1}      │
                                          └──────────────┘
```

### 3. RangeSet: 빌드 번호의 효율적 표현

빌드 번호 1,2,3,5,7,8,9를 개별적으로 저장하면 7개의 정수가 필요하지만,
RangeSet은 이를 `[1,4),[5,6),[7,10)` (3개의 Range)으로 압축한다.

```
빌드 번호: 1, 2, 3, 5, 7, 8, 9

RangeSet 내부 표현:
  Range[0]: [1, 4)   → 1, 2, 3
  Range[1]: [5, 6)   → 5
  Range[2]: [7, 10)  → 7, 8, 9

직렬화 형식: "1-3,5,7-9"
```

#### RangeSet의 핵심 연산

| 연산 | 설명 | 예시 |
|------|------|------|
| `add(n)` | 빌드 번호 추가, 인접 범위 자동 병합 | add(4) → [1,4),[5,6) 이 [1,6)으로 병합 |
| `add(RangeSet)` | 두 RangeSet 합집합 | O(n+m) 투 포인터 알고리즘 |
| `includes(n)` | 빌드 번호 포함 여부 | includes(3) → true |
| `retainAll(RangeSet)` | 교집합으로 갱신 | trim()에서 kept 빌드 필터링에 사용 |
| `removeAll(RangeSet)` | 차집합으로 갱신 | trim()에서 오래된 빌드 제거에 사용 |
| `isSmallerThan(n)` | 모든 값이 n 미만인지 | isAlive()에서 dead 판정에 사용 |

### 4. BuildPtr: 빌드 참조

```
BuildPtr {
  name:   "my-team/build-job"   ← Job의 fullName
  number: 5                     ← 빌드 번호
}
```

- `original`: Fingerprint의 original 필드. 이 아티팩트를 처음 생성한 빌드.
- null이면 Jenkins 외부에서 생성된 파일.

### 5. FingerprintMap: 전역 캐시

```
FingerprintMap (extends KeyedDataStorage)
  │
  │ getOrCreate(build, fileName, md5hex)
  │   → MD5 길이 검증 (32자)
  │   → 소문자 정규화
  │   → 기존 FP 있으면 반환
  │   → 없으면 새 Fingerprint 생성
  │
  └─→ WeakReference 기반으로 미사용 FP는 GC 대상
```

### 6. 파일시스템 저장 경로 (FileFingerprintStorage)

```
JENKINS_HOME/fingerprints/{hash[0:2]}/{hash[2:4]}/{hash}.xml

예) MD5 = a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6
  → fingerprints/a1/b2/a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6.xml
```

### 7. isAlive와 Cleanup

```
isAlive() 판정 로직:
  1. original 빌드가 살아있으면 → true
  2. usages 순회:
     - Job이 존재하는지 확인
     - Job의 firstBuild 번호 확인
     - RangeSet.isSmallerThan(firstBuild)이면 dead
     - 하나라도 살아있으면 → true
  3. 모두 dead → false

FingerprintCleanupThread (매일 실행):
  - isAlive() == false → Fingerprint 삭제
  - isAlive() == true  → trim()으로 오래된 참조 정리
```

## 실제 Jenkins 소스 참조

| 파일 | 역할 |
|------|------|
| `core/src/main/java/hudson/model/Fingerprint.java` | Fingerprint 본체 (BuildPtr, Range, RangeSet 내부 클래스 포함) |
| `core/src/main/java/hudson/model/FingerprintMap.java` | 전역 캐시 (KeyedDataStorage, getOrCreate) |
| `core/src/main/java/jenkins/fingerprints/FingerprintStorage.java` | 플러거블 저장소 추상 클래스 (save, load, delete) |
| `core/src/main/java/jenkins/fingerprints/FileFingerprintStorage.java` | 파일시스템 기반 구현 (XML 직렬화) |
| `core/src/main/java/hudson/model/FingerprintCleanupThread.java` | 주기적 정리 스레드 (AsyncPeriodicWork, DAY 주기) |

### 주요 코드 위치

- **Fingerprint 필드**: `md5sum(byte[])`, `fileName(String)`, `timestamp(Date)`, `original(BuildPtr)`, `usages(Hashtable<String, RangeSet>)` (Fingerprint.java:839-854)
- **Range**: `[start, end)` 불변 범위, `includes()`, `combine()`, `intersect()` (Fingerprint.java:224-335)
- **RangeSet.add(int)**: 삽입 + 인접 범위 병합 (checkCollapse) (Fingerprint.java:396-418)
- **RangeSet.add(RangeSet)**: O(n+m) 투 포인터 병합 (Fingerprint.java:446-479)
- **RangeSet.retainAll**: O(n+m) 교집합 (Fingerprint.java:486-524)
- **RangeSet.removeAll**: O(n+m) 차집합 (Fingerprint.java:531-594)
- **RangeSet.ConverterImpl.serialize**: "1-3,5,7-9" 형식 직렬화 (Fingerprint.java:776-786)
- **isAlive()**: original + usages 순회 (Fingerprint.java:1046-1064)
- **trim()**: 오래된 빌드 참조 정리 (Fingerprint.java:1074-1128)
- **FingerprintMap.getOrCreate**: MD5 길이 검증 + 소문자 정규화 (FingerprintMap.java:63-83)

## 실행 방법

```bash
cd jenkins_EDU/poc-16-fingerprint
go run main.go
```

## 예상 출력

1. RangeSet 기본 동작: 빌드 번호 추가 시 인접 범위 자동 병합 관찰
2. RangeSet 집합 연산: AddRangeSet(합집합), RetainAll(교집합), RemoveAll(차집합)
3. Fingerprint 추적 흐름: 아티팩트 생성 → MD5 계산 → Fingerprint 생성 → 다른 빌드에서 사용 기록
4. 여러 아티팩트의 FingerprintMap 관리
5. isAlive 확인: Job 삭제 시 DEAD 판정, FingerprintCleanupThread로 정리
6. 파일시스템 저장 경로 체계 (hash[0:2]/hash[2:4]/hash.xml)
7. 아티팩트 사용 이력 조회 테이블
8. RangeSet XML 직렬화 형식 (구 형식/신 형식)
9. 전체 추적 흐름 ASCII 다이어그램
