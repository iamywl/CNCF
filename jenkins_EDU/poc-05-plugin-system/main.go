// poc-05-plugin-system: Jenkins ExtensionPoint/@Extension 디스커버리 시뮬레이션
//
// Jenkins의 플러그인 시스템 핵심 메커니즘을 Go 표준 라이브러리만으로 재현한다.
//
// 실제 Jenkins 소스 참조:
//   - hudson.ExtensionPoint (마커 인터페이스, ExtensionPoint.java)
//   - hudson.Extension (@Extension 어노테이션, Extension.java)
//     - ordinal: 확장 정렬 우선순위 (높을수록 먼저 선택)
//     - dynamicLoadable: 동적 로딩 지원 여부 (YES/NO/MAYBE)
//   - hudson.ExtensionFinder (확장 발견 전략, ExtensionFinder.java)
//     - Sezpoz 기반 어노테이션 인덱싱 → Guice 모듈로 변환
//   - hudson.ExtensionList (발견된 확장 컬렉션, ExtensionList.java)
//     - CopyOnWrite 패턴으로 동시성 안전
//     - 지연 로딩: 첫 접근 시 ExtensionFinder를 통해 확장 탐색
//   - hudson.PluginManager (PluginManager.java)
//     - plugins: 전체 플러그인 목록
//     - activePlugins: 활성 플러그인 (위상 정렬됨)
//     - uberClassLoader: 모든 플러그인 클래스를 로드할 수 있는 통합 ClassLoader
//     - initTasks(): 로딩 → 의존성 해석 → 시작 태스크 그래프 구성
//   - hudson.PluginWrapper (PluginWrapper.java)
//     - shortName, version, dependencies
//     - classLoader: 플러그인별 격리된 ClassLoader
//
// 실행: go run main.go

package main

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// 1. ExtensionPoint: 마커 인터페이스
// =============================================================================
// Jenkins의 hudson.ExtensionPoint는 빈 마커 인터페이스이다.
// 플러그인이 확장할 수 있는 컴포넌트임을 표시한다.
// 실제 코드: public interface ExtensionPoint {}

// ExtensionPoint 는 Jenkins의 확장 가능한 컴포넌트를 표시하는 마커 인터페이스이다.
// Jenkins에서 Builder, Publisher, SCM, Trigger 등 모든 확장 가능한 타입이
// 이 인터페이스를 구현한다.
type ExtensionPoint interface {
	// ExtensionPointMarker 는 마커 메서드이다.
	// 실제 Jenkins에서는 빈 인터페이스이지만, Go에서는 최소 하나의 메서드가 필요하다.
	ExtensionPointMarker()
}

// =============================================================================
// 2. @Extension 어노테이션 시뮬레이션
// =============================================================================
// Jenkins의 hudson.Extension은 자동 발견을 위한 어노테이션이다.
// - ordinal: double, 기본값 0, 높을수록 우선순위 높음 (내림차순 정렬)
// - optional: boolean, true이면 로딩 실패 시 로그 생략 (deprecated)
// - dynamicLoadable: YesNoMaybe (YES/NO/MAYBE), 동적 로딩 지원 여부

// DynamicLoadable 은 동적 로딩 지원 여부를 나타낸다.
// Jenkins의 jenkins.YesNoMaybe에 대응한다.
type DynamicLoadable int

const (
	// DynYes 는 동적 로딩을 명시적으로 지원함을 나타낸다.
	DynYes DynamicLoadable = iota
	// DynNo 는 동적 로딩을 지원하지 않음을 나타낸다. 재시작 필요.
	DynNo
	// DynMaybe 는 동적 로딩 지원 여부가 불확실함을 나타낸다 (기본값).
	DynMaybe
)

func (d DynamicLoadable) String() string {
	switch d {
	case DynYes:
		return "YES"
	case DynNo:
		return "NO"
	case DynMaybe:
		return "MAYBE"
	default:
		return "UNKNOWN"
	}
}

// ExtensionAnnotation 은 Jenkins의 @Extension 어노테이션을 시뮬레이션한다.
// Go에는 어노테이션이 없으므로 구조체로 메타데이터를 표현한다.
type ExtensionAnnotation struct {
	// Ordinal 은 확장의 우선순위이다. 높을수록 먼저 선택된다.
	// Jenkins 소스: double ordinal() default 0;
	Ordinal float64

	// Optional 은 true이면 로딩 실패 시 로그를 생략한다 (deprecated).
	// Jenkins 소스: boolean optional() default false;
	Optional bool

	// DynamicLoadable 은 동적 로딩 지원 여부이다.
	// Jenkins 소스: YesNoMaybe dynamicLoadable() default MAYBE;
	DynamicLoadable DynamicLoadable
}

// =============================================================================
// 3. ExtensionComponent: 확장 인스턴스 + 메타데이터
// =============================================================================
// Jenkins의 hudson.ExtensionComponent<T>는 발견된 확장 인스턴스와
// @Extension 메타데이터를 함께 보관한다.

// ExtensionComponent 는 확장 인스턴스와 메타데이터를 함께 보관한다.
type ExtensionComponent struct {
	// Instance 는 확장 구현체 인스턴스이다.
	Instance ExtensionPoint

	// Annotation 은 @Extension 어노테이션 정보이다.
	Annotation ExtensionAnnotation

	// TypeName 은 확장 타입의 전체 이름이다 (Java의 클래스명에 대응).
	TypeName string

	// PluginName 은 이 확장을 제공한 플러그인의 이름이다.
	PluginName string
}

// =============================================================================
// 4. ExtensionFinder: 확장 발견 전략
// =============================================================================
// Jenkins의 hudson.ExtensionFinder는 ExtensionPoint 자체도 ExtensionPoint이다.
// 기본 구현은 Sezpoz(어노테이션 인덱싱) → Guice(DI 컨테이너)를 사용한다.
//
// 실제 Jenkins의 발견 전략:
// 1. Sezpoz가 컴파일 타임에 @Extension이 붙은 클래스를 인덱싱
//    → META-INF/annotations/ 디렉토리에 인덱스 파일 생성
// 2. ExtensionFinder.Sezpoz가 인덱스를 읽어 Guice 모듈로 변환
// 3. Guice가 인스턴스를 생성하고 @Inject 의존성 주입

// ExtensionFinder 는 확장을 발견하는 전략 인터페이스이다.
type ExtensionFinder interface {
	ExtensionPoint

	// Find 는 주어진 타입의 확장을 찾아 반환한다.
	// extensionType: 찾으려는 ExtensionPoint 타입 이름
	Find(extensionType string) []ExtensionComponent

	// IsRefreshable 은 동적 새로고침을 지원하는지 여부를 반환한다.
	IsRefreshable() bool

	// Refresh 는 새로운 확장을 동적으로 발견한다.
	Refresh() []ExtensionComponent
}

// =============================================================================
// 5. 어노테이션 인덱스 기반 ExtensionFinder (Sezpoz 시뮬레이션)
// =============================================================================
// Jenkins의 ExtensionFinder.Sezpoz는 META-INF/annotations/ 디렉토리의
// 인덱스 파일을 읽어서 확장을 발견한다.

// AnnotationIndex 는 어노테이션 인덱스 항목이다.
// Sezpoz의 IndexItem<Extension,Object>에 대응한다.
type AnnotationIndex struct {
	// ClassName 은 @Extension이 붙은 클래스의 전체 이름이다.
	ClassName string
	// ExtensionType 은 이 클래스가 구현하는 ExtensionPoint 타입이다.
	ExtensionType string
	// Annotation 은 @Extension 어노테이션 정보이다.
	Annotation ExtensionAnnotation
	// Factory 는 인스턴스 생성 팩토리 함수이다.
	Factory func() ExtensionPoint
	// PluginName 은 이 확장을 제공한 플러그인 이름이다.
	PluginName string
}

// SezpozFinder 는 어노테이션 인덱스 기반 확장 발견기이다.
// Jenkins의 ExtensionFinder.Sezpoz에 대응한다.
type SezpozFinder struct {
	// indices 는 등록된 어노테이션 인덱스 목록이다.
	// 실제 Jenkins에서는 META-INF/annotations/ 파일에서 읽는다.
	indices []AnnotationIndex

	mu sync.RWMutex
}

