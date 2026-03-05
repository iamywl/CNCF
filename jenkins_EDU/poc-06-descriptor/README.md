# PoC-06: Jenkins Descriptor/Describable 패턴

## 개요

Jenkins의 Descriptor/Describable 패턴을 Go 표준 라이브러리만으로 시뮬레이션한다.
이 패턴은 Jenkins 확장성의 핵심 설계로, Java의 `Object`/`Class` 관계와 유사하게
**설정 가능한 객체(Describable)**와 **그 타입의 메타데이터(Descriptor 싱글턴)**를 분리한다.

Jenkins에서 빌드 스텝, SCM, 트리거, 보안 설정 등 "구성 가능한(configurable)" 모든 것이
이 패턴을 기반으로 동작한다.

## 핵심 개념: Describable-Descriptor 관계

```
Describable (인스턴스, N개)              Descriptor (싱글턴, 타입당 1개)
┌──────────────────────┐               ┌──────────────────────────────┐
│ GitSCM               │               │ GitSCM.DescriptorImpl        │
│   url = "..."        │─getDescriptor()─>│   getDisplayName() = "Git"   │
│   branch = "main"    │               │   getPropertyTypes()         │
│                      │               │   newInstance(req, json) → T │
│ (설정 데이터 보유)    │               │   configure(req, json)       │
│ (XML 직렬화 대상)    │               │   getId()                    │
└──────────────────────┘               └──────────────────────────────┘
                                          ↑
┌──────────────────────┐               │ (동일한 Descriptor 싱글턴)
│ GitSCM               │               │
│   url = "..."        │─getDescriptor()─┘
│   branch = "develop" │
└──────────────────────┘

Java Object/Class 관계와의 비교:
  Object  ←→  Class           : JVM이 자동 제공
  Describable ←→ Descriptor   : 개발자가 명시 구현, UI/검증/팩토리 기능 포함
```

## 전체 아키텍처

```
Jenkins (싱글턴)
├── ExtensionList<Descriptor>            ← 전체 Descriptor 저장소
│   ├── GitSCM.DescriptorImpl
│   ├── SubversionSCM.DescriptorImpl
│   ├── Shell.DescriptorImpl
│   ├── Maven.DescriptorImpl
│   └── Mailer.DescriptorImpl
│
└── Map<Class, DescriptorExtensionList>  ← 확장 포인트별 분류
    ├── SCM       → [GitSCM.Desc, SubversionSCM.Desc]
    ├── Builder   → [Shell.Desc, Maven.Desc]
    └── Publisher → [Mailer.Desc]
```

## 실행

```bash
go run main.go
```

## Jenkins 소스 참조

| 컴포넌트 | 실제 파일 | 핵심 내용 |
|----------|-----------|-----------|
| Describable | `core/src/main/java/hudson/model/Describable.java` (51줄) | `getDescriptor()` default 메서드, `T extends Describable<T>` CRTP |
| Descriptor | `core/src/main/java/hudson/model/Descriptor.java` (~1334줄) | `clazz`, `propertyTypes`, `getDisplayName()`, `getId()`, `newInstance()`, `configure()` |
| Descriptor.PropertyType | `Descriptor.java` 내부 클래스 (168-246줄) | `clazz`, `type`, `displayName`, `itemType`, `getApplicableDescriptors()` |
| DescriptorExtensionList | `core/src/main/java/hudson/DescriptorExtensionList.java` (251줄) | `find(Class)`, `findByName(String)`, `newInstanceFromRadioList()` |
| Jenkins.getDescriptor() | `core/src/main/java/jenkins/model/Jenkins.java` (1542-1547줄) | `for (d : ExtensionList) if (d.clazz == type) return d` |
| Jenkins.getDescriptorOrDie() | `Jenkins.java` (1557-1562줄) | `getDescriptor()` + `AssertionError` |
| Jenkins.getDescriptorList() | `Jenkins.java` (2845-2847줄) | `descriptorLists.computeIfAbsent(type, ...)` |

## 시뮬레이션 시나리오

### 시나리오 1: Object/Class vs Describable/Descriptor 유사성
- Java Object/Class 관계와 Describable/Descriptor 관계의 구조적 유사성 시각화
- 핵심 차이점: Descriptor는 UI 통합, 팩토리, 전역 설정 기능 포함

