// poc-06-descriptor: Jenkins Descriptor/Describable 패턴 시뮬레이션
//
// Jenkins의 Descriptor/Describable 패턴을 Go 표준 라이브러리만으로 재현한다.
// 이 패턴은 Java의 Object/Class 관계와 유사하게, 설정 가능한 객체(Describable)와
// 그 타입의 메타데이터(Descriptor 싱글턴)를 분리하는 설계이다.
//
// 실제 Jenkins 소스 참조:
//   - hudson.model.Describable (Describable.java, 51줄)
//     - getDescriptor(): Jenkins.get().getDescriptorOrDie(getClass())
//     - T extends Describable<T> 자기참조 제네릭 (CRTP)
//   - hudson.model.Descriptor (Descriptor.java, ~1334줄)
//     - clazz: 설명하는 Describable 클래스
//     - propertyTypes: 필드별 타입 정보 (지연 초기화)
//     - getDisplayName(): 사람이 읽을 수 있는 이름 (기본값: clazz.getSimpleName())
//     - getId(): 고유 식별자 (기본값: clazz.getName())
//     - newInstance(StaplerRequest2, JSONObject): JSON → Describable 인스턴스 변환
//     - configure(StaplerRequest2, JSONObject): 전역 설정 업데이트
//     - PropertyType 내부 클래스: clazz, type, displayName, itemType
//   - hudson.DescriptorExtensionList (DescriptorExtensionList.java, 251줄)
//     - 특정 ExtensionPoint에 대한 Descriptor 컬렉션
//     - find(Class<? extends T>): d.clazz == type인 Descriptor 검색
//     - findByName(String id): Descriptor.getId()로 검색
//     - newInstanceFromRadioList(JSONObject): 라디오 선택 → 인스턴스 생성
//   - jenkins.model.Jenkins (Jenkins.java)
//     - getDescriptor(Class): ExtensionList<Descriptor> 순회, d.clazz == type 매칭
//     - getDescriptorOrDie(Class): getDescriptor() + AssertionError
//     - getDescriptorList(Class): DescriptorExtensionList 조회 (computeIfAbsent)
//
// 실행: go run main.go

package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// =============================================================================
// 1. PropertyType: 필드별 타입 정보
// =============================================================================
// Jenkins의 Descriptor.PropertyType 내부 클래스에 대응한다.
// 실제 코드 (Descriptor.java:168-246):
//   public static final class PropertyType {
//       public final Class clazz;
//       public final Type type;
//       private volatile Class itemType;
//       public final String displayName;
//   }
// PropertyType은 Describable의 각 설정 필드에 대한 리플렉션 정보를 제공한다.
// Jelly/Groovy 폼 렌더링 시 필드 타입에 따라 적절한 UI 위젯을 선택하는 데 사용된다.

// PropertyType 은 Describable 필드 하나의 타입 정보를 표현한다.
type PropertyType struct {
	Name        string // 필드 이름
	TypeName    string // 타입 이름 ("string", "int", "bool", "[]string" 등)
	DisplayName string // 사람이 읽을 수 있는 표시 이름
	Required    bool   // 필수 여부
	ItemType    string // 컬렉션인 경우 요소 타입 (예: []string → "string")
}

// String 은 PropertyType의 문자열 표현을 반환한다.
func (pt PropertyType) String() string {
	req := ""
	if pt.Required {
		req = " [필수]"
	}
	item := ""
	if pt.ItemType != "" {
		item = fmt.Sprintf(" (요소타입: %s)", pt.ItemType)
	}
	return fmt.Sprintf("%-15s %-10s %s%s%s", pt.Name, pt.TypeName, pt.DisplayName, req, item)
}

// =============================================================================
// 2. Describable: 설정 가능한 객체 인터페이스
// =============================================================================
// Jenkins의 hudson.model.Describable<T extends Describable<T>>에 대응한다.
// 실제 코드 (Describable.java:35-50):
//   public interface Describable<T extends Describable<T>> {
//       default Descriptor<T> getDescriptor() {
//           return Jenkins.get().getDescriptorOrDie(getClass());
//       }
//   }
//
// Java에서는 자기참조 제네릭(CRTP)을 통해 Describable 구현체가 자신의 타입을
// Descriptor에 전달한다. Go에서는 인터페이스로 이를 시뮬레이션한다.
//
// Jenkins의 모든 설정 가능한 객체(Builder, SCM, Publisher, Trigger 등)가
// 이 인터페이스를 구현한다. getDescriptor()는 default 메서드로 구현되어 있어
// 대부분의 구현체는 이 메서드를 직접 구현하지 않아도 된다.

// Describable 은 Jenkins의 Describable 인터페이스에 대응한다.
// 설정 가능한 객체가 자신의 메타데이터 제공자(Descriptor)를 반환한다.
type Describable interface {
	// GetDescriptor 는 이 인스턴스의 Descriptor 싱글턴을 반환한다.
	// Jenkins에서는 Jenkins.get().getDescriptorOrDie(getClass())를 호출한다.
	GetDescriptor() *Descriptor

	// TypeID 는 이 Describable의 타입 식별자를 반환한다.
	// Java의 getClass().getName()에 대응한다.
	TypeID() string

	// ExtensionPointType 은 이 Describable이 속한 확장 포인트 타입을 반환한다.
	// Java의 Descriptor.getT()에 대응 — Descriptor가 어느 DescriptorExtensionList에
	// 속하는지를 결정한다.
	ExtensionPointType() string
}

// =============================================================================
// 3. Descriptor: 타입별 메타데이터 싱글턴
// =============================================================================
// Jenkins의 hudson.model.Descriptor<T extends Describable<T>>에 대응한다.
// 실제 코드 (Descriptor.java:152-156):
//   public abstract class Descriptor<T extends Describable<T>>
//       implements Loadable, Saveable, OnMaster {
//       public final transient Class<? extends T> clazz;
//       private transient volatile Map<String, PropertyType> propertyTypes;
//   }
//
// Descriptor는 Describable 타입에 대한 메타데이터를 보유하는 싱글턴이다.
// Java의 Class 객체가 인스턴스의 메타데이터를 제공하듯, Descriptor는
// Describable 인스턴스의 메타데이터를 제공한다.
//
// 핵심 역할:
//   1. 팩토리: newInstance()로 JSON 데이터 → Describable 인스턴스 생성
//   2. 메타데이터: displayName, propertyTypes, id 제공
//   3. 전역 설정: configure()로 시스템 레벨 설정 관리
//   4. 폼 렌더링: Jelly/Groovy 뷰와 연동하여 설정 UI 생성
//   5. 폼 검증: doCheckXxx() 메서드로 실시간 필드 검증