func (s *SezpozFinder) ExtensionPointMarker() {}

// RegisterIndex 는 어노테이션 인덱스를 등록한다.
// 실제 Jenkins에서는 컴파일 타임에 자동 생성되지만, 여기서는 수동 등록한다.
func (s *SezpozFinder) RegisterIndex(idx AnnotationIndex) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.indices = append(s.indices, idx)
}

// Find 는 주어진 타입의 확장을 어노테이션 인덱스에서 찾아 인스턴스를 생성한다.
func (s *SezpozFinder) Find(extensionType string) []ExtensionComponent {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []ExtensionComponent
	for _, idx := range s.indices {
		if idx.ExtensionType == extensionType {
			instance := idx.Factory()
			if instance != nil {
				result = append(result, ExtensionComponent{
					Instance:   instance,
					Annotation: idx.Annotation,
					TypeName:   idx.ClassName,
					PluginName: idx.PluginName,
				})
			}
		}
	}
	return result
}

func (s *SezpozFinder) IsRefreshable() bool { return true }

func (s *SezpozFinder) Refresh() []ExtensionComponent {
	// 동적으로 새로 추가된 인덱스만 반환 (여기서는 전체 반환)
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []ExtensionComponent
	for _, idx := range s.indices {
		instance := idx.Factory()
		if instance != nil {
			result = append(result, ExtensionComponent{
				Instance:   instance,
				Annotation: idx.Annotation,
				TypeName:   idx.ClassName,
				PluginName: idx.PluginName,
			})
		}
	}
	return result
}

// =============================================================================
// 6. ExtensionList: 발견된 확장 컬렉션 (지연 로딩 + 캐싱)
// =============================================================================
// Jenkins의 hudson.ExtensionList<T>는 AbstractList<T>를 상속하며,
// CopyOnWrite 패턴으로 동시성 안전성을 보장한다.
//
// 핵심 특성:
// - 지연 로딩: extensions 필드가 nil이면 첫 접근 시 load()
// - CopyOnWrite: 수정 시 전체 리스트를 복제
// - 리스너: ExtensionListListener로 변경 알림

// ExtensionListListener 는 ExtensionList 변경 알림을 받는 인터페이스이다.
type ExtensionListListener interface {
	OnChange()
}

// ExtensionList 는 특정 타입의 모든 확장 인스턴스를 보관하는 컬렉션이다.
type ExtensionList struct {
	// extensionType 은 이 리스트가 관리하는 ExtensionPoint 타입 이름이다.
	extensionType string

	// extensions 는 발견된 확장 컴포넌트 목록이다 (CopyOnWrite).
	// nil이면 아직 로드되지 않음 (지연 로딩).
	extensions []ExtensionComponent

	// legacyInstances 는 수동으로 등록된 인스턴스이다.
	legacyInstances []ExtensionComponent

	// listeners 는 변경 알림을 받는 리스너 목록이다.
	listeners []ExtensionListListener

	// finders 는 확장을 발견하는 전략 목록이다.
	finders []ExtensionFinder

	// loaded 는 초기 로딩 완료 여부이다.
	loaded bool

	mu sync.RWMutex
}

// NewExtensionList 는 새로운 ExtensionList를 생성한다.
func NewExtensionList(extensionType string, finders []ExtensionFinder) *ExtensionList {
	return &ExtensionList{
		extensionType: extensionType,
		finders:       finders,
	}
}

// ensureLoaded 는 지연 로딩을 수행한다.
// Jenkins의 ExtensionList.ensureLoaded()에 대응한다.
func (el *ExtensionList) ensureLoaded() {
	if el.loaded {
		return
	}
	el.mu.Lock()
	defer el.mu.Unlock()
	if el.loaded {
		return // double-check locking
	}

	fmt.Printf("  [ExtensionList] '%s' 타입의 확장을 지연 로딩합니다...\n", el.extensionType)

	var components []ExtensionComponent
	for _, finder := range el.finders {
		found := finder.Find(el.extensionType)
		components = append(components, found...)
	}

	// legacyInstances 추가
	components = append(components, el.legacyInstances...)

	// ordinal 기준 내림차순 정렬 (높을수록 먼저)
	sort.Slice(components, func(i, j int) bool {
		return components[i].Annotation.Ordinal > components[j].Annotation.Ordinal
	})

	// CopyOnWrite: 새 슬라이스로 교체
	el.extensions = components
	el.loaded = true

	fmt.Printf("  [ExtensionList] '%s' 타입의 확장 %d개 로드 완료\n", el.extensionType, len(components))
}

// GetAll 은 모든 확장 인스턴스를 반환한다.
func (el *ExtensionList) GetAll() []ExtensionComponent {
	el.ensureLoaded()
	el.mu.RLock()
	defer el.mu.RUnlock()
	// CopyOnWrite: 읽기는 안전하게 복사본 반환
	result := make([]ExtensionComponent, len(el.extensions))
	copy(result, el.extensions)
	return result
}

// Get 은 주어진 타입 이름의 확장을 찾는다.
func (el *ExtensionList) Get(typeName string) *ExtensionComponent {
	for _, comp := range el.GetAll() {
		if comp.TypeName == typeName {
			return &comp
		}
	}
	return nil
}

// Size 는 확장 개수를 반환한다.
func (el *ExtensionList) Size() int {
	return len(el.GetAll())
}

// Add 는 수동으로 확장을 등록한다 (레거시 지원).
func (el *ExtensionList) Add(comp ExtensionComponent) {
	el.mu.Lock()
	defer el.mu.Unlock()
	el.legacyInstances = append(el.legacyInstances, comp)
	// 이미 로드됐으면 현재 리스트에도 추가
	if el.loaded {
		newExts := make([]ExtensionComponent, len(el.extensions)+1)
		copy(newExts, el.extensions)
		newExts[len(el.extensions)] = comp
		el.extensions = newExts
	}
	// 리스너 알림
	for _, l := range el.listeners {
		l.OnChange()
	}
}

// AddListener 는 변경 리스너를 등록한다.
func (el *ExtensionList) AddListener(l ExtensionListListener) {
	el.mu.Lock()
	defer el.mu.Unlock()
	el.listeners = append(el.listeners, l)
}

// =============================================================================
// 7. ClassLoader 격리 시뮬레이션
// =============================================================================
// Jenkins의 PluginFirstClassLoader는 일반적인 부모-우선(parent-first) 대신
// 플러그인-우선(plugin-first) 클래스 로딩을 수행한다.
// 이를 통해 플러그인이 자체 의존성 버전을 사용할 수 있다.

// ClassLoader 는 Java의 ClassLoader를 시뮬레이션한다.
type ClassLoader struct {
	name    string
	classes map[string]string // className -> 로드 위치
	parent  *ClassLoader
	// pluginFirst 는 true이면 자식 ClassLoader를 먼저 검색한다.
	pluginFirst bool
}

// NewClassLoader 는 새로운 ClassLoader를 생성한다.
func NewClassLoader(name string, parent *ClassLoader, pluginFirst bool) *ClassLoader {
	return &ClassLoader{
		name:        name,
		classes:     make(map[string]string),
		parent:      parent,
		pluginFirst: pluginFirst,
	}
}

// AddClass 는 클래스를 등록한다.
func (cl *ClassLoader) AddClass(className string) {
	cl.classes[className] = cl.name
}

// LoadClass 는 클래스를 로드한다.
// pluginFirst=true이면 자신을 먼저 검색한 뒤 부모로 위임한다.
// 이것이 Jenkins PluginFirstClassLoader의 핵심 동작이다.
func (cl *ClassLoader) LoadClass(className string) (string, bool) {
	if cl.pluginFirst {
		// 플러그인-우선: 자신을 먼저 검색
		if loc, ok := cl.classes[className]; ok {
			return loc, true
		}
		// 자신에게 없으면 부모에게 위임
		if cl.parent != nil {
			return cl.parent.LoadClass(className)
		}
		return "", false
	}

	// 부모-우선 (기본 Java ClassLoader 동작)
	if cl.parent != nil {
		if loc, ok := cl.parent.LoadClass(className); ok {
			return loc, true
		}
	}
	if loc, ok := cl.classes[className]; ok {
		return loc, true
	}
	return "", false
}

