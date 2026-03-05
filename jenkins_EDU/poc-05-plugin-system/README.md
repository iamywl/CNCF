# PoC-05: Jenkins 플러그인 시스템 (ExtensionPoint/@Extension 디스커버리)

## 개요

Jenkins의 플러그인 시스템 핵심 메커니즘을 Go 표준 라이브러리만으로 시뮬레이션한다.
이 PoC는 Jenkins가 1,800개 이상의 플러그인을 지원하는 확장 아키텍처의 핵심 원리를 재현한다.

## 실행

```bash
go run main.go
```

## Jenkins 소스 참조

| 컴포넌트 | 실제 파일 | 설명 |
|----------|-----------|------|
| ExtensionPoint | `hudson/ExtensionPoint.java` | 마커 인터페이스 (빈 인터페이스) |
| @Extension | `hudson/Extension.java` | 자동 발견 어노테이션 (ordinal, dynamicLoadable) |
| ExtensionFinder | `hudson/ExtensionFinder.java` | 확장 발견 전략 (Sezpoz + Guice) |
| ExtensionList | `hudson/ExtensionList.java` | 확장 컬렉션 (CopyOnWrite, 지연 로딩) |
| PluginManager | `hudson/PluginManager.java` | 플러그인 라이프사이클 관리 |
| PluginWrapper | `hudson/PluginWrapper.java` | 플러그인 메타데이터 + 상태 |

## 시뮬레이션 시나리오

### 시나리오 1: 플러그인 라이프사이클
- Manifest에서 플러그인 메타데이터 읽기
- 의존성 해석 (순환 감지, 위상 정렬)
- ClassLoader 생성 → 확장 등록 → 플러그인 시작

### 시나리오 2: ExtensionList 지연 로딩
- 첫 접근 시 ExtensionFinder를 통해 확장 탐색
- ordinal 기준 내림차순 정렬
- 캐시된 이후 즉시 반환

### 시나리오 3: ClassLoader 격리
- PluginFirstClassLoader vs ParentFirstClassLoader 비교
- 플러그인별 독립적인 라이브러리 버전 지원
- UberClassLoader: 모든 플러그인 클래스 통합 검색

### 시나리오 4: 동적 플러그인 로딩
- 런타임에 플러그인 추가
- ExtensionList 캐시 무효화 → 재로드
- dynamicLoadable 플래그 확인

### 시나리오 5: 빌드 파이프라인
- SCM → Builder → Publisher 확장 활용

### 시나리오 6: 플러그인 상태 관리
- 플러그인 목록 조회, 중지

### 시나리오 7: ExtensionList 리스너
- 확장 변경 시 리스너 알림

### 시나리오 8: 동시성 안전
- CopyOnWrite 패턴으로 여러 고루틴에서 안전한 읽기

## 핵심 구조

```
ExtensionPoint (마커 인터페이스)
    |
    +-- Builder, Publisher, SCM, Trigger (구체 ExtensionPoint)
    |
@Extension (어노테이션)
    |
    +-- ordinal: 우선순위 (내림차순)
    +-- dynamicLoadable: YES/NO/MAYBE
    |
ExtensionFinder (발견 전략)
    |
    +-- SezpozFinder (어노테이션 인덱스 기반)
    |
ExtensionList (컬렉션)
    |
    +-- 지연 로딩 + CopyOnWrite + ordinal 정렬
    |
PluginManager
    |
    +-- discover → resolve → load → start → stop
    +-- UberClassLoader (통합 클래스 로딩)
    +-- PluginFirstClassLoader (격리)
```

## Jenkins 플러그인 로딩 순서 (InitMilestone)

```
PLUGINS_LISTED    → 디렉토리 스캔, Manifest 읽기
PLUGINS_PREPARED  → 의존성 해석, ClassLoader 생성
PLUGINS_STARTED   → Plugin.start() 호출
COMPLETED         → 초기화 완료
```