### 시나리오 2: DescriptorRegistry 전역 조회
- `Jenkins.getDescriptor(Class)`: 타입 ID로 단건 조회
- `Jenkins.getDescriptorOrDie(Class)`: 없으면 패닉 (AssertionError)

### 시나리오 3: DescriptorExtensionList 확장 포인트별 필터링
- `Jenkins.getDescriptorList(Class)`: 확장 포인트별 Descriptor 목록
- UI 드롭다운/라디오 버튼 선택지가 생성되는 원리

### 시나리오 4: PropertyType 필드 타입 정보
- 각 Describable의 설정 필드 타입 정보
- Jelly/Groovy 폼 렌더링에서 위젯 선택 기준

### 시나리오 5: newInstance(req, JSONObject) 폼 바인딩
- Stapler가 HTTP 폼 → JSON 변환 → Descriptor.newInstance() 호출
- 여러 타입(GitSCM, Shell, Maven, Mailer)의 인스턴스 생성
- 필수 필드 검증

### 시나리오 6: configure(req, JSONObject) 전역 설정 관리
- Descriptor의 전역 설정 (Git 전역 설정, Maven HOME, SMTP 서버 등)
- configure()로 설정 업데이트

### 시나리오 7: 완전한 파이프라인 설정
- SCM 선택 → Builder 추가 → Publisher 추가
- Jenkins Job 설정 전체 흐름 시뮬레이션

### 시나리오 8: findByName() ID 기반 조회
- XML 직렬화/역직렬화 시 Descriptor ID 활용

### 시나리오 9: Describable.getDescriptor() 싱글턴 보장
- 서로 다른 인스턴스가 동일한 Descriptor 싱글턴을 공유하는지 검증

## 예상 출력

```
╔══════════════════════════════════════════════════════════════════════════╗
║  Jenkins Descriptor/Describable 패턴 시뮬레이션                            ║
║  Object/Class 관계의 확장: 인스턴스(Describable) ↔ 메타데이터(Descriptor)    ║
╚══════════════════════════════════════════════════════════════════════════╝

============================================================================
  시나리오 1: Object/Class vs Describable/Descriptor 유사성
============================================================================
  (Object/Class 관계와 Describable/Descriptor 관계 비교 다이어그램)

============================================================================
  시나리오 2: DescriptorRegistry — 전역 조회
============================================================================
  [0] id=hudson.plugins.git.GitSCM           displayName="Git" extPoint=hudson.scm.SCM
  [1] id=hudson.scm.SubversionSCM            displayName="Subversion" extPoint=hudson.scm.SCM
  [2] id=hudson.tasks.Shell                  displayName="Execute shell" extPoint=hudson.tasks.Builder
  [3] id=hudson.tasks.Maven                  displayName="Invoke top-level Maven targets" extPoint=hudson.tasks.Builder
  [4] id=hudson.tasks.Mailer                 displayName="E-mail Notification" extPoint=hudson.tasks.Publisher
  ...

============================================================================
  시나리오 5: newInstance(req, JSONObject) — 폼 바인딩
============================================================================
  생성된 인스턴스: GitSCM{url=https://github.com/jenkinsci/jenkins.git, branch=master, ...}
  인스턴스.getDescriptor() == 레지스트리.getDescriptor() ? true (싱글턴 확인)
  ...

============================================================================
  시나리오 9: Describable.getDescriptor() 싱글턴 보장
============================================================================
  git1.getDescriptor() == git2.getDescriptor() ? true (동일한 Descriptor 싱글턴)
```

## 왜 Descriptor/Describable 패턴인가?

| 설계 목표 | Descriptor 패턴의 해법 |
|-----------|----------------------|
| 관심사 분리 | 인스턴스 데이터(Describable)와 타입 메타데이터(Descriptor)를 분리 |
| 플러그인 확장 | `@Extension` 하나로 새 타입 + UI + 검증 + 전역 설정 등록 |
| 직렬화 효율 | Describable만 XML 직렬화, Descriptor는 `transient` (메모리에만 존재) |
| 타입 안전성 | 제네릭으로 `Descriptor<Builder>`가 `Builder`만 생성하도록 보장 |
| UI 자동화 | PropertyType으로 폼 위젯 자동 선택, Jelly/Groovy 뷰 자동 탐색 |