// =============================================================================
// 8. UberClassLoader 시뮬레이션
// =============================================================================
// Jenkins의 PluginManager.UberClassLoader는 모든 활성 플러그인의
// ClassLoader를 통합하여 하나의 ClassLoader로 제공한다.
// XStream 역직렬화 등에서 플러그인 클래스를 찾을 때 사용한다.

// UberClassLoader 는 모든 플러그인 ClassLoader를 통합한다.
type UberClassLoader struct {
	pluginLoaders []*ClassLoader
	cache         map[string]string // className -> 로드 위치 (캐시)
	mu            sync.RWMutex
}

// NewUberClassLoader 는 새로운 UberClassLoader를 생성한다.
func NewUberClassLoader() *UberClassLoader {
	return &UberClassLoader{
		cache: make(map[string]string),
	}
}

// AddPluginLoader 는 플러그인의 ClassLoader를 추가한다.
func (u *UberClassLoader) AddPluginLoader(cl *ClassLoader) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.pluginLoaders = append(u.pluginLoaders, cl)
}

// LoadClass 는 모든 플러그인에서 클래스를 찾는다.
// 실제 Jenkins: 캐시 → 각 플러그인 ClassLoader 순회
func (u *UberClassLoader) LoadClass(className string) (string, bool) {
	u.mu.RLock()
	if loc, ok := u.cache[className]; ok {
		u.mu.RUnlock()
		return loc, true
	}
	u.mu.RUnlock()

	// 각 플러그인 ClassLoader를 순회
	for _, loader := range u.pluginLoaders {
		if loc, ok := loader.LoadClass(className); ok {
			u.mu.Lock()
			u.cache[className] = loc
			u.mu.Unlock()
			return loc, true
		}
	}
	return "", false
}

// =============================================================================
// 9. Dependency: 플러그인 의존성
// =============================================================================
// Jenkins의 PluginWrapper.Dependency는 shortName + version + optional 정보를 가진다.

// Dependency 는 플러그인 의존성을 나타낸다.
type Dependency struct {
	ShortName string
	Version   string
	Optional  bool
}

func (d Dependency) String() string {
	opt := ""
	if d.Optional {
		opt = " (optional)"
	}
	return fmt.Sprintf("%s:%s%s", d.ShortName, d.Version, opt)
}

// =============================================================================
// 10. PluginWrapper: 플러그인 래퍼
// =============================================================================
// Jenkins의 hudson.PluginWrapper는 플러그인의 메타데이터와 상태를 관리한다.
// Manifest에서 읽은 정보 (shortName, version, dependencies)와
// 런타임 상태 (active, enabled, classLoader)를 보관한다.

// PluginState 는 플러그인의 라이프사이클 상태이다.
type PluginState int

const (
	PluginDiscovered PluginState = iota
	PluginLoaded
	PluginStarted
	PluginStopped
	PluginFailed
)

func (s PluginState) String() string {
	switch s {
	case PluginDiscovered:
		return "DISCOVERED"
	case PluginLoaded:
		return "LOADED"
	case PluginStarted:
		return "STARTED"
	case PluginStopped:
		return "STOPPED"
	case PluginFailed:
		return "FAILED"
	default:
		return "UNKNOWN"
	}
}

// PluginManifest 는 MANIFEST.MF 정보를 시뮬레이션한다.
// Jenkins 플러그인의 META-INF/MANIFEST.MF에 대응한다.
type PluginManifest struct {
	ShortName         string
	LongName          string
	Version           string
	JenkinsVersion    string // 최소 Jenkins 버전
	PluginDependencies string // "dep1:1.0,dep2:2.0;resolution:=optional" 형식
	DynamicLoad       string // "true", "false", "maybe"
}

// PluginWrapper 는 하나의 Jenkins 플러그인을 나타낸다.
type PluginWrapper struct {
	// 메타데이터 (Manifest에서 읽음)
	ShortName    string
	LongName     string
	Version      string
	Dependencies []Dependency

	// 런타임 상태
	State       PluginState
	Active      bool
	Enabled     bool
	IsBundled   bool
	ClassLoader *ClassLoader

	// 이 플러그인이 제공하는 확장 인덱스
	ExtensionIndices []AnnotationIndex

	// 동적 로딩 지원 여부
	DynamicLoadable DynamicLoadable

	// 에러 메시지 (실패 시)
	ErrorMessage string
}

// NewPluginWrapper 는 Manifest에서 PluginWrapper를 생성한다.
// Jenkins의 strategy.createPluginWrapper(arc)에 대응한다.
func NewPluginWrapper(manifest PluginManifest) *PluginWrapper {
	pw := &PluginWrapper{
		ShortName: manifest.ShortName,
		LongName:  manifest.LongName,
		Version:   manifest.Version,
		State:     PluginDiscovered,
		Active:    false,
		Enabled:   true,
	}

	// 의존성 파싱 (Jenkins의 "dep1:1.0,dep2:2.0;resolution:=optional" 형식)
	if manifest.PluginDependencies != "" {
		for _, dep := range strings.Split(manifest.PluginDependencies, ",") {
			dep = strings.TrimSpace(dep)
			optional := false
			if strings.Contains(dep, ";resolution:=optional") {
				optional = true
				dep = strings.Split(dep, ";")[0]
			}
			parts := strings.SplitN(dep, ":", 2)
			if len(parts) == 2 {
				pw.Dependencies = append(pw.Dependencies, Dependency{
					ShortName: parts[0],
					Version:   parts[1],
					Optional:  optional,
				})
			}
		}
	}

	// 동적 로딩 설정
	switch strings.ToLower(manifest.DynamicLoad) {
	case "true":
		pw.DynamicLoadable = DynYes
	case "false":
		pw.DynamicLoadable = DynNo
	default:
		pw.DynamicLoadable = DynMaybe
	}

	return pw
}

func (pw *PluginWrapper) String() string {
	return fmt.Sprintf("Plugin[%s v%s, state=%s, active=%v]",
		pw.ShortName, pw.Version, pw.State, pw.Active)
}

// =============================================================================
// 11. FailedPlugin: 로딩 실패 플러그인
// =============================================================================

// FailedPlugin 은 로딩에 실패한 플러그인 정보이다.
type FailedPlugin struct {
	Name    string
	Message string
}

// =============================================================================
// 12. PluginManager: 플러그인 관리자
// =============================================================================
// Jenkins의 hudson.PluginManager는 플러그인의 전체 라이프사이클을 관리한다.
// - 디렉토리 스캔 → Manifest 읽기 → 의존성 해석 → ClassLoader 생성 → 시작
//
// 실제 Jenkins의 초기화 순서 (InitMilestone):
// 1. PLUGINS_LISTED: 플러그인 아카이브 목록 작성
// 2. PLUGINS_PREPARED: ClassLoader 생성, 의존성 해석
// 3. PLUGINS_STARTED: 플러그인 시작 (Plugin.start() 호출)
// 4. COMPLETED: 초기화 완료

// PluginManager 는 Jenkins의 플러그인 관리자를 시뮬레이션한다.
type PluginManager struct {
	// plugins 는 발견된 모든 플러그인 목록이다.
	plugins []*PluginWrapper

	// activePlugins 는 활성 플러그인 (위상 정렬됨)이다.
	activePlugins []*PluginWrapper

	// failedPlugins 는 로딩 실패 플러그인 목록이다.
	failedPlugins []FailedPlugin

	// uberClassLoader 는 모든 플러그인 클래스를 로드할 수 있는 통합 ClassLoader이다.
	uberClassLoader *UberClassLoader

	// finder 는 확장 발견기이다.
	finder *SezpozFinder

	// extensionLists 는 타입별 ExtensionList 캐시이다.
	extensionLists map[string]*ExtensionList

	// coreClassLoader 는 Jenkins 코어의 ClassLoader이다.
	coreClassLoader *ClassLoader

	mu sync.RWMutex
}

// NewPluginManager 는 새로운 PluginManager를 생성한다.
func NewPluginManager() *PluginManager {
	coreCL := NewClassLoader("jenkins-core", nil, false)
	coreCL.AddClass("hudson.model.Job")
	coreCL.AddClass("hudson.model.Run")
	coreCL.AddClass("hudson.tasks.Builder")
	coreCL.AddClass("hudson.tasks.Publisher")
	coreCL.AddClass("hudson.scm.SCM")
	coreCL.AddClass("hudson.model.Descriptor")

	return &PluginManager{
		uberClassLoader: NewUberClassLoader(),
		finder:          &SezpozFinder{},
		extensionLists:  make(map[string]*ExtensionList),
		coreClassLoader: coreCL,
	}
}

