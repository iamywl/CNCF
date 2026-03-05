# PoC-14: Jenkins View 시스템

## 목적

Jenkins의 View 시스템을 Go로 시뮬레이션한다.
View(작업 목록 UI 컴포넌트), AllView(전체 뷰), ListView(선택적 뷰),
ViewGroup(뷰 컨테이너), ViewJobFilter(필터 체인), HealthReport(건강 점수)의
동작 원리를 재현한다.

## 핵심 개념

### 1. View 추상 클래스

View는 Jenkins의 작업(Job) 목록을 보여주는 UI 컴포넌트이다.
확장 포인트(ExtensionPoint)로 설계되어 플러그인에서 커스텀 뷰를 만들 수 있다.

```
View (abstract)
├── name: 뷰 이름
├── description: 설명
├── owner: ViewGroup (컨테이너 참조)
├── filterExecutors: 관련 실행자만 표시 여부
├── filterQueue: 관련 큐 항목만 표시 여부
├── getItems(): 뷰에 포함된 작업 목록 (추상)
├── contains(item): 특정 작업 포함 여부 (추상)
├── getComputers(): 실행자 목록 (filterExecutors 적용)
└── getQueueItems(): 큐 항목 (filterQueue 적용)
```

### 2. AllView

모든 작업을 무조건 보여주는 기본 뷰이다.

- `getItems()` -> `owner.getItemGroup().getItems()` (전체 반환)
- `contains(item)` -> 항상 `true`
- `isEditable()` -> `false` (설정 변경 불가)
- `DEFAULT_VIEW_NAME = "all"`

### 3. ListView

명시적으로 선택한 작업만 보여주는 뷰이다. 3단계로 작업을 선별한다:

```
1단계: jobNames (TreeSet, 대소문자 무시)
   → 명시적으로 이름을 지정한 작업

2단계: includeRegex / includePattern
   → 정규식 패턴에 매칭되는 작업 추가 (1단계와 합집합)

3단계: jobFilters (ViewJobFilter 체인)
   → 필터를 순차적으로 적용하여 최종 목록 결정

4단계: 중복 제거 (LinkedHashSet)
```

### 4. ViewGroup

View의 컨테이너 인터페이스이다. Jenkins 인스턴스 자체가 ViewGroup을 구현한다.

- `getViews()`: 모든 뷰 목록
- `getView(name)`: 이름으로 뷰 조회
- `getPrimaryView()`: 기본 뷰 (대시보드에 표시)
- `canDelete(view)`: 삭제 가능 여부 (기본 뷰는 불가)
- `getItemGroup()`: 작업 컨테이너

### 5. ViewJobFilter 체인

```
ViewJobFilter (abstract)
└── filter(added, all, filteringView) → List<TopLevelItem>

체인 적용 순서:
  초기 목록 → [Filter 1] → [Filter 2] → ... → 최종 목록

내장 구현:
- StatusFilter: 활성/비활성 상태로 필터링
  - statusFilter=true: 활성 작업만
  - statusFilter=false: 비활성 작업만
```

### 6. HealthReport (건강 보고서)

최근 5개 빌드의 성공/실패 비율로 건강 점수를 계산한다.

```
계산식: score = 100 * (totalCount - failCount) / totalCount

점수 구간 및 아이콘:
  81~100  [SUNNY]  icon-health-80plus   해
  61~80   [CLOUDY] icon-health-60to79   구름+해
  41~60   [CLOUD]  icon-health-40to59   구름
  21~40   [RAIN]   icon-health-20to39   비
   0~20   [STORM]  icon-health-00to19   폭풍
```

### 7. filterExecutors / filterQueue

- `filterExecutors=true`: 뷰에 포함된 작업을 실행 중인 노드의 실행자만 표시
- `filterQueue=true`: 뷰에 포함된 작업의 큐 항목만 표시

## 실제 Jenkins 소스 참조

| 파일 | 역할 |
|------|------|
| `core/src/main/java/hudson/model/View.java` | View 추상 클래스, filterExecutors/filterQueue |
| `core/src/main/java/hudson/model/AllView.java` | 모든 작업 표시, DEFAULT_VIEW_NAME="all" |
| `core/src/main/java/hudson/model/ListView.java` | jobNames + includeRegex + jobFilters 3단계 선별 |
| `core/src/main/java/hudson/model/ViewGroup.java` | View 컨테이너 인터페이스, Jenkins가 구현 |
| `core/src/main/java/hudson/views/ViewJobFilter.java` | 뷰 작업 필터 추상 클래스, filter() 메서드 |
| `core/src/main/java/hudson/views/StatusFilter.java` | 활성/비활성 상태 필터, isDisabled() XOR statusFilter |
| `core/src/main/java/hudson/model/HealthReport.java` | 건강 점수(0~100), 5단계 아이콘, min()/max() |
| `core/src/main/java/hudson/model/Job.java` | getBuildStabilityHealthReport(), 최근 5개 빌드 기반 |

## 실행 방법

```bash
cd jenkins_EDU/poc-14-view-system
go run main.go
```

## 예상 출력

1. Jenkins 인스턴스에 8개 작업(Job) 생성 (다양한 상태: 활성/비활성, 성공/실패)
2. AllView: 모든 8개 작업 표시, contains()가 항상 true
3. ListView (jobNames): 명시적으로 추가한 빌드 작업 2개만 표시
4. ListView (includeRegex): `deploy-.*` 패턴으로 배포 작업 자동 매칭
5. ViewJobFilter 체인: StatusFilter(활성만/비활성만), 복합 필터(StatusFilter + HealthFilter)
6. HealthReport: 각 작업의 최근 5개 빌드 기반 건강 점수 및 날씨 아이콘 시각화
7. filterExecutors/filterQueue: 뷰 범위에 따른 실행자/큐 필터링 데모
8. ViewGroup: 뷰 목록 조회, 기본 뷰, 삭제 가능 여부, 뷰 삭제
9. View 시스템 전체 아키텍처 다이어그램
10. 핵심 원리 요약