// Descriptor 는 하나의 Describable 타입에 대한 메타데이터 싱글턴이다.
type Descriptor struct {
	// Clazz 는 이 Descriptor가 설명하는 Describable의 타입 ID이다.
	// Java의 public final transient Class<? extends T> clazz에 대응한다.
	Clazz string

	// ExtPoint 는 이 Descriptor가 속하는 확장 포인트 타입이다.
	// Java의 getT() 반환값에 대응 — DescriptorExtensionList 분류 기준이다.
	ExtPoint string

	// displayName 은 사람이 읽을 수 있는 이름이다.
	// Java에서 기본값은 clazz.getSimpleName()이다.
	displayName string

	// propertyTypes 는 Describable의 설정 필드별 타입 정보이다.
	// Java에서 지연 초기화(volatile)되며, 리플렉션으로 자동 수집한다.
	propertyTypes []PropertyType

	// globalConfig 는 전역 설정 데이터이다.
	// Java에서 Descriptor 필드에 직접 저장하고, save()/load()로 XML 영속화한다.
	globalConfig map[string]interface{}

	// newInstanceFn 은 JSON → Describable 인스턴스 팩토리 함수이다.
	// Java의 newInstance(StaplerRequest2, JSONObject)에 대응한다.
	// 실제 코드 (Descriptor.java:590-596):
	//   public T newInstance(StaplerRequest2 req, JSONObject formData) throws FormException {
	//       return newInstanceImpl(req, formData);
	//   }
	newInstanceFn func(jsonData map[string]interface{}) (Describable, error)

	// configureFn 은 전역 설정 업데이트 함수이다.
	// Java의 configure(StaplerRequest2, JSONObject)에 대응한다.
	// 실제 코드 (Descriptor.java:869-878):
	//   public boolean configure(StaplerRequest2 req, JSONObject json) throws FormException {
	//       return true;  // 기본 구현은 항상 true
	//   }
	configureFn func(d *Descriptor, jsonData map[string]interface{}) bool
}

// GetDisplayName 은 사람이 읽을 수 있는 이름을 반환한다.
// Java의 getDisplayName() 메서드에 대응한다.
// 실제 코드 (Descriptor.java:328-330):
//
//	public String getDisplayName() { return clazz.getSimpleName(); }
func (d *Descriptor) GetDisplayName() string {
	if d.displayName != "" {
		return d.displayName
	}
	// 기본값: Java의 clazz.getSimpleName()처럼 마지막 '.' 이후 부분
	parts := strings.Split(d.Clazz, ".")
	return parts[len(parts)-1]
}

// GetID 는 이 Descriptor의 고유 식별자를 반환한다.
// Java의 getId() 메서드에 대응한다.
// 실제 코드 (Descriptor.java:348-350):
//
//	public String getId() { return clazz.getName(); }
func (d *Descriptor) GetID() string {
	return d.Clazz
}

// GetPropertyTypes 는 이 Descriptor가 설명하는 Describable의 필드 타입 정보를 반환한다.
func (d *Descriptor) GetPropertyTypes() []PropertyType {
	return d.propertyTypes
}

// NewInstance 는 JSON 데이터로부터 Describable 인스턴스를 생성한다.
// Java의 newInstance(StaplerRequest2 req, JSONObject formData)에 대응한다.
// Jenkins에서는 Stapler가 HTTP 폼 데이터를 JSONObject로 변환한 뒤
// 이 메서드를 호출하여 설정 가능한 객체를 생성한다.
func (d *Descriptor) NewInstance(jsonData map[string]interface{}) (Describable, error) {
	if d.newInstanceFn != nil {
		return d.newInstanceFn(jsonData)
	}
	return nil, fmt.Errorf("newInstance가 구현되지 않음: %s", d.Clazz)
}

// Configure 는 전역 설정을 업데이트한다.
// Java의 configure(StaplerRequest2 req, JSONObject json)에 대응한다.
// 실제 코드 (Descriptor.java:869):
//
//	public boolean configure(StaplerRequest2 req, JSONObject json) throws FormException
//
// 반환값: false면 클라이언트가 같은 설정 페이지에 머물러야 한다.
func (d *Descriptor) Configure(jsonData map[string]interface{}) bool {
	if d.globalConfig == nil {
		d.globalConfig = make(map[string]interface{})
	}
	if d.configureFn != nil {
		return d.configureFn(d, jsonData)
	}
	// 기본 구현: JSON 데이터를 전역 설정에 병합
	for k, v := range jsonData {
		d.globalConfig[k] = v
	}
	return true
}

// GetGlobalConfig 는 현재 전역 설정을 반환한다.
func (d *Descriptor) GetGlobalConfig() map[string]interface{} {
	if d.globalConfig == nil {
		return map[string]interface{}{}
	}
	return d.globalConfig
}

// =============================================================================
// 4. DescriptorExtensionList: Descriptor 컬렉션
// =============================================================================
// Jenkins의 hudson.DescriptorExtensionList에 대응한다.
// 실제 코드 (DescriptorExtensionList.java:65):
//   public class DescriptorExtensionList<T extends Describable<T>, D extends Descriptor<T>>
//       extends ExtensionList<D> {
//       private final Class<T> describableType;
//   }
//
// 특정 확장 포인트(예: Builder, SCM)에 대한 모든 Descriptor를 보유한다.
// Jenkins에서는 ExtensionList를 상속하여 Descriptor 전용 필터링 로직을 추가한다.
// _load() 메서드에서 d.getT() == describableType인 것만 필터링한다.

// DescriptorExtensionList 는 특정 확장 포인트에 대한 Descriptor 컬렉션이다.
type DescriptorExtensionList struct {
	// describableType 은 이 리스트가 관리하는 확장 포인트의 타입 ID이다.
	describableType string

	// descriptors 는 이 확장 포인트에 등록된 Descriptor 목록이다.
	descriptors []*Descriptor
}

// Find 는 주어진 타입에 대한 Descriptor를 찾는다. d.clazz == type인 것을 반환한다.
// Java의 find(Class<? extends T> type) 메서드에 대응한다.
// 실제 코드 (DescriptorExtensionList.java:123-128):
//
//	public D find(Class<? extends T> type) {
//	    for (D d : this) if (d.clazz == type) return d;
//	    return null;
//	}
func (del *DescriptorExtensionList) Find(typeID string) *Descriptor {
	for _, d := range del.descriptors {
		if d.Clazz == typeID {
			return d
		}
	}
	return nil
}