// =============================================================================
// 13. 디렉토리 스캔 → Manifest 읽기 → PluginWrapper 생성
// =============================================================================
// Jenkins의 PluginManager.initTasks()의 첫 번째 단계이다.
// 실제로는 $JENKINS_HOME/plugins/ 디렉토리에서 .jpi/.hpi 파일을 찾는다.

// DiscoverPlugins 는 플러그인을 발견한다 (디렉토리 스캔 시뮬레이션).
func (pm *PluginManager) DiscoverPlugins(manifests []PluginManifest) {
	fmt.Println("\n[PluginManager] === 1단계: 플러그인 발견 (PLUGINS_LISTED) ===")

	inspectedNames := make(map[string]bool)

	for _, m := range manifests {
		pw := NewPluginWrapper(m)

		// 중복 검사 (Jenkins의 isDuplicate와 동일)
		if inspectedNames[pw.ShortName] {
			fmt.Printf("  [경고] '%s' 플러그인이 중복됩니다. 건너뜁니다.\n", pw.ShortName)
			continue
		}
		inspectedNames[pw.ShortName] = true

		pm.plugins = append(pm.plugins, pw)
		fmt.Printf("  [발견] %s v%s (의존성: %v)\n", pw.ShortName, pw.Version,
			formatDeps(pw.Dependencies))
	}

	fmt.Printf("  [결과] 총 %d개 플러그인 발견\n", len(pm.plugins))
}

func formatDeps(deps []Dependency) string {
	if len(deps) == 0 {
		return "없음"
	}
	names := make([]string, len(deps))
	for i, d := range deps {
		names[i] = d.String()
	}
	return strings.Join(names, ", ")
}

// =============================================================================
// 14. 의존성 해석 + 위상 정렬
// =============================================================================
// Jenkins의 PluginManager는 CyclicGraphDetector로 순환 의존성을 감지하고,
// 위상 정렬로 로딩 순서를 결정한다.

// ResolveDependencies 는 의존성을 검증하고 위상 정렬한다.
func (pm *PluginManager) ResolveDependencies() error {
	fmt.Println("\n[PluginManager] === 2단계: 의존성 해석 (PLUGINS_PREPARED) ===")

	// 이름 → 플러그인 맵 구축
	byName := make(map[string]*PluginWrapper)
	for _, p := range pm.plugins {
		byName[p.ShortName] = p
	}

	// 1. 순환 의존성 검사
	fmt.Println("  [검사] 순환 의존성 확인 중...")
	visited := make(map[string]int) // 0=미방문, 1=방문중, 2=완료
	var order []*PluginWrapper

	var visit func(name string) error
	visit = func(name string) error {
		switch visited[name] {
		case 1: // 순환 감지
			return fmt.Errorf("순환 의존성 감지: %s", name)
		case 2: // 이미 처리 완료
			return nil
		}

		visited[name] = 1 // 방문 중
		pw := byName[name]
		if pw == nil {
			return nil // 의존성이 없는 플러그인은 건너뜀
		}

		// 의존성 먼저 방문
		for _, dep := range pw.Dependencies {
			if dep.Optional {
				continue // optional 의존성은 건너뜀
			}
			if byName[dep.ShortName] == nil {
				// 필수 의존성이 없음
				pw.State = PluginFailed
				pw.ErrorMessage = fmt.Sprintf("필수 의존성 '%s' 없음", dep.ShortName)
				pm.failedPlugins = append(pm.failedPlugins, FailedPlugin{
					Name:    pw.ShortName,
					Message: pw.ErrorMessage,
				})
				fmt.Printf("  [실패] '%s' 플러그인: %s\n", pw.ShortName, pw.ErrorMessage)
				visited[name] = 2
				return nil
			}
			if err := visit(dep.ShortName); err != nil {
				return err
			}
		}

		visited[name] = 2 // 완료
		if pw.State != PluginFailed {
			order = append(order, pw)
		}
		return nil
	}

	for _, p := range pm.plugins {
		if err := visit(p.ShortName); err != nil {
			fmt.Printf("  [오류] %v\n", err)
			return err
		}
	}

	// 2. 활성 플러그인 목록 설정 (위상 정렬 순서)
	pm.activePlugins = order
	fmt.Printf("  [결과] 활성 플러그인 %d개 (위상 정렬 순서):\n", len(pm.activePlugins))
	for i, p := range pm.activePlugins {
		fmt.Printf("    %d. %s v%s\n", i+1, p.ShortName, p.Version)
	}

	return nil
}

// =============================================================================
// 15. ClassLoader 생성 + 플러그인 시작
// =============================================================================

// PrepareClassLoaders 는 각 플러그인의 ClassLoader를 생성한다.
func (pm *PluginManager) PrepareClassLoaders() {
	fmt.Println("\n[PluginManager] === 3단계: ClassLoader 생성 ===")

	for _, pw := range pm.activePlugins {
		// PluginFirstClassLoader 생성 (부모는 코어 ClassLoader)
		cl := NewClassLoader(
			fmt.Sprintf("PluginCL[%s]", pw.ShortName),
			pm.coreClassLoader,
			true, // plugin-first
		)

		// 플러그인 고유 클래스 등록 (시뮬레이션)
		for _, idx := range pw.ExtensionIndices {
			cl.AddClass(idx.ClassName)
		}

		pw.ClassLoader = cl
		pw.State = PluginLoaded
		pw.Active = true

		// UberClassLoader에 등록
		pm.uberClassLoader.AddPluginLoader(cl)

		fmt.Printf("  [ClassLoader] '%s' → PluginFirstClassLoader 생성\n", pw.ShortName)
	}
}

// RegisterExtensions 는 활성 플러그인의 확장을 ExtensionFinder에 등록한다.
func (pm *PluginManager) RegisterExtensions() {
	fmt.Println("\n[PluginManager] === 4단계: 확장 등록 ===")

	for _, pw := range pm.activePlugins {
		for _, idx := range pw.ExtensionIndices {
			pm.finder.RegisterIndex(idx)
			fmt.Printf("  [등록] %s → '%s' (ordinal=%.1f, dynamic=%s)\n",
				idx.ClassName, idx.ExtensionType,
				idx.Annotation.Ordinal, idx.Annotation.DynamicLoadable)
		}
	}
}

// StartPlugins 는 모든 활성 플러그인을 시작한다.
func (pm *PluginManager) StartPlugins() {
	fmt.Println("\n[PluginManager] === 5단계: 플러그인 시작 (PLUGINS_STARTED) ===")

	for _, pw := range pm.activePlugins {
		fmt.Printf("  [시작] %s v%s...\n", pw.ShortName, pw.Version)
		pw.State = PluginStarted
		// 실제 Jenkins에서는 Plugin.start() → Plugin.postInitialize() 호출
		time.Sleep(10 * time.Millisecond) // 시뮬레이션 지연
		fmt.Printf("  [완료] %s 시작됨\n", pw.ShortName)
	}
}

// StopPlugin 은 특정 플러그인을 중지한다.
func (pm *PluginManager) StopPlugin(shortName string) bool {
	for _, pw := range pm.activePlugins {
		if pw.ShortName == shortName {
			pw.State = PluginStopped
			pw.Active = false
			fmt.Printf("  [중지] %s 플러그인 중지됨\n", shortName)
			return true
		}
	}
	return false
}

// GetExtensionList 는 주어진 타입의 ExtensionList를 반환한다.
// Jenkins의 Jenkins.getExtensionList(Class<T>)에 대응한다.
func (pm *PluginManager) GetExtensionList(extensionType string) *ExtensionList {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if el, ok := pm.extensionLists[extensionType]; ok {
		return el
	}

	el := NewExtensionList(extensionType, []ExtensionFinder{pm.finder})
	pm.extensionLists[extensionType] = el
	return el
}

// =============================================================================
// 16. 구체적인 ExtensionPoint 구현 예시
// =============================================================================

// --- Builder ExtensionPoint ---
// Jenkins의 hudson.tasks.Builder에 대응

// Builder 는 빌드 단계를 수행하는 ExtensionPoint이다.
type Builder interface {
	ExtensionPoint
	Perform(workspace string) bool
	GetDisplayName() string
}

// ShellBuilder 는 셸 명령을 실행하는 Builder이다.
type ShellBuilder struct {
	Command string
}

func (b *ShellBuilder) ExtensionPointMarker() {}
func (b *ShellBuilder) GetDisplayName() string { return "셸 빌드 스텝" }
func (b *ShellBuilder) Perform(workspace string) bool {
	fmt.Printf("    [ShellBuilder] 작업공간 '%s'에서 실행: %s\n", workspace, b.Command)
	return true
}

// MavenBuilder 는 Maven 빌드를 수행하는 Builder이다.
type MavenBuilder struct {
	Goals string
}

func (b *MavenBuilder) ExtensionPointMarker() {}
func (b *MavenBuilder) GetDisplayName() string { return "Maven 빌드 스텝" }
func (b *MavenBuilder) Perform(workspace string) bool {
	fmt.Printf("    [MavenBuilder] 작업공간 '%s'에서 Maven 실행: %s\n", workspace, b.Goals)
	return true
}

// GradleBuilder 는 Gradle 빌드를 수행하는 Builder이다.
type GradleBuilder struct {
	Tasks string
}

func (b *GradleBuilder) ExtensionPointMarker() {}
func (b *GradleBuilder) GetDisplayName() string { return "Gradle 빌드 스텝" }
func (b *GradleBuilder) Perform(workspace string) bool {
	fmt.Printf("    [GradleBuilder] 작업공간 '%s'에서 Gradle 실행: %s\n", workspace, b.Tasks)
	return true
}

// --- Publisher ExtensionPoint ---
// Jenkins의 hudson.tasks.Publisher에 대응

// Publisher 는 빌드 후 작업을 수행하는 ExtensionPoint이다.
type Publisher interface {
	ExtensionPoint
	Perform(buildResult string) bool
	GetDisplayName() string
}

// JUnitPublisher 는 JUnit 테스트 결과를 수집하는 Publisher이다.
type JUnitPublisher struct {
	TestResultPattern string
}

func (p *JUnitPublisher) ExtensionPointMarker() {}
func (p *JUnitPublisher) GetDisplayName() string { return "JUnit 테스트 결과 수집" }
func (p *JUnitPublisher) Perform(buildResult string) bool {
	fmt.Printf("    [JUnitPublisher] 테스트 결과 수집: %s (빌드: %s)\n",
		p.TestResultPattern, buildResult)
	return true
}

// EmailPublisher 는 빌드 결과를 이메일로 알린다.
type EmailPublisher struct {
	Recipients string
}

func (p *EmailPublisher) ExtensionPointMarker() {}
func (p *EmailPublisher) GetDisplayName() string { return "이메일 알림" }
func (p *EmailPublisher) Perform(buildResult string) bool {
	fmt.Printf("    [EmailPublisher] '%s'에게 결과 발송: %s\n",
		p.Recipients, buildResult)
	return true
}

// --- SCM ExtensionPoint ---
// Jenkins의 hudson.scm.SCM에 대응

// SCM 은 소스코드 관리 확장점이다.
type SCM interface {
	ExtensionPoint
	Checkout(workspace string) bool
	GetDisplayName() string
}

// GitSCM 은 Git 소스코드 관리이다.
type GitSCM struct {
	URL    string
	Branch string
}

func (s *GitSCM) ExtensionPointMarker() {}
func (s *GitSCM) GetDisplayName() string { return "Git" }
func (s *GitSCM) Checkout(workspace string) bool {
	fmt.Printf("    [GitSCM] '%s' 브랜치 '%s'를 '%s'에 체크아웃\n",
		s.URL, s.Branch, workspace)
	return true
}

// SubversionSCM 은 Subversion 소스코드 관리이다.
type SubversionSCM struct {
	URL string
}

func (s *SubversionSCM) ExtensionPointMarker() {}
func (s *SubversionSCM) GetDisplayName() string { return "Subversion" }
func (s *SubversionSCM) Checkout(workspace string) bool {
	fmt.Printf("    [SubversionSCM] '%s'를 '%s'에 체크아웃\n", s.URL, workspace)
	return true
}

// --- Trigger ExtensionPoint ---

// Trigger 는 빌드 트리거 ExtensionPoint이다.
type Trigger interface {
	ExtensionPoint
	ShouldTrigger() bool
	GetDisplayName() string
}

// CronTrigger 는 Cron 기반 빌드 트리거이다.
type CronTrigger struct {
	CronExpression string
}

func (t *CronTrigger) ExtensionPointMarker()  {}
func (t *CronTrigger) GetDisplayName() string  { return "Cron 트리거" }
func (t *CronTrigger) ShouldTrigger() bool {
	// 시뮬레이션: 50% 확률로 트리거
	return rand.Intn(2) == 0
}

// WebhookTrigger 는 Webhook 기반 빌드 트리거이다.
type WebhookTrigger struct {
	HookURL string
}

func (t *WebhookTrigger) ExtensionPointMarker()  {}
func (t *WebhookTrigger) GetDisplayName() string  { return "Webhook 트리거" }
func (t *WebhookTrigger) ShouldTrigger() bool {
	return rand.Intn(3) == 0
}

// =============================================================================
// 17. 데모 데이터 생성
// =============================================================================

func createDemoManifests() []PluginManifest {
	return []PluginManifest{
		{
			ShortName:          "git",
			LongName:           "Git Plugin",
			Version:            "5.2.1",
			JenkinsVersion:     "2.426.3",
			PluginDependencies: "credentials:1311.vcf0a_900b_37c2,git-client:4.7.0",
			DynamicLoad:        "true",
		},
		{
			ShortName:          "credentials",
			LongName:           "Credentials Plugin",
			Version:            "1311.vcf0a_900b_37c2",
			JenkinsVersion:     "2.426.3",
			PluginDependencies: "",
			DynamicLoad:        "true",
		},
		{
			ShortName:          "git-client",
			LongName:           "Git Client Plugin",
			Version:            "4.7.0",
			JenkinsVersion:     "2.426.3",
			PluginDependencies: "credentials:1311.vcf0a_900b_37c2",
			DynamicLoad:        "true",
		},
		{
			ShortName:          "junit",
			LongName:           "JUnit Plugin",
			Version:            "1265.v65b_14fa_f12f0",
			JenkinsVersion:     "2.426.3",
			PluginDependencies: "",
			DynamicLoad:        "true",
		},
		{
			ShortName:          "email-ext",
			LongName:           "Email Extension Plugin",
			Version:            "2.103",
			JenkinsVersion:     "2.426.3",
			PluginDependencies: "credentials:1311.vcf0a_900b_37c2",
			DynamicLoad:        "maybe",
		},
		{
			ShortName:          "maven-plugin",
			LongName:           "Maven Integration Plugin",
			Version:            "3.23",
			JenkinsVersion:     "2.426.3",
			PluginDependencies: "junit:1265.v65b_14fa_f12f0",
			DynamicLoad:        "false",
		},
		{
			ShortName:          "gradle",
			LongName:           "Gradle Plugin",
			Version:            "2.11",
			JenkinsVersion:     "2.426.3",
			PluginDependencies: "",
			DynamicLoad:        "true",
		},
		{
			ShortName:          "subversion",
			LongName:           "Subversion Plugin",
			Version:            "2.17.4",
			JenkinsVersion:     "2.426.3",
			PluginDependencies: "credentials:1311.vcf0a_900b_37c2",
			DynamicLoad:        "true",
		},
		// 실패할 플러그인 (존재하지 않는 의존성)
		{
			ShortName:          "broken-plugin",
			LongName:           "Broken Plugin",
			Version:            "1.0",
			JenkinsVersion:     "2.426.3",
			PluginDependencies: "nonexistent-dep:1.0",
			DynamicLoad:        "false",
		},
		// 중복 플러그인 (무시될 것)
		{
			ShortName:          "git",
			LongName:           "Git Plugin (duplicate)",
			Version:            "4.0.0",
			JenkinsVersion:     "2.400",
			PluginDependencies: "",
			DynamicLoad:        "true",
		},
	}
}