// FindByName 은 Descriptor.getId()로 Descriptor를 찾는다.
// Java의 findByName(String id) 메서드에 대응한다.
// 실제 코드 (DescriptorExtensionList.java:168-173):
//
//	public D findByName(String id) {
//	    for (D d : this) if (d.getId().equals(id)) return d;
//	    return null;
//	}
func (del *DescriptorExtensionList) FindByName(id string) *Descriptor {
	for _, d := range del.descriptors {
		if d.GetID() == id {
			return d
		}
	}
	return nil
}

// All 은 등록된 모든 Descriptor를 반환한다.
func (del *DescriptorExtensionList) All() []*Descriptor {
	return del.descriptors
}

// Add 는 Descriptor를 이 리스트에 추가한다.
func (del *DescriptorExtensionList) Add(d *Descriptor) {
	del.descriptors = append(del.descriptors, d)
}

// =============================================================================
// 5. DescriptorRegistry: Jenkins 싱글턴의 Descriptor 관리 부분
// =============================================================================
// Jenkins의 jenkins.model.Jenkins에서 Descriptor 관련 메서드를 시뮬레이션한다.
//
// 실제 Jenkins의 Descriptor 관리 구조:
//   - ExtensionList<Descriptor>: 전체 Descriptor 저장 (Jenkins.java:1542)
//   - Map<Class, DescriptorExtensionList>: 확장 포인트별 분류 (Jenkins.java:2845)
//   - getDescriptor(Class): d.clazz == type인 Descriptor 검색 (Jenkins.java:1542-1547)
//   - getDescriptorOrDie(Class): 없으면 AssertionError (Jenkins.java:1557-1562)
//   - getDescriptorList(Class): computeIfAbsent로 DescriptorExtensionList 조회 (Jenkins.java:2845)

// DescriptorRegistry 는 Jenkins 싱글턴의 Descriptor 레지스트리를 시뮬레이션한다.
type DescriptorRegistry struct {
	mu sync.RWMutex

	// allDescriptors 는 전체 Descriptor 목록이다.
	// Java의 ExtensionList<Descriptor>에 대응한다.
	allDescriptors []*Descriptor

	// descriptorLists 는 확장 포인트 타입별 DescriptorExtensionList이다.
	// Java의 descriptorLists: ConcurrentHashMap<Class, DescriptorExtensionList>에 대응한다.
	// 실제 코드 (Jenkins.java:2845-2847):
	//   public DescriptorExtensionList getDescriptorList(Class type) {
	//       return descriptorLists.computeIfAbsent(type, ...);
	//   }
	descriptorLists map[string]*DescriptorExtensionList
}

// 전역 레지스트리 싱글턴 — Jenkins.get()에 대응
var registry = &DescriptorRegistry{
	descriptorLists: make(map[string]*DescriptorExtensionList),
}

// Register 는 Descriptor를 레지스트리에 등록한다.
// Jenkins에서는 @Extension 어노테이션 + ExtensionFinder가 자동으로 수행한다.
func (r *DescriptorRegistry) Register(d *Descriptor) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.allDescriptors = append(r.allDescriptors, d)

	// 확장 포인트 타입별 리스트에도 추가
	del, ok := r.descriptorLists[d.ExtPoint]
	if !ok {
		del = &DescriptorExtensionList{describableType: d.ExtPoint}
		r.descriptorLists[d.ExtPoint] = del
	}
	del.Add(d)
}

// GetDescriptor 는 주어진 타입 ID에 대한 Descriptor를 반환한다.
// Java의 Jenkins.getDescriptor(Class) 메서드에 대응한다.
// 실제 코드 (Jenkins.java:1542-1547):
//
//	public Descriptor getDescriptor(Class<? extends Describable> type) {
//	    for (Descriptor d : getExtensionList(Descriptor.class))
//	        if (d.clazz == type) return d;
//	    return null;
//	}
func (r *DescriptorRegistry) GetDescriptor(typeID string) *Descriptor {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, d := range r.allDescriptors {
		if d.Clazz == typeID {
			return d
		}
	}
	return nil
}

// GetDescriptorOrDie 는 Descriptor를 찾지 못하면 패닉한다.
// Java의 Jenkins.getDescriptorOrDie(Class) 메서드에 대응한다.
// 실제 코드 (Jenkins.java:1557-1562):
//
//	public Descriptor getDescriptorOrDie(Class<? extends Describable> type) {
//	    Descriptor d = getDescriptor(type);
//	    if (d == null) throw new AssertionError(type + " is missing its descriptor");
//	    return d;
//	}
func (r *DescriptorRegistry) GetDescriptorOrDie(typeID string) *Descriptor {
	d := r.GetDescriptor(typeID)
	if d == nil {
		panic(fmt.Sprintf("%s is missing its descriptor", typeID))
	}
	return d
}

// GetDescriptorList 는 확장 포인트 타입에 대한 DescriptorExtensionList를 반환한다.
// Java의 Jenkins.getDescriptorList(Class) 메서드에 대응한다.
// 실제 코드 (Jenkins.java:2845-2847):
//
//	public DescriptorExtensionList getDescriptorList(Class type) {
//	    return descriptorLists.computeIfAbsent(type,
//	        key -> DescriptorExtensionList.createDescriptorList(this, key));
//	}
func (r *DescriptorRegistry) GetDescriptorList(extPointType string) *DescriptorExtensionList {
	r.mu.Lock()
	defer r.mu.Unlock()

	del, ok := r.descriptorLists[extPointType]
	if !ok {
		del = &DescriptorExtensionList{describableType: extPointType}
		r.descriptorLists[extPointType] = del
	}
	return del
}

// AllDescriptors 는 등록된 모든 Descriptor를 반환한다.
func (r *DescriptorRegistry) AllDescriptors() []*Descriptor {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]*Descriptor, len(r.allDescriptors))
	copy(result, r.allDescriptors)
	return result
}

// =============================================================================
// 6. 구체 Describable 구현: SCM (소스 코드 관리)
// =============================================================================
// Jenkins의 hudson.scm.SCM을 시뮬레이션한다.
// SCM은 ExtensionPoint이면서 Describable이다.
// 대표적인 구현: GitSCM, SubversionSCM 등.

// --- GitSCM ---

// GitSCM 은 Git 소스 코드 관리 설정을 표현하는 Describable이다.
type GitSCM struct {
	RepoURL  string   // 저장소 URL
	Branch   string   // 브랜치
	Branches []string // 다중 브랜치 (PropertyType.itemType 시연용)
}

func (g *GitSCM) GetDescriptor() *Descriptor {
	return registry.GetDescriptorOrDie("hudson.plugins.git.GitSCM")
}

func (g *GitSCM) TypeID() string          { return "hudson.plugins.git.GitSCM" }
func (g *GitSCM) ExtensionPointType() string { return "hudson.scm.SCM" }

func (g *GitSCM) String() string {
	return fmt.Sprintf("GitSCM{url=%s, branch=%s, branches=%v}", g.RepoURL, g.Branch, g.Branches)
}

// --- SubversionSCM ---

// SubversionSCM 은 Subversion 소스 코드 관리 설정을 표현하는 Describable이다.
type SubversionSCM struct {
	SvnURL   string
	Revision string
}

func (s *SubversionSCM) GetDescriptor() *Descriptor {
	return registry.GetDescriptorOrDie("hudson.scm.SubversionSCM")
}

func (s *SubversionSCM) TypeID() string          { return "hudson.scm.SubversionSCM" }
func (s *SubversionSCM) ExtensionPointType() string { return "hudson.scm.SCM" }

func (s *SubversionSCM) String() string {
	return fmt.Sprintf("SubversionSCM{url=%s, revision=%s}", s.SvnURL, s.Revision)
}

// =============================================================================
// 7. 구체 Describable 구현: Builder (빌드 스텝)
// =============================================================================
// Jenkins의 hudson.tasks.Builder를 시뮬레이션한다.
// Builder는 ExtensionPoint이면서 Describable이다.
// 대표적인 구현: Shell, BatchFile, Maven 등.

// --- ShellBuilder ---

// ShellBuilder 는 셸 명령어를 실행하는 빌드 스텝이다.
// Jenkins의 hudson.tasks.Shell에 대응한다.
type ShellBuilder struct {
	Command    string
	Unstable   bool // 종료코드가 0이 아니면 UNSTABLE로 처리할지 여부
}

func (s *ShellBuilder) GetDescriptor() *Descriptor {
	return registry.GetDescriptorOrDie("hudson.tasks.Shell")
}

func (s *ShellBuilder) TypeID() string          { return "hudson.tasks.Shell" }
func (s *ShellBuilder) ExtensionPointType() string { return "hudson.tasks.Builder" }

func (s *ShellBuilder) String() string {
	return fmt.Sprintf("Shell{command=%q, unstable=%v}", s.Command, s.Unstable)
}

// --- MavenBuilder ---

// MavenBuilder 는 Maven 빌드 스텝이다.
type MavenBuilder struct {
	Targets    string
	PomFile    string
	Properties map[string]string
}

func (m *MavenBuilder) GetDescriptor() *Descriptor {
	return registry.GetDescriptorOrDie("hudson.tasks.Maven")
}

func (m *MavenBuilder) TypeID() string          { return "hudson.tasks.Maven" }
func (m *MavenBuilder) ExtensionPointType() string { return "hudson.tasks.Builder" }

func (m *MavenBuilder) String() string {
	return fmt.Sprintf("Maven{targets=%q, pom=%s}", m.Targets, m.PomFile)
}

// =============================================================================
// 8. 구체 Describable 구현: Publisher (빌드 후 작업)
// =============================================================================

// --- EmailPublisher ---

// EmailPublisher 는 빌드 결과를 이메일로 알리는 Publisher이다.
type EmailPublisher struct {
	Recipients string
	SendOnFail bool
}

func (e *EmailPublisher) GetDescriptor() *Descriptor {
	return registry.GetDescriptorOrDie("hudson.tasks.Mailer")
}

func (e *EmailPublisher) TypeID() string          { return "hudson.tasks.Mailer" }
func (e *EmailPublisher) ExtensionPointType() string { return "hudson.tasks.Publisher" }

func (e *EmailPublisher) String() string {
	return fmt.Sprintf("Mailer{recipients=%q, sendOnFail=%v}", e.Recipients, e.SendOnFail)
}

// =============================================================================
// 9. Descriptor 등록: 각 Describable 타입에 대한 싱글턴 Descriptor 생성
// =============================================================================
// Jenkins에서는 @Extension 어노테이션이 붙은 DescriptorImpl 내부 클래스를
// ExtensionFinder가 자동으로 발견하여 등록한다.
// 예: public class GitSCM extends SCM {
//         @Extension public static class DescriptorImpl extends SCMDescriptor<GitSCM> {
//             public String getDisplayName() { return "Git"; }
//         }
//     }