// createExtensionIndices 는 각 플러그인의 확장 인덱스를 생성한다.
func createExtensionIndices(plugins []*PluginWrapper) {
	for _, pw := range plugins {
		switch pw.ShortName {
		case "git":
			pw.ExtensionIndices = []AnnotationIndex{
				{
					ClassName:     "hudson.plugins.git.GitSCM",
					ExtensionType: "SCM",
					Annotation:    ExtensionAnnotation{Ordinal: 10, DynamicLoadable: DynYes},
					Factory: func() ExtensionPoint {
						return &GitSCM{URL: "https://github.com/example/repo.git", Branch: "main"}
					},
					PluginName: "git",
				},
				{
					ClassName:     "hudson.plugins.git.GitTrigger",
					ExtensionType: "Trigger",
					Annotation:    ExtensionAnnotation{Ordinal: 5, DynamicLoadable: DynYes},
					Factory: func() ExtensionPoint {
						return &WebhookTrigger{HookURL: "/git/notifyCommit"}
					},
					PluginName: "git",
				},
			}
		case "subversion":
			pw.ExtensionIndices = []AnnotationIndex{
				{
					ClassName:     "hudson.scm.SubversionSCM",
					ExtensionType: "SCM",
					Annotation:    ExtensionAnnotation{Ordinal: 5, DynamicLoadable: DynYes},
					Factory: func() ExtensionPoint {
						return &SubversionSCM{URL: "svn://svn.example.com/trunk"}
					},
					PluginName: "subversion",
				},
			}
		case "junit":
			pw.ExtensionIndices = []AnnotationIndex{
				{
					ClassName:     "hudson.tasks.junit.JUnitResultArchiver",
					ExtensionType: "Publisher",
					Annotation:    ExtensionAnnotation{Ordinal: 100, DynamicLoadable: DynYes},
					Factory: func() ExtensionPoint {
						return &JUnitPublisher{TestResultPattern: "**/test-reports/*.xml"}
					},
					PluginName: "junit",
				},
			}
		case "email-ext":
			pw.ExtensionIndices = []AnnotationIndex{
				{
					ClassName:     "hudson.plugins.emailext.ExtendedEmailPublisher",
					ExtensionType: "Publisher",
					Annotation:    ExtensionAnnotation{Ordinal: 50, DynamicLoadable: DynMaybe},
					Factory: func() ExtensionPoint {
						return &EmailPublisher{Recipients: "dev-team@example.com"}
					},
					PluginName: "email-ext",
				},
			}
		case "maven-plugin":
			pw.ExtensionIndices = []AnnotationIndex{
				{
					ClassName:     "hudson.maven.MavenModuleSetBuild$Builder",
					ExtensionType: "Builder",
					Annotation:    ExtensionAnnotation{Ordinal: 20, DynamicLoadable: DynNo},
					Factory: func() ExtensionPoint {
						return &MavenBuilder{Goals: "clean install"}
					},
					PluginName: "maven-plugin",
				},
			}
		case "gradle":
			pw.ExtensionIndices = []AnnotationIndex{
				{
					ClassName:     "hudson.plugins.gradle.Gradle",
					ExtensionType: "Builder",
					Annotation:    ExtensionAnnotation{Ordinal: 15, DynamicLoadable: DynYes},
					Factory: func() ExtensionPoint {
						return &GradleBuilder{Tasks: "build test"}
					},
					PluginName: "gradle",
				},
			}
		}
	}
}

// =============================================================================
// 18. 동적 로딩 시뮬레이션
// =============================================================================

// DynamicPluginLoader 는 런타임에 플러그인을 동적으로 로드한다.
// Jenkins의 PluginManager.dynamicLoad()에 대응한다.
type DynamicPluginLoader struct {
	pm *PluginManager
}

// Load 는 플러그인을 동적으로 로드한다.
func (dpl *DynamicPluginLoader) Load(manifest PluginManifest, indices []AnnotationIndex) error {
	fmt.Printf("\n[DynamicLoader] 플러그인 '%s' 동적 로딩 시작...\n", manifest.ShortName)

	// 1. PluginWrapper 생성
	pw := NewPluginWrapper(manifest)
	pw.ExtensionIndices = indices

	// 2. 동적 로딩 가능 여부 확인
	canDynamic := true
	for _, idx := range indices {
		if idx.Annotation.DynamicLoadable == DynNo {
			canDynamic = false
			break
		}
	}

	if !canDynamic {
		fmt.Printf("  [경고] 일부 확장이 동적 로딩을 지원하지 않습니다. 재시작이 필요할 수 있습니다.\n")
	}

	// 3. ClassLoader 생성
	cl := NewClassLoader(
		fmt.Sprintf("PluginCL[%s]", pw.ShortName),
		dpl.pm.coreClassLoader,
		true,
	)
	pw.ClassLoader = cl
	pw.State = PluginLoaded
	pw.Active = true

	// 4. 플러그인 목록에 추가
	dpl.pm.plugins = append(dpl.pm.plugins, pw)
	dpl.pm.activePlugins = append(dpl.pm.activePlugins, pw)
	dpl.pm.uberClassLoader.AddPluginLoader(cl)

	// 5. 확장 인덱스 등록
	for _, idx := range indices {
		dpl.pm.finder.RegisterIndex(idx)
		fmt.Printf("  [등록] %s → '%s'\n", idx.ClassName, idx.ExtensionType)
	}

	// 6. 기존 ExtensionList 무효화 (캐시 리프레시)
	dpl.pm.mu.Lock()
	for typeName, el := range dpl.pm.extensionLists {
		el.mu.Lock()
		el.loaded = false // 다음 접근 시 다시 로드
		el.mu.Unlock()
		fmt.Printf("  [무효화] '%s' ExtensionList 캐시 클리어\n", typeName)
	}
	dpl.pm.mu.Unlock()

	// 7. 플러그인 시작
	pw.State = PluginStarted
	fmt.Printf("  [완료] '%s' v%s 동적 로딩 완료\n", pw.ShortName, pw.Version)

	return nil
}

// =============================================================================
// 19. 데모: 빌드 파이프라인 실행
// =============================================================================

func runBuildPipeline(pm *PluginManager, jobName string) {
	fmt.Printf("\n[빌드] === '%s' 빌드 파이프라인 실행 ===\n", jobName)
	workspace := fmt.Sprintf("/var/jenkins/workspace/%s", jobName)

	// 1. SCM 체크아웃
	fmt.Println("\n  [단계 1] SCM 체크아웃")
	scmList := pm.GetExtensionList("SCM")
	scms := scmList.GetAll()
	if len(scms) > 0 {
		scm := scms[0].Instance.(SCM)
		fmt.Printf("  SCM 선택: %s (ordinal=%.1f)\n",
			scms[0].TypeName, scms[0].Annotation.Ordinal)
		scm.Checkout(workspace)
	}

	// 2. 빌드 수행
	fmt.Println("\n  [단계 2] 빌드 수행")
	builderList := pm.GetExtensionList("Builder")
	builders := builderList.GetAll()
	for _, comp := range builders {
		builder := comp.Instance.(Builder)
		fmt.Printf("  Builder: %s (plugin=%s)\n", comp.TypeName, comp.PluginName)
		builder.Perform(workspace)
	}

	// 3. 빌드 후 작업
	fmt.Println("\n  [단계 3] 빌드 후 작업")
	pubList := pm.GetExtensionList("Publisher")
	pubs := pubList.GetAll()
	for _, comp := range pubs {
		pub := comp.Instance.(Publisher)
		fmt.Printf("  Publisher: %s (ordinal=%.1f, plugin=%s)\n",
			comp.TypeName, comp.Annotation.Ordinal, comp.PluginName)
		pub.Perform("SUCCESS")
	}
}

// =============================================================================
// 20. main: 전체 시나리오 실행
// =============================================================================