func init() {
	// --- SCM Descriptors ---

	registry.Register(&Descriptor{
		Clazz:       "hudson.plugins.git.GitSCM",
		ExtPoint:    "hudson.scm.SCM",
		displayName: "Git",
		propertyTypes: []PropertyType{
			{Name: "repoURL", TypeName: "string", DisplayName: "Repository URL", Required: true},
			{Name: "branch", TypeName: "string", DisplayName: "Branch", Required: false},
			{Name: "branches", TypeName: "[]string", DisplayName: "Branches", Required: false, ItemType: "string"},
		},
		newInstanceFn: func(data map[string]interface{}) (Describable, error) {
			scm := &GitSCM{}
			if v, ok := data["repoURL"].(string); ok {
				scm.RepoURL = v
			} else {
				return nil, fmt.Errorf("repoURL은 필수 필드입니다")
			}
			if v, ok := data["branch"].(string); ok {
				scm.Branch = v
			} else {
				scm.Branch = "main"
			}
			if v, ok := data["branches"].([]interface{}); ok {
				for _, b := range v {
					if s, ok := b.(string); ok {
						scm.Branches = append(scm.Branches, s)
					}
				}
			}
			return scm, nil
		},
		configureFn: func(d *Descriptor, data map[string]interface{}) bool {
			// Git 전역 설정: 기본 브랜치, 글로벌 credential 등
			for k, v := range data {
				d.globalConfig[k] = v
			}
			return true
		},
	})

	registry.Register(&Descriptor{
		Clazz:       "hudson.scm.SubversionSCM",
		ExtPoint:    "hudson.scm.SCM",
		displayName: "Subversion",
		propertyTypes: []PropertyType{
			{Name: "svnURL", TypeName: "string", DisplayName: "Repository URL", Required: true},
			{Name: "revision", TypeName: "string", DisplayName: "Revision", Required: false},
		},
		newInstanceFn: func(data map[string]interface{}) (Describable, error) {
			scm := &SubversionSCM{}
			if v, ok := data["svnURL"].(string); ok {
				scm.SvnURL = v
			} else {
				return nil, fmt.Errorf("svnURL은 필수 필드입니다")
			}
			if v, ok := data["revision"].(string); ok {
				scm.Revision = v
			} else {
				scm.Revision = "HEAD"
			}
			return scm, nil
		},
	})

	// --- Builder Descriptors ---

	registry.Register(&Descriptor{
		Clazz:       "hudson.tasks.Shell",
		ExtPoint:    "hudson.tasks.Builder",
		displayName: "Execute shell",
		propertyTypes: []PropertyType{
			{Name: "command", TypeName: "string", DisplayName: "Command", Required: true},
			{Name: "unstableReturn", TypeName: "bool", DisplayName: "Unstable return", Required: false},
		},
		newInstanceFn: func(data map[string]interface{}) (Describable, error) {
			b := &ShellBuilder{}
			if v, ok := data["command"].(string); ok {
				b.Command = v
			} else {
				return nil, fmt.Errorf("command는 필수 필드입니다")
			}
			if v, ok := data["unstableReturn"].(bool); ok {
				b.Unstable = v
			}
			return b, nil
		},
	})

	registry.Register(&Descriptor{
		Clazz:       "hudson.tasks.Maven",
		ExtPoint:    "hudson.tasks.Builder",
		displayName: "Invoke top-level Maven targets",
		propertyTypes: []PropertyType{
			{Name: "targets", TypeName: "string", DisplayName: "Goals", Required: true},
			{Name: "pomFile", TypeName: "string", DisplayName: "POM file", Required: false},
			{Name: "properties", TypeName: "map[string]string", DisplayName: "Properties", Required: false},
		},
		newInstanceFn: func(data map[string]interface{}) (Describable, error) {
			b := &MavenBuilder{}
			if v, ok := data["targets"].(string); ok {
				b.Targets = v
			} else {
				return nil, fmt.Errorf("targets는 필수 필드입니다")
			}
			if v, ok := data["pomFile"].(string); ok {
				b.PomFile = v
			} else {
				b.PomFile = "pom.xml"
			}
			return b, nil
		},
		configureFn: func(d *Descriptor, data map[string]interface{}) bool {
			// Maven 전역 설정: MAVEN_HOME 등
			for k, v := range data {
				d.globalConfig[k] = v
			}
			return true
		},
	})

	// --- Publisher Descriptors ---

	registry.Register(&Descriptor{
		Clazz:       "hudson.tasks.Mailer",
		ExtPoint:    "hudson.tasks.Publisher",
		displayName: "E-mail Notification",
		propertyTypes: []PropertyType{
			{Name: "recipients", TypeName: "string", DisplayName: "Recipients", Required: true},
			{Name: "sendOnFail", TypeName: "bool", DisplayName: "Send on failure only", Required: false},
		},
		newInstanceFn: func(data map[string]interface{}) (Describable, error) {
			p := &EmailPublisher{}
			if v, ok := data["recipients"].(string); ok {
				p.Recipients = v
			} else {
				return nil, fmt.Errorf("recipients는 필수 필드입니다")
			}
			if v, ok := data["sendOnFail"].(bool); ok {
				p.SendOnFail = v
			}
			return p, nil
		},
		configureFn: func(d *Descriptor, data map[string]interface{}) bool {
			// Mailer 전역 설정: SMTP 서버, 기본 수신자 접미사 등
			for k, v := range data {
				d.globalConfig[k] = v
			}
			return true
		},
	})
}

// =============================================================================
// 유틸리티 함수
// =============================================================================

func printSeparator(title string) {
	fmt.Println()
	fmt.Println(strings.Repeat("=", 76))
	fmt.Printf("  %s\n", title)
	fmt.Println(strings.Repeat("=", 76))
	fmt.Println()
}

func printSubSection(title string) {
	fmt.Printf("--- %s ---\n", title)
}

// =============================================================================
// 메인: 데모 시나리오
// =============================================================================

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════════════════╗")
	fmt.Println("║  Jenkins Descriptor/Describable 패턴 시뮬레이션                            ║")
	fmt.Println("║  Object/Class 관계의 확장: 인스턴스(Describable) ↔ 메타데이터(Descriptor)    ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════════════════╝")

	// =========================================================================
	// 시나리오 1: Object/Class 관계와의 유사성 시각화
	// =========================================================================
	printSeparator("시나리오 1: Object/Class vs Describable/Descriptor 유사성")

	fmt.Println("Java의 Object/Class 관계:")
	fmt.Println()
	fmt.Println("    Object instance          Class metaobject")
	fmt.Println("    ┌──────────────┐         ┌──────────────────┐")
	fmt.Println("    │ new String() │──.getClass()──>│ String.class     │")
	fmt.Println("    │ \"hello\"      │         │ getName()        │")
	fmt.Println("    │              │         │ getFields()      │")
	fmt.Println("    │              │         │ newInstance()     │")
	fmt.Println("    └──────────────┘         └──────────────────┘")
	fmt.Println("     (N개 존재)                (타입당 1개: 싱글턴)")
	fmt.Println()
	fmt.Println("Jenkins의 Describable/Descriptor 관계:")
	fmt.Println()
	fmt.Println("    Describable instance     Descriptor singleton")
	fmt.Println("    ┌──────────────┐         ┌───────────────────────┐")
	fmt.Println("    │ GitSCM       │─getDescriptor()─>│ GitSCM.DescriptorImpl │")
	fmt.Println("    │ url=...      │         │ getDisplayName()      │")
	fmt.Println("    │ branch=main  │         │ getPropertyTypes()    │")
	fmt.Println("    │              │         │ newInstance(req,json)  │")
	fmt.Println("    │              │         │ configure(req,json)   │")
	fmt.Println("    └──────────────┘         └───────────────────────┘")
	fmt.Println("     (N개 존재)                (타입당 1개: 싱글턴)")
	fmt.Println()
	fmt.Println("핵심 차이:")
	fmt.Println("  - Class는 JVM이 자동 제공 / Descriptor는 개발자가 명시적으로 구현")
	fmt.Println("  - Descriptor는 폼 바인딩, 전역 설정, 검증 등 UI 통합 기능 포함")
	fmt.Println("  - Descriptor는 ExtensionPoint → 플러그인이 새 타입 추가 가능")

	// =========================================================================
	// 시나리오 2: 레지스트리에서 Descriptor 조회
	// =========================================================================
	printSeparator("시나리오 2: DescriptorRegistry — 전역 조회")

	printSubSection("전체 등록된 Descriptor 목록")
	for i, d := range registry.AllDescriptors() {
		fmt.Printf("  [%d] id=%-35s displayName=%q extPoint=%s\n",
			i, d.GetID(), d.GetDisplayName(), d.ExtPoint)
	}

	fmt.Println()
	printSubSection("Jenkins.getDescriptor(Class) — 타입 ID로 단건 조회")
	gitDesc := registry.GetDescriptor("hudson.plugins.git.GitSCM")
	if gitDesc != nil {
		fmt.Printf("  getDescriptor('hudson.plugins.git.GitSCM') → displayName=%q\n", gitDesc.GetDisplayName())
	}

	fmt.Println()
	printSubSection("Jenkins.getDescriptorOrDie(Class) — 없으면 패닉")
	fmt.Println("  getDescriptorOrDie('hudson.tasks.Shell') → (정상)")
	_ = registry.GetDescriptorOrDie("hudson.tasks.Shell")
	fmt.Println("  getDescriptorOrDie('nonexistent.Type') → (패닉 복구 시연)")
	func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Printf("  [PANIC 복구] %v\n", r)
			}
		}()
		registry.GetDescriptorOrDie("nonexistent.Type")
	}()

	// =========================================================================
	// 시나리오 3: DescriptorExtensionList — 확장 포인트별 조회
	// =========================================================================
	printSeparator("시나리오 3: DescriptorExtensionList — 확장 포인트별 필터링")

	extPoints := []string{"hudson.scm.SCM", "hudson.tasks.Builder", "hudson.tasks.Publisher"}
	for _, ep := range extPoints {
		del := registry.GetDescriptorList(ep)
		printSubSection(fmt.Sprintf("getDescriptorList('%s')", ep))
		for _, d := range del.All() {
			fmt.Printf("  - %s (%q)\n", d.GetID(), d.GetDisplayName())
		}
		fmt.Println()
	}

	fmt.Println("이것이 Jenkins 설정 UI에서 드롭다운/라디오 버튼에 선택지가 표시되는 원리이다.")
	fmt.Println("예: 'SCM' 설정 영역에서 [Git] [Subversion]을 선택할 수 있는 이유:")
	fmt.Println("  → getDescriptorList(SCM.class)가 Git과 Subversion의 Descriptor를 반환하기 때문")

	// =========================================================================
	// 시나리오 4: PropertyType — 필드별 타입 정보
	// =========================================================================
	printSeparator("시나리오 4: PropertyType — Describable 필드 타입 정보")

	fmt.Println("Jenkins에서 PropertyType은 Jelly/Groovy 폼 렌더링에 사용된다.")
	fmt.Println("필드 타입에 따라 텍스트 입력, 체크박스, 드롭다운 등 적절한 위젯이 선택된다.")
	fmt.Println()

	for _, d := range registry.AllDescriptors() {
		printSubSection(fmt.Sprintf("%s의 PropertyTypes", d.GetDisplayName()))
		fmt.Printf("  %-15s %-10s %s\n", "필드명", "타입", "표시이름")
		fmt.Printf("  %s\n", strings.Repeat("-", 55))
		for _, pt := range d.GetPropertyTypes() {
			fmt.Printf("  %s\n", pt)
		}
		fmt.Println()
	}

	// =========================================================================
	// 시나리오 5: newInstance() — JSON → Describable 인스턴스 변환
	// =========================================================================
	printSeparator("시나리오 5: newInstance(req, JSONObject) — 폼 바인딩")

	fmt.Println("Jenkins에서 사용자가 설정 폼을 제출하면:")
	fmt.Println("  1. Stapler가 HTTP 폼 데이터를 JSONObject로 변환")
	fmt.Println("  2. Descriptor.newInstance(req, json)이 호출되어 Describable 인스턴스 생성")
	fmt.Println("  3. 생성된 인스턴스가 Job의 설정에 저장(XML 직렬화)")
	fmt.Println()

	// JSON → GitSCM 인스턴스
	gitJSON := map[string]interface{}{
		"repoURL":  "https://github.com/jenkinsci/jenkins.git",
		"branch":   "master",
		"branches": []interface{}{"main", "release/2.x", "feature/jep-123"},
	}
	printSubSection("GitSCM 인스턴스 생성")
	jsonBytes, _ := json.MarshalIndent(gitJSON, "  ", "  ")
	fmt.Printf("  입력 JSON:\n  %s\n", string(jsonBytes))

	gitInst, err := gitDesc.NewInstance(gitJSON)
	if err != nil {
		fmt.Printf("  오류: %v\n", err)
	} else {
		fmt.Printf("  생성된 인스턴스: %s\n", gitInst)
		// 인스턴스에서 다시 Descriptor를 얻을 수 있음 (getDescriptor() 순환)
		desc := gitInst.GetDescriptor()
		fmt.Printf("  인스턴스.getDescriptor().getDisplayName() = %q\n", desc.GetDisplayName())
		fmt.Printf("  인스턴스.getDescriptor() == 레지스트리.getDescriptor() ? %v (싱글턴 확인)\n",
			desc == gitDesc)
	}

	// JSON → ShellBuilder 인스턴스
	fmt.Println()
	shellJSON := map[string]interface{}{
		"command":        "mvn clean install -DskipTests",
		"unstableReturn": true,
	}
	shellDesc := registry.GetDescriptorOrDie("hudson.tasks.Shell")
	printSubSection("Shell 인스턴스 생성")
	jsonBytes, _ = json.MarshalIndent(shellJSON, "  ", "  ")
	fmt.Printf("  입력 JSON:\n  %s\n", string(jsonBytes))

	shellInst, err := shellDesc.NewInstance(shellJSON)
	if err != nil {
		fmt.Printf("  오류: %v\n", err)
	} else {
		fmt.Printf("  생성된 인스턴스: %s\n", shellInst)
	}

	// JSON → Maven 인스턴스
	fmt.Println()
	mavenJSON := map[string]interface{}{
		"targets": "clean package",
		"pomFile": "backend/pom.xml",
	}
	mavenDesc := registry.GetDescriptorOrDie("hudson.tasks.Maven")
	printSubSection("Maven 인스턴스 생성")
	jsonBytes, _ = json.MarshalIndent(mavenJSON, "  ", "  ")
	fmt.Printf("  입력 JSON:\n  %s\n", string(jsonBytes))

	mavenInst, err := mavenDesc.NewInstance(mavenJSON)
	if err != nil {
		fmt.Printf("  오류: %v\n", err)
	} else {
		fmt.Printf("  생성된 인스턴스: %s\n", mavenInst)
	}

	// JSON → Email Publisher 인스턴스
	fmt.Println()
	emailJSON := map[string]interface{}{
		"recipients": "dev-team@company.com, qa-team@company.com",
		"sendOnFail": true,
	}
	emailDesc := registry.GetDescriptorOrDie("hudson.tasks.Mailer")
	printSubSection("Mailer 인스턴스 생성")
	jsonBytes, _ = json.MarshalIndent(emailJSON, "  ", "  ")
	fmt.Printf("  입력 JSON:\n  %s\n", string(jsonBytes))

	emailInst, err := emailDesc.NewInstance(emailJSON)
	if err != nil {
		fmt.Printf("  오류: %v\n", err)
	} else {
		fmt.Printf("  생성된 인스턴스: %s\n", emailInst)
	}

	// 필수 필드 누락 시 오류
	fmt.Println()
	printSubSection("필수 필드 누락 시 오류 처리")
	badJSON := map[string]interface{}{
		"branch": "develop", // repoURL 누락
	}
	_, err = gitDesc.NewInstance(badJSON)
	if err != nil {
		fmt.Printf("  [오류] %v\n", err)
	}

	// =========================================================================
	// 시나리오 6: configure() — 전역 설정 변경
	// =========================================================================
	printSeparator("시나리오 6: configure(req, JSONObject) — 전역 설정 관리")

	fmt.Println("Descriptor는 인스턴스별 설정 외에 시스템 전역 설정도 관리한다.")
	fmt.Println("예: Git 플러그인의 전역 설정 = 기본 사용자명, 이메일, 전역 Credential ID")
	fmt.Println("    Maven 전역 설정 = MAVEN_HOME 경로")
	fmt.Println("    Mailer 전역 설정 = SMTP 서버 주소, 기본 수신자 접미사")
	fmt.Println()

	// Git 전역 설정
	printSubSection("Git 전역 설정")
	gitGlobalConfig := map[string]interface{}{
		"globalConfigName":       "Jenkins CI",
		"globalConfigEmail":      "jenkins@company.com",
		"defaultCredentialsId":   "github-token",
		"createAccountBasedOnEmail": true,
	}
	jsonBytes, _ = json.MarshalIndent(gitGlobalConfig, "  ", "  ")
	fmt.Printf("  설정 JSON:\n  %s\n", string(jsonBytes))
	ok := gitDesc.Configure(gitGlobalConfig)
	fmt.Printf("  configure() 결과: %v\n", ok)
	fmt.Printf("  저장된 전역 설정: %v\n", gitDesc.GetGlobalConfig())

	// Maven 전역 설정
	fmt.Println()
	printSubSection("Maven 전역 설정")
	mavenGlobalConfig := map[string]interface{}{
		"mavenHome":    "/usr/local/maven",
		"globalOpts":   "-Xmx1024m",
		"localRepo":    "/var/jenkins_home/.m2/repository",
	}
	jsonBytes, _ = json.MarshalIndent(mavenGlobalConfig, "  ", "  ")
	fmt.Printf("  설정 JSON:\n  %s\n", string(jsonBytes))
	ok = mavenDesc.Configure(mavenGlobalConfig)
	fmt.Printf("  configure() 결과: %v\n", ok)
	fmt.Printf("  저장된 전역 설정: %v\n", mavenDesc.GetGlobalConfig())

	// Mailer 전역 설정
	fmt.Println()
	printSubSection("Mailer 전역 설정")
	mailerGlobalConfig := map[string]interface{}{
		"smtpHost":          "smtp.company.com",
		"smtpPort":          587,
		"defaultSuffix":     "@company.com",
		"useSsl":            true,
		"charset":           "UTF-8",
	}
	jsonBytes, _ = json.MarshalIndent(mailerGlobalConfig, "  ", "  ")
	fmt.Printf("  설정 JSON:\n  %s\n", string(jsonBytes))
	ok = emailDesc.Configure(mailerGlobalConfig)
	fmt.Printf("  configure() 결과: %v\n", ok)
	fmt.Printf("  저장된 전역 설정: %v\n", emailDesc.GetGlobalConfig())

	// =========================================================================
	// 시나리오 7: 완전한 파이프라인 설정 시뮬레이션
	// =========================================================================
	printSeparator("시나리오 7: 완전한 파이프라인 설정 — SCM + Builders + Publishers")

	fmt.Println("Jenkins Job 설정 시나리오: 사용자가 웹 UI에서 폼을 작성하고 저장한다.")
	fmt.Println()

	// 1) SCM 선택 — 드롭다운에서 선택한 타입의 Descriptor를 통해 인스턴스 생성
	fmt.Println("[1단계] SCM 선택")
	scmList := registry.GetDescriptorList("hudson.scm.SCM")
	fmt.Println("  사용 가능한 SCM 플러그인:")
	for i, d := range scmList.All() {
		fmt.Printf("    (%d) %s\n", i, d.GetDisplayName())
	}
	fmt.Println("  → 사용자가 'Git'을 선택")
	selectedSCMDesc := scmList.Find("hudson.plugins.git.GitSCM")
	scmInstance, _ := selectedSCMDesc.NewInstance(map[string]interface{}{
		"repoURL": "https://github.com/myorg/myapp.git",
		"branch":  "develop",
	})
	fmt.Printf("  → 생성된 SCM: %s\n", scmInstance)

	// 2) Builder 추가
	fmt.Println()
	fmt.Println("[2단계] Build Steps 추가")
	builderList := registry.GetDescriptorList("hudson.tasks.Builder")
	fmt.Println("  사용 가능한 빌드 스텝:")
	for i, d := range builderList.All() {
		fmt.Printf("    (%d) %s\n", i, d.GetDisplayName())
	}
	fmt.Println("  → Shell 스텝 추가")
	shellBuildDesc := builderList.Find("hudson.tasks.Shell")
	builder1, _ := shellBuildDesc.NewInstance(map[string]interface{}{
		"command": "echo 'Building...' && make build",
	})
	fmt.Printf("  → 생성된 Builder #1: %s\n", builder1)

	fmt.Println("  → Maven 스텝 추가")
	mavenBuildDesc := builderList.Find("hudson.tasks.Maven")
	builder2, _ := mavenBuildDesc.NewInstance(map[string]interface{}{
		"targets": "clean verify",
		"pomFile": "pom.xml",
	})
	fmt.Printf("  → 생성된 Builder #2: %s\n", builder2)

	// 3) Publisher 추가
	fmt.Println()
	fmt.Println("[3단계] Post-build Actions 추가")
	pubList := registry.GetDescriptorList("hudson.tasks.Publisher")
	fmt.Println("  사용 가능한 빌드 후 작업:")
	for i, d := range pubList.All() {
		fmt.Printf("    (%d) %s\n", i, d.GetDisplayName())
	}
	fmt.Println("  → E-mail Notification 추가")
	emailPubDesc := pubList.Find("hudson.tasks.Mailer")
	publisher1, _ := emailPubDesc.NewInstance(map[string]interface{}{
		"recipients": "dev-lead@company.com",
		"sendOnFail": true,
	})
	fmt.Printf("  → 생성된 Publisher: %s\n", publisher1)

	// 4) 최종 Job 설정 요약
	fmt.Println()
	fmt.Println("[최종 Job 설정 요약]")
	fmt.Println("  ┌─────────────────────────────────────────────────────────────┐")
	fmt.Printf("  │ SCM:       %s\n", scmInstance)
	fmt.Printf("  │ Builder#1: %s\n", builder1)
	fmt.Printf("  │ Builder#2: %s\n", builder2)
	fmt.Printf("  │ Publisher: %s\n", publisher1)
	fmt.Println("  └─────────────────────────────────────────────────────────────┘")

	// =========================================================================
	// 시나리오 8: DescriptorExtensionList.findByName() — ID 기반 조회
	// =========================================================================
	printSeparator("시나리오 8: findByName() — Descriptor ID로 조회")

	fmt.Println("Jenkins에서는 XML 직렬화 시 Descriptor ID를 사용하여 타입을 식별한다.")
	fmt.Println("deserialization 시 findByName(id)로 Descriptor를 복원한다.")
	fmt.Println()

	ids := []string{
		"hudson.plugins.git.GitSCM",
		"hudson.tasks.Shell",
		"hudson.tasks.Maven",
		"hudson.tasks.Mailer",
		"unknown.Descriptor",
	}
	for _, id := range ids {
		// 전체 Descriptor에서 검색
		d := registry.GetDescriptor(id)
		if d != nil {
			fmt.Printf("  findByName(%q) → %q (extPoint=%s)\n", id, d.GetDisplayName(), d.ExtPoint)
		} else {
			fmt.Printf("  findByName(%q) → null (미등록)\n", id)
		}
	}

	// =========================================================================
	// 시나리오 9: Describable.getDescriptor() 싱글턴 검증
	// =========================================================================
	printSeparator("시나리오 9: Describable.getDescriptor() 싱글턴 보장")

	fmt.Println("같은 타입의 서로 다른 인스턴스는 동일한 Descriptor 싱글턴을 공유한다.")
	fmt.Println("Java에서: if (a.getClass() == b.getClass()) then a.getDescriptor() == b.getDescriptor()")
	fmt.Println()

	// 두 개의 서로 다른 GitSCM 인스턴스 생성
	git1, _ := gitDesc.NewInstance(map[string]interface{}{
		"repoURL": "https://github.com/repo-A.git",
		"branch":  "main",
	})
	git2, _ := gitDesc.NewInstance(map[string]interface{}{
		"repoURL": "https://github.com/repo-B.git",
		"branch":  "develop",
	})

	desc1 := git1.GetDescriptor()
	desc2 := git2.GetDescriptor()

	fmt.Printf("  git1 = %s\n", git1)
	fmt.Printf("  git2 = %s\n", git2)
	fmt.Printf("  git1 == git2 ? %v (서로 다른 인스턴스)\n", git1 == git2)
	fmt.Printf("  git1.getDescriptor() == git2.getDescriptor() ? %v (동일한 Descriptor 싱글턴)\n",
		desc1 == desc2)
	fmt.Printf("  Descriptor 포인터: git1=%p, git2=%p\n", desc1, desc2)

	// =========================================================================
	// 시나리오 10: 패턴 요약
	// =========================================================================
	printSeparator("패턴 요약: Descriptor/Describable 아키텍처")

	fmt.Println("Jenkins Descriptor/Describable 패턴의 전체 구조:")
	fmt.Println()
	fmt.Println("  Jenkins (싱글턴)")
	fmt.Println("  ├── ExtensionList<Descriptor>          ← 전체 Descriptor 저장소")
	fmt.Println("  │   ├── GitSCM.DescriptorImpl")
	fmt.Println("  │   ├── SubversionSCM.DescriptorImpl")
	fmt.Println("  │   ├── Shell.DescriptorImpl")
	fmt.Println("  │   ├── Maven.DescriptorImpl")
	fmt.Println("  │   └── Mailer.DescriptorImpl")
	fmt.Println("  │")
	fmt.Println("  └── Map<Class, DescriptorExtensionList> ← 확장 포인트별 분류")
	fmt.Println("      ├── SCM → [GitSCM.Desc, SubversionSCM.Desc]")
	fmt.Println("      ├── Builder → [Shell.Desc, Maven.Desc]")
	fmt.Println("      └── Publisher → [Mailer.Desc]")
	fmt.Println()
	fmt.Println("  각 Descriptor의 역할:")
	fmt.Println("  ┌─────────────────────────────────────────────────────────────────────┐")
	fmt.Println("  │ 1. 메타데이터  │ getDisplayName(), getId(), getPropertyTypes()       │")
	fmt.Println("  │ 2. 팩토리     │ newInstance(req, json) → Describable 인스턴스 생성    │")
	fmt.Println("  │ 3. 전역 설정  │ configure(req, json), save(), load()                │")
	fmt.Println("  │ 4. 폼 검증   │ doCheckXxx() → FormValidation                       │")
	fmt.Println("  │ 5. 뷰 제공   │ config.jelly, global.jelly, help-xxx.html            │")
	fmt.Println("  └─────────────────────────────────────────────────────────────────────┘")
	fmt.Println()
	fmt.Println("  왜 이 패턴인가?")
	fmt.Println("  - 관심사 분리: 인스턴스 데이터 vs 타입 메타데이터")
	fmt.Println("  - 플러그인 확장: @Extension 하나로 새 타입 + UI + 검증 등록")
	fmt.Println("  - 직렬화 효율: Describable만 XML 직렬화, Descriptor는 transient")
	fmt.Println("  - 타입 안전성: 제네릭으로 Descriptor<Builder>가 Builder만 생성 보장")
}