func main() {
	fmt.Println("=======================================================================")
	fmt.Println("Jenkins 플러그인 시스템 시뮬레이션")
	fmt.Println("=======================================================================")
	fmt.Println()
	fmt.Println("이 PoC는 Jenkins의 플러그인 시스템 핵심 메커니즘을 시뮬레이션합니다.")
	fmt.Println()
	fmt.Println("핵심 컴포넌트:")
	fmt.Println("  - ExtensionPoint: 확장 가능한 컴포넌트 (마커 인터페이스)")
	fmt.Println("  - @Extension: 자동 발견 어노테이션 (ordinal, dynamicLoadable)")
	fmt.Println("  - ExtensionFinder: 확장 발견 전략 (Sezpoz 인덱싱)")
	fmt.Println("  - ExtensionList: 확장 컬렉션 (지연 로딩, CopyOnWrite)")
	fmt.Println("  - PluginManager: 플러그인 라이프사이클 관리")
	fmt.Println("  - ClassLoader 격리: PluginFirstClassLoader")
	fmt.Println()

	// =================================================================
	// 시나리오 1: 플러그인 발견 → 의존성 해석 → 시작
	// =================================================================
	fmt.Println("=======================================================================")
	fmt.Println("[시나리오 1] 플러그인 라이프사이클: discover → load → start")
	fmt.Println("=======================================================================")

	pm := NewPluginManager()
	manifests := createDemoManifests()

	// 1단계: 플러그인 발견
	pm.DiscoverPlugins(manifests)

	// 확장 인덱스 생성 (실제로는 JPI 파일 내 META-INF/annotations/에서 읽음)
	createExtensionIndices(pm.plugins)

	// 2단계: 의존성 해석
	if err := pm.ResolveDependencies(); err != nil {
		fmt.Printf("[오류] 의존성 해석 실패: %v\n", err)
	}

	// 3단계: ClassLoader 생성
	pm.PrepareClassLoaders()

	// 4단계: 확장 등록
	pm.RegisterExtensions()

	// 5단계: 플러그인 시작
	pm.StartPlugins()

	// =================================================================
	// 시나리오 2: ExtensionList 지연 로딩 + ordinal 정렬
	// =================================================================
	fmt.Println()
	fmt.Println("=======================================================================")
	fmt.Println("[시나리오 2] ExtensionList 지연 로딩 & ordinal 정렬")
	fmt.Println("=======================================================================")

	fmt.Println("\n[테스트] Builder ExtensionList 접근 (첫 접근 → 지연 로딩 발생)")
	builderList := pm.GetExtensionList("Builder")
	builders := builderList.GetAll()
	fmt.Println("\n  Builder 확장 목록 (ordinal 내림차순):")
	for i, comp := range builders {
		fmt.Printf("    %d. %s (ordinal=%.1f, dynamic=%s, plugin=%s)\n",
			i+1, comp.TypeName, comp.Annotation.Ordinal,
			comp.Annotation.DynamicLoadable, comp.PluginName)
	}

	fmt.Println("\n[테스트] Publisher ExtensionList 접근")
	pubList := pm.GetExtensionList("Publisher")
	pubs := pubList.GetAll()
	fmt.Println("\n  Publisher 확장 목록 (ordinal 내림차순):")
	for i, comp := range pubs {
		fmt.Printf("    %d. %s (ordinal=%.1f, dynamic=%s, plugin=%s)\n",
			i+1, comp.TypeName, comp.Annotation.Ordinal,
			comp.Annotation.DynamicLoadable, comp.PluginName)
	}

	fmt.Println("\n[테스트] SCM ExtensionList 접근")
	scmList := pm.GetExtensionList("SCM")
	scms := scmList.GetAll()
	fmt.Println("\n  SCM 확장 목록 (ordinal 내림차순):")
	for i, comp := range scms {
		fmt.Printf("    %d. %s (ordinal=%.1f, plugin=%s)\n",
			i+1, comp.TypeName, comp.Annotation.Ordinal, comp.PluginName)
	}

	fmt.Println("\n[테스트] Builder ExtensionList 재접근 (캐시된 → 지연 로딩 없음)")
	builderList2 := pm.GetExtensionList("Builder")
	fmt.Printf("  Builder 확장 개수: %d (캐시에서 즉시 반환)\n", builderList2.Size())

	// =================================================================
	// 시나리오 3: ClassLoader 격리 시뮬레이션
	// =================================================================
	fmt.Println()
	fmt.Println("=======================================================================")
	fmt.Println("[시나리오 3] ClassLoader 격리 (PluginFirstClassLoader)")
	fmt.Println("=======================================================================")

	fmt.Println("\n--- PluginFirst vs ParentFirst 클래스 로딩 비교 ---")

	// 부모 ClassLoader (Jenkins 코어)
	parentCL := NewClassLoader("jenkins-core", nil, false)
	parentCL.AddClass("hudson.model.Job")
	parentCL.AddClass("com.google.common.collect.ImmutableList") // guava v30

	// 플러그인 ClassLoader (PluginFirst)
	pluginCL := NewClassLoader("PluginCL[my-plugin]", parentCL, true)
	pluginCL.AddClass("com.google.common.collect.ImmutableList") // guava v33 (다른 버전)
	pluginCL.AddClass("com.example.MyPluginClass")

	// 일반 ClassLoader (ParentFirst)
	normalCL := NewClassLoader("NormalCL[other]", parentCL, false)
	normalCL.AddClass("com.google.common.collect.ImmutableList") // guava v33

	testClasses := []string{
		"com.google.common.collect.ImmutableList",
		"hudson.model.Job",
		"com.example.MyPluginClass",
		"com.nonexistent.Class",
	}

	fmt.Println("\n  클래스 로딩 결과:")
	fmt.Printf("  %-48s %-25s %-25s\n", "클래스", "PluginFirst", "ParentFirst")
	fmt.Println("  " + strings.Repeat("-", 98))

	for _, cls := range testClasses {
		pfLoc, pfOk := pluginCL.LoadClass(cls)
		nLoc, nOk := normalCL.LoadClass(cls)

		pfResult := "로드 실패"
		if pfOk {
			pfResult = pfLoc
		}
		nResult := "로드 실패"
		if nOk {
			nResult = nLoc
		}

		fmt.Printf("  %-48s %-25s %-25s\n", cls, pfResult, nResult)
	}

	fmt.Println("\n  [핵심] PluginFirst에서 ImmutableList는 플러그인 자체에서 로드됨")
	fmt.Println("  → 플러그인이 코어와 다른 버전의 라이브러리를 사용할 수 있음")

	// UberClassLoader 테스트
	fmt.Println("\n--- UberClassLoader 테스트 ---")
	uber := NewUberClassLoader()
	uber.AddPluginLoader(pluginCL)

	// 두 번째 플러그인 ClassLoader
	plugin2CL := NewClassLoader("PluginCL[another-plugin]", parentCL, true)
	plugin2CL.AddClass("org.example.AnotherPluginClass")
	uber.AddPluginLoader(plugin2CL)

	uberTests := []string{
		"com.example.MyPluginClass",
		"org.example.AnotherPluginClass",
		"hudson.model.Job",
		"com.nonexistent.Class",
	}

	fmt.Println("  UberClassLoader 검색 결과:")
	for _, cls := range uberTests {
		loc, ok := uber.LoadClass(cls)
		if ok {
			fmt.Printf("    %-48s → %s\n", cls, loc)
		} else {
			fmt.Printf("    %-48s → 로드 실패\n", cls)
		}
	}

	// =================================================================
	// 시나리오 4: 동적 플러그인 로딩
	// =================================================================
	fmt.Println()
	fmt.Println("=======================================================================")
	fmt.Println("[시나리오 4] 동적 플러그인 로딩 (런타임 확장 추가)")
	fmt.Println("=======================================================================")

	dynLoader := &DynamicPluginLoader{pm: pm}

	// 새로운 Builder 플러그인 동적 로드
	newManifest := PluginManifest{
		ShortName:          "ant",
		LongName:           "Ant Plugin",
		Version:            "497.v94e7d9fffa_b_9",
		JenkinsVersion:     "2.426.3",
		PluginDependencies: "",
		DynamicLoad:        "true",
	}

	newIndices := []AnnotationIndex{
		{
			ClassName:     "hudson.tasks.Ant",
			ExtensionType: "Builder",
			Annotation:    ExtensionAnnotation{Ordinal: 25, DynamicLoadable: DynYes},
			Factory: func() ExtensionPoint {
				return &ShellBuilder{Command: "ant build"}
			},
			PluginName: "ant",
		},
	}

	_ = dynLoader.Load(newManifest, newIndices)

	// 리로드 후 Builder 목록 확인
	fmt.Println("\n[확인] 동적 로딩 후 Builder ExtensionList:")
	builderList3 := pm.GetExtensionList("Builder")
	builders3 := builderList3.GetAll()
	for i, comp := range builders3 {
		fmt.Printf("  %d. %s (ordinal=%.1f, plugin=%s)\n",
			i+1, comp.TypeName, comp.Annotation.Ordinal, comp.PluginName)
	}

	// Cron 트리거 플러그인도 동적 로드
	cronManifest := PluginManifest{
		ShortName:   "cron-trigger",
		LongName:    "Cron Trigger Plugin",
		Version:     "1.0",
		DynamicLoad: "true",
	}
	cronIndices := []AnnotationIndex{
		{
			ClassName:     "hudson.triggers.TimerTrigger",
			ExtensionType: "Trigger",
			Annotation:    ExtensionAnnotation{Ordinal: 10, DynamicLoadable: DynYes},
			Factory: func() ExtensionPoint {
				return &CronTrigger{CronExpression: "H/15 * * * *"}
			},
			PluginName: "cron-trigger",
		},
	}
	_ = dynLoader.Load(cronManifest, cronIndices)

	// =================================================================
	// 시나리오 5: 빌드 파이프라인 실행
	// =================================================================
	fmt.Println()
	fmt.Println("=======================================================================")
	fmt.Println("[시나리오 5] 빌드 파이프라인 실행 (확장 활용)")
	fmt.Println("=======================================================================")

	runBuildPipeline(pm, "my-java-project")

	// =================================================================
	// 시나리오 6: 플러그인 중지 & 상태 확인
	// =================================================================
	fmt.Println()
	fmt.Println("=======================================================================")
	fmt.Println("[시나리오 6] 플러그인 상태 확인 & 중지")
	fmt.Println("=======================================================================")

	fmt.Println("\n[상태] 전체 플러그인 목록:")
	fmt.Printf("  %-20s %-12s %-10s %-10s %-20s\n",
		"이름", "버전", "상태", "활성", "동적로딩")
	fmt.Println("  " + strings.Repeat("-", 72))
	for _, pw := range pm.plugins {
		fmt.Printf("  %-20s %-12s %-10s %-10v %-20s\n",
			pw.ShortName, pw.Version, pw.State, pw.Active, pw.DynamicLoadable)
	}

	if len(pm.failedPlugins) > 0 {
		fmt.Println("\n[실패] 로딩 실패 플러그인:")
		for _, fp := range pm.failedPlugins {
			fmt.Printf("  - %s: %s\n", fp.Name, fp.Message)
		}
	}

	// 플러그인 중지
	fmt.Println("\n[동작] 'email-ext' 플러그인 중지")
	pm.StopPlugin("email-ext")

	fmt.Println("\n[상태] 중지 후 플러그인 목록:")
	for _, pw := range pm.plugins {
		if pw.State == PluginStopped || pw.State == PluginFailed {
			fmt.Printf("  [%s] %s v%s\n", pw.State, pw.ShortName, pw.Version)
		}
	}

	// =================================================================
	// 시나리오 7: ExtensionList 리스너
	// =================================================================
	fmt.Println()
	fmt.Println("=======================================================================")
	fmt.Println("[시나리오 7] ExtensionList 리스너")
	fmt.Println("=======================================================================")

	// 리스너 등록
	triggerList := pm.GetExtensionList("Trigger")
	listenerCalled := false
	triggerList.AddListener(&SimpleListener{
		name: "TriggerListener",
		callback: func() {
			listenerCalled = true
			fmt.Println("  [리스너] Trigger ExtensionList가 변경되었습니다!")
		},
	})

	// 수동으로 확장 추가 (레거시 방식)
	fmt.Println("\n[동작] 레거시 방식으로 Trigger 확장 수동 추가")
	triggerList.Add(ExtensionComponent{
		Instance: &CronTrigger{CronExpression: "0 0 * * *"},
		Annotation: ExtensionAnnotation{
			Ordinal:         0,
			DynamicLoadable: DynYes,
		},
		TypeName:   "hudson.triggers.ManualCronTrigger",
		PluginName: "manual-registration",
	})

	if listenerCalled {
		fmt.Println("  [확인] 리스너가 호출되었습니다.")
	}

	fmt.Println("\n[확인] Trigger ExtensionList 최종 상태:")
	triggers := triggerList.GetAll()
	for i, comp := range triggers {
		fmt.Printf("  %d. %s (ordinal=%.1f, plugin=%s)\n",
			i+1, comp.TypeName, comp.Annotation.Ordinal, comp.PluginName)
	}

	// =================================================================
	// 시나리오 8: 동시성 안전 테스트
	// =================================================================
	fmt.Println()
	fmt.Println("=======================================================================")
	fmt.Println("[시나리오 8] 동시성 안전 테스트 (CopyOnWrite)")
	fmt.Println("=======================================================================")

	var wg sync.WaitGroup
	concurrentList := pm.GetExtensionList("Builder")
	errors := make(chan string, 100)

	// 여러 고루틴에서 동시에 읽기
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			all := concurrentList.GetAll()
			if len(all) == 0 {
				errors <- fmt.Sprintf("고루틴 %d: 빈 리스트 반환", id)
			}
		}(i)
	}

	wg.Wait()
	close(errors)

	hasErrors := false
	for err := range errors {
		fmt.Printf("  [오류] %s\n", err)
		hasErrors = true
	}
	if !hasErrors {
		fmt.Println("  [성공] 10개 고루틴에서 동시 읽기 성공 (CopyOnWrite 안전)")
	}

	// =================================================================
	// 요약
	// =================================================================
	fmt.Println()
	fmt.Println("=======================================================================")
	fmt.Println("요약: Jenkins 플러그인 시스템 핵심 메커니즘")
	fmt.Println("=======================================================================")
	fmt.Println()
	fmt.Println("1. ExtensionPoint (마커 인터페이스)")
	fmt.Println("   - Builder, Publisher, SCM, Trigger 등 확장 가능한 컴포넌트를 표시")
	fmt.Println("   - 실제: hudson.ExtensionPoint (빈 인터페이스)")
	fmt.Println()
	fmt.Println("2. @Extension (자동 발견 어노테이션)")
	fmt.Println("   - ordinal: 우선순위 (높을수록 먼저, 기본값 0)")
	fmt.Println("   - dynamicLoadable: YES/NO/MAYBE (동적 로딩 지원)")
	fmt.Println("   - Sezpoz가 컴파일 타임에 인덱싱 → 런타임에 Guice로 인스턴스 생성")
	fmt.Println()
	fmt.Println("3. ExtensionFinder → ExtensionList")
	fmt.Println("   - ExtensionFinder: 확장 발견 전략 (자체도 ExtensionPoint)")
	fmt.Println("   - ExtensionList: CopyOnWrite + 지연 로딩 + ordinal 정렬")
	fmt.Println("   - 첫 접근 시 ExtensionFinder를 통해 자동 로드")
	fmt.Println()
	fmt.Println("4. PluginManager 라이프사이클")
	fmt.Println("   - discover: 디렉토리 스캔 → Manifest 읽기")
	fmt.Println("   - resolve: 의존성 검증 + 순환 감지 + 위상 정렬")
	fmt.Println("   - load: ClassLoader 생성 (PluginFirstClassLoader)")
	fmt.Println("   - start: Plugin.start() 호출")
	fmt.Println("   - stop: Plugin.stop() 호출")
	fmt.Println()
	fmt.Println("5. ClassLoader 격리")
	fmt.Println("   - PluginFirstClassLoader: 플러그인-우선 클래스 로딩")
	fmt.Println("   - 각 플러그인이 독립적인 라이브러리 버전 사용 가능")
	fmt.Println("   - UberClassLoader: 모든 플러그인 클래스 통합 검색")
	fmt.Println()
	fmt.Println("6. 동적 로딩")
	fmt.Println("   - 런타임에 플러그인 추가/제거 가능")
	fmt.Println("   - ExtensionList 캐시 무효화 → 다음 접근 시 재로드")
	fmt.Println("   - dynamicLoadable=NO이면 재시작 필요")
}

// SimpleListener 는 ExtensionListListener의 간단한 구현이다.
type SimpleListener struct {
	name     string
	callback func()
}

func (l *SimpleListener) OnChange() {
	l.callback()
}
